// Nydus http proxy backend only can ask for byte ranges of blobs, but the cluster stores chunks by digest
// => This proxy sits on every cluster node and translates a given byte range to chunk digests
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"nydus-cluster-cache/pkg/blobindex"
	"nydus-cluster-cache/pkg/nydus"
)

// Resolves a blob id and byte range to the specific chunk digests and corresponding registry repo
type Resolver interface {
	Resolve(blobID string, start, end uint64) ([]blobindex.Chunk, bool)
	BlobRepo(blobID string) (string, bool)
}

// Allows reading/writing chunks by their corresponding digest to the cluster cache
type Cache interface {
	Get(ctx context.Context, digest string) (io.ReadCloser, bool, error)
	Put(ctx context.Context, digest string, body io.Reader) error
}

// Allows to fetch blob byte ranges from a remote registry in case of cache miss
type Registry interface {
	GetBlobRange(ctx context.Context, repo, blob string, start, end uint64) (io.ReadCloser, error)
	HeadBlob(ctx context.Context, repo, blob string) (uint64, error)
}

// Settings for the cache proxy
type Options struct {
	CacheTimeout     time.Duration // Max. waiting time for a cache read before going to the remote registry instead
	PopulateWorkers  int           // amount of workers that upload freshly received chunks to the cache, default: 8
	PopulateQueue    int           // amount of uploads that are allowed to wait for a worker before new ones are dropped
	BreakerThreshold int           // amount of failed cache requests before we wait for breakerCooldown
	BreakerCooldown  time.Duration // timeout (s) until we start sending requests to a previously failed cache again

	// Whether we ignore the cluster cache path and send requests directly to the remote registry
	CacheDisabled bool

	// max. amount of chunks that we fetch in parallel within one request
	FetchConcurrency int

	// fct to re-read the blob metadata files to build the blob index
	Refresh func(context.Context) error

	// Counters this proxy reports, an unregistered set is used when left empty
	Metrics *Metrics

	// Ask the registry at the same time as the cache instead of after a miss
	ParallelMissFetch bool
}

// Set the default settings of this proxy
func (proxyOptions Options) withDefaults() Options {
	if proxyOptions.CacheTimeout == 0 {
		proxyOptions.CacheTimeout = 50 * time.Millisecond
	}
	if proxyOptions.PopulateWorkers == 0 {
		proxyOptions.PopulateWorkers = 8
	}
	if proxyOptions.PopulateQueue == 0 {
		proxyOptions.PopulateQueue = 1024
	}
	if proxyOptions.BreakerThreshold == 0 {
		proxyOptions.BreakerThreshold = 5
	}
	if proxyOptions.BreakerCooldown == 0 {
		proxyOptions.BreakerCooldown = 2 * time.Second
	}
	if proxyOptions.FetchConcurrency == 0 {
		proxyOptions.FetchConcurrency = 16
	}
	if proxyOptions.Metrics == nil {
		proxyOptions.Metrics = NewMetrics(nil)
	}
	return proxyOptions
}

// Serves the http requests from nydusd
type Handler struct {
	// Components
	resolver Resolver
	cache    Cache
	registry Registry
	opts     Options

	// Whether skipping the cache and go to the remote registry immediately or not
	cacheEnabled bool

	loader    singleflight.Group // Merge multiple concurrent requests for the same chunk into one
	refresher singleflight.Group // ... concurrent blob index re-scans requests into one
	breaker   *breaker           // failure breaker in case of multiple subsequent failed cache requests
	pool      *populatePool      // Pool of workers and queue to upload recently fetched chunks from remote registry to the cache
	mux       *http.ServeMux     // request router
}

func NewHandler(resolver Resolver, cache Cache, registry Registry, opts Options) *Handler {
	opts = opts.withDefaults()
	newHandler := &Handler{
		resolver:     resolver,
		cache:        cache,
		registry:     registry,
		opts:         opts,
		cacheEnabled: !opts.CacheDisabled,
		breaker:      newBreaker(opts.BreakerThreshold, opts.BreakerCooldown),
	}

	newHandler.pool = newPopulatePool(opts.PopulateWorkers, opts.PopulateQueue, newHandler.populate)
	newHandler.mux = http.NewServeMux()
	newHandler.mux.HandleFunc("/", newHandler.serve)

	return newHandler
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

// Stops the populate pool (handler cant be used afterwards)
func (handler *Handler) Close() { handler.pool.stop() }

// Starting point for every nydusd request:
// 1) Checks if we know the repo of the blob, if not re-scan blob index
// 2) Get blob size from origin repo & then check cache before going to the remote registry for chunk
func (handler *Handler) serve(writer http.ResponseWriter, request *http.Request) {
	// Check if we know the blob repo via blob index
	blob := strings.TrimPrefix(request.URL.Path, "/")
	targetRegistryRepo, ok := handler.resolver.BlobRepo(blob)
	// Not known yet => re-scan blob index and try again
	if !ok {
		targetRegistryRepo, ok = handler.retryAfterRefresh(request.Context(), blob)
	}
	// Still not known after retry => stop attempting
	if !ok {
		handler.fail(writer, request, http.StatusBadGateway, "unknown blob")
		return
	}

	switch request.Method {
	case http.MethodHead:
		handler.serveHead(writer, request, targetRegistryRepo, blob)
	case http.MethodGet:
		handler.serveGet(writer, request, targetRegistryRepo, blob)
	default:
		handler.fail(writer, request, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// re-reads the blob index and looks the blob up once more.
func (handler *Handler) retryAfterRefresh(ctx context.Context, blob string) (string, bool) {
	// Check if the refresh function is actually defined
	if handler.opts.Refresh == nil {
		return "", false
	}

	// nydusd issues many reads at once when a fresh image mounts, so concurrent misses turn into a single index refresh
	if _, err, _ := handler.refresher.Do("refresh", func() (any, error) {
		return nil, handler.opts.Refresh(ctx)
	}); err != nil {
		return "", false
	}
	return handler.resolver.BlobRepo(blob)
}

// Debugging function that sends an error and logs the corresponding context for easier debugging
func (handler *Handler) fail(writer http.ResponseWriter, request *http.Request, status int, message string) {
	log.Printf("%s %s range=%q -> %d: %s", request.Method, request.URL.Path, request.Header.Get("Range"), status, message)
	http.Error(writer, message, status)
}

// Sends a head-request to the remote registry to get the blob size (in bytes)
func (handler *Handler) serveHead(writer http.ResponseWriter, request *http.Request, repo, blob string) {
	blobSizeBytes, err := handler.registry.HeadBlob(request.Context(), repo, blob)
	if err != nil {
		handler.fail(writer, request, http.StatusBadGateway, err.Error())
		return
	}

	writer.Header().Set("Content-Length", strconv.FormatUint(blobSizeBytes, 10))
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.WriteHeader(http.StatusOK)
}

// Sends a get request to the cluster cache after receiving a nydusd blob range request
func (handler *Handler) serveGet(writer http.ResponseWriter, request *http.Request, repo, blob string) {
	// Parse range from the request
	start, end, err := parseRange(request.Header.Get("Range"))
	if err != nil {
		handler.fail(writer, request, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve the range to the specific chunks
	chunks, aligned := handler.resolver.Resolve(blob, start, end)
	// If the range doesnt start and end cleanly at chunk borders => request range from registry instead
	if !aligned {
		handler.passthrough(writer, request, repo, blob, start, end)
		return
	}

	// Fetch the chunks in the range in parallel
	payload, err := handler.fetchChunks(request.Context(), repo, blob, chunks)
	if err != nil {
		handler.fail(writer, request, http.StatusBadGateway, err.Error())
		return
	}

	// Respond with 206 and chunk bytes in payload
	writePartial(writer, start, end, payload)
}

// Fetch every chunk of a request (in parallel depending on proxy setting FetchConcurrency)
// => Keeps the chunks in order and the following chunks are still fetched (even though 502 resp is sent early)
func (handler *Handler) fetchChunks(ctx context.Context, repo, blob string, chunks []blobindex.Chunk) ([][]byte, error) {
	payload := make([][]byte, len(chunks))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(handler.opts.FetchConcurrency)

	// Attempt to fetch every requested chunk
	for iteration, currChunk := range chunks {
		group.Go(func() error {
			data, err := handler.fetchChunk(ctx, repo, blob, currChunk)
			if err != nil {
				return err
			}
			payload[iteration] = data
			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}
	return payload, nil
}

// Fetches a byte range directly from registry
// This may be needed if the byte range doesnt align cleanly with the chunk borders
func (handler *Handler) passthrough(writer http.ResponseWriter, request *http.Request, repo, blob string, start, end uint64) {
	body, err := handler.registry.GetBlobRange(request.Context(), repo, blob, start, end)
	if err != nil {
		handler.opts.Metrics.recordRegistryFailure(err)
		handler.fail(writer, request, http.StatusBadGateway, err.Error())
		return
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		handler.opts.Metrics.recordRegistryFailure(err)
		handler.fail(writer, request, http.StatusBadGateway, err.Error())
		return
	}
	writePartial(writer, start, end, [][]byte{data})
}

// Returns a single chunk while merging concurrent requests for the same digest
// This happens preferably from the cahce before falling back on the remote registry
// due to cache misses or failure
func (handler *Handler) fetchChunk(ctx context.Context, repo, blob string, chunk blobindex.Chunk) ([]byte, error) {
	// The loader is a singleflight group => merge concurrent requests
	result, err, _ := handler.loader.Do(chunk.Digest, func() (any, error) {

		// Add a 1 min limit for receiving the the chunk in a dedicated context
		// Sometimes nydus cancels slow responses and in that case there may be other requests
		// waiting for the result as well that still need it, regardless if the initial request got canceled
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()

		useCache := handler.cacheEnabled && !handler.breaker.open()

		// Ask the registry while the cache lookup is still running, so a miss costs no extra wait
		if useCache && handler.opts.ParallelMissFetch {
			return handler.fetchCacheAndRegistryTogether(fetchCtx, repo, blob, chunk)
		}

		// Attempt to fetch it from cache first
		if useCache {
			if data, ok := handler.tryCache(fetchCtx, chunk.Digest); ok {
				return data, nil
			}
		}

		// ...before going to the source registry
		data, err := handler.fetchFromRegistry(fetchCtx, repo, blob, chunk)
		if err != nil {
			return nil, err
		}

		// Cache the newly fetched chunk, unless the lookup above just found the cache down
		if handler.cacheEnabled && !handler.breaker.open() {
			handler.pool.submit(chunk.Digest, data)
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}

	// This arr is apparently shared with every concurrent caller for the same digest, so callers must treat it as read-only!
	return result.([]byte), nil
}

// Fetches a chunk from the cache and the registry at the same time, returning the first result that comes back
func (handler *Handler) fetchCacheAndRegistryTogether(ctx context.Context, repo, blob string, chunk blobindex.Chunk) ([]byte, error) {
	registryCtx, cancelRegistry := context.WithCancel(ctx)
	defer cancelRegistry()

	type registryAnswer struct {
		data []byte
		err  error
	}
	answer := make(chan registryAnswer, 1)
	go func() {
		data, err := handler.fetchFromRegistry(registryCtx, repo, blob, chunk)
		answer <- registryAnswer{data: data, err: err}
	}()

	// A hit makes the registry request unnecessary
	if data, ok := handler.tryCache(ctx, chunk.Digest); ok {
		return data, nil
	}

	fromRegistry := <-answer
	if fromRegistry.err != nil {
		return nil, fromRegistry.err
	}
	if !handler.breaker.open() {
		handler.pool.submit(chunk.Digest, fromRegistry.data)
	}
	return fromRegistry.data, nil
}

// Reads one chunk from the origin registry
func (handler *Handler) fetchFromRegistry(ctx context.Context, repo, blob string, chunk blobindex.Chunk) ([]byte, error) {
	body, err := handler.registry.GetBlobRange(ctx, repo, blob, chunk.Offset, chunk.Offset+chunk.Size-1)
	if err != nil {
		handler.opts.Metrics.recordRegistryFailure(err)
		return nil, fmt.Errorf("error while attempting to fetch from registry %s: %w", chunk.Digest, err)
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		handler.opts.Metrics.recordRegistryFailure(err)
		return nil, fmt.Errorf("error while reading from registry body %s: %w", chunk.Digest, err)
	}
	return data, nil
}

// Attempt to read a chunk from the cluster cache
func (handler *Handler) tryCache(ctx context.Context, digest string) ([]byte, bool) {
	startedAt := time.Now()
	data, outcome := handler.lookupCache(ctx, digest)
	handler.opts.Metrics.LookupSeconds.WithLabelValues(outcome).Observe(time.Since(startedAt).Seconds())

	return data, outcome == "hit"
}

// Asks the cluster cache for one chunk
func (handler *Handler) lookupCache(ctx context.Context, digest string) ([]byte, string) {
	// Own timeout so cache cant stall the read unnecessarily long
	reqCtx, cancel := context.WithTimeout(ctx, handler.opts.CacheTimeout)
	defer cancel()

	readCloser, hit, err := handler.cache.Get(reqCtx, digest)
	if err != nil {
		handler.breaker.recordFailure() // Record failure, so the breakerThreshold can be triggered at a certain amount
		return nil, "error"
	}

	// Cache miss => shows that the cache is healthy
	if !hit {
		handler.breaker.recordSuccess()
		return nil, "miss"
	}
	defer readCloser.Close()

	data, err := io.ReadAll(readCloser)
	if err != nil {
		handler.breaker.recordFailure()
		return nil, "error"
	}

	handler.breaker.recordSuccess()

	if !nydus.MatchesChunkDigest(data, digest) {
		handler.opts.Metrics.DigestMismatches.Inc()
		return nil, "mismatch"
	}
	return data, "hit"
}

// Upload a chunk to the cluster cache
func (handler *Handler) populate(digest string, data []byte) {
	// Dedicated context with 5s timeout for avoiding a worker being blocked by a slow upload
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = handler.cache.Put(ctx, digest, bytes.NewReader(data))
}

// Convert a range header field from a http request into (start,end)
// E.g. Range: bytes=100-199 => returns (100, 199)
func parseRange(header string) (uint64, uint64, error) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, fmt.Errorf("missing or malformed Range header in %q", header)
	}

	spec := strings.TrimPrefix(header, "bytes=")
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, fmt.Errorf("malformed range %q", spec)
	}

	start, err := strconv.ParseUint(strings.TrimSpace(spec[:dash]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range start %q: %w", spec, err)
	}

	end, err := strconv.ParseUint(strings.TrimSpace(spec[dash+1:]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range end %q: %w", spec, err)
	}
	return start, end, nil
}

// Creates a 206 response to a GET-range request with the requested bytes
func writePartial(w http.ResponseWriter, start, end uint64, byteParts [][]byte) {
	var totalBytes int
	for _, currPart := range byteParts {
		totalBytes += len(currPart)
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, end))
	w.Header().Set("Content-Length", strconv.Itoa(totalBytes))
	w.WriteHeader(http.StatusPartialContent)

	for _, currPart := range byteParts {
		_, _ = w.Write(currPart)
	}
}
