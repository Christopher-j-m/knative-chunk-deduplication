// pkg/proxy/proxy.go
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"nydus-cluster-cache/pkg/blobindex"
)

// Resolver maps a blob id and byte range to the chunks it covers and the source
// repository.
type Resolver interface {
	Resolve(blobID string, start, end uint64) ([]blobindex.Chunk, bool)
	BlobRepo(blobID string) (string, bool)
}

// Cache reads and writes chunk payloads keyed by content digest.
type Cache interface {
	Get(ctx context.Context, digest string) (io.ReadCloser, bool, error)
	Put(ctx context.Context, digest string, body io.Reader) error
}

// Registry fetches blob bytes from the origin on a miss.
type Registry interface {
	GetBlobRange(ctx context.Context, repo, blob string, start, end uint64) (io.ReadCloser, error)
	HeadBlob(ctx context.Context, repo, blob string) (uint64, error)
}

// Options tunes the proxy. Zero values select working defaults.
type Options struct {
	CacheTimeout     time.Duration
	PopulateWorkers  int
	PopulateQueue    int
	BreakerThreshold int
	BreakerCooldown  time.Duration
}

func (o Options) withDefaults() Options {
	if o.CacheTimeout == 0 {
		o.CacheTimeout = 50 * time.Millisecond
	}
	if o.PopulateWorkers == 0 {
		o.PopulateWorkers = 8
	}
	if o.PopulateQueue == 0 {
		o.PopulateQueue = 1024
	}
	if o.BreakerThreshold == 0 {
		o.BreakerThreshold = 5
	}
	if o.BreakerCooldown == 0 {
		o.BreakerCooldown = 2 * time.Second
	}
	return o
}

// Handler serves nydusd's http-proxy backend protocol. Create it with NewHandler
// and call Close when done to stop the populate pool.
type Handler struct {
	resolver Resolver
	cache    Cache
	registry Registry
	opts     Options

	loader  singleflight.Group
	breaker *breaker
	pool    *populatePool
	mux     *http.ServeMux
}

// NewHandler wires a proxy handler over the given index, cache, and registry.
func NewHandler(resolver Resolver, cache Cache, registry Registry, opts Options) *Handler {
	opts = opts.withDefaults()
	h := &Handler{
		resolver: resolver,
		cache:    cache,
		registry: registry,
		opts:     opts,
		breaker:  newBreaker(opts.BreakerThreshold, opts.BreakerCooldown),
	}
	h.pool = newPopulatePool(opts.PopulateWorkers, opts.PopulateQueue, h.populate)
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("/", h.serve)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Close stops the populate pool. The handler must not be used afterward.
func (h *Handler) Close() { h.pool.stop() }

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	blob := strings.TrimPrefix(r.URL.Path, "/")
	repo, ok := h.resolver.BlobRepo(blob)
	if !ok {
		http.Error(w, "unknown blob", http.StatusBadGateway)
		return
	}

	switch r.Method {
	case http.MethodHead:
		h.serveHead(w, r, repo, blob)
	case http.MethodGet:
		h.serveGet(w, r, repo, blob)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) serveHead(w http.ResponseWriter, r *http.Request, repo, blob string) {
	size, err := h.registry.HeadBlob(r.Context(), repo, blob)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatUint(size, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) serveGet(w http.ResponseWriter, r *http.Request, repo, blob string) {
	start, end, err := parseRange(r.Header.Get("Range"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	chunks, aligned := h.resolver.Resolve(blob, start, end)
	if !aligned {
		h.passthrough(w, r, repo, blob, start, end)
		return
	}

	payload := make([][]byte, 0, len(chunks))
	for _, chunk := range chunks {
		data, err := h.fetchChunk(r.Context(), repo, blob, chunk)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		payload = append(payload, data)
	}

	writePartial(w, start, end, payload)
}

// passthrough serves a byte range that does not map to whole chunks straight from
// the registry without caching, used for blob-metadata tails and any unaligned
// request.
func (h *Handler) passthrough(w http.ResponseWriter, r *http.Request, repo, blob string, start, end uint64) {
	body, err := h.registry.GetBlobRange(r.Context(), repo, blob, start, end)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writePartial(w, start, end, [][]byte{data})
}

// fetchChunk returns a chunk's bytes, collapsing concurrent fetches for the same
// digest. It serves from the cache, falls back to the registry on a miss or while
// the breaker is open, and schedules an async populate after a registry fetch.
func (h *Handler) fetchChunk(ctx context.Context, repo, blob string, chunk blobindex.Chunk) ([]byte, error) {
	v, err, _ := h.loader.Do(chunk.Digest, func() (any, error) {
		if !h.breaker.open() {
			if data, ok := h.tryCache(ctx, chunk.Digest); ok {
				return data, nil
			}
		}

		body, err := h.registry.GetBlobRange(ctx, repo, blob, chunk.Offset, chunk.Offset+chunk.Size-1)
		if err != nil {
			return nil, fmt.Errorf("registry fetch %s: %w", chunk.Digest, err)
		}
		defer body.Close()

		data, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("read registry body %s: %w", chunk.Digest, err)
		}

		if !h.breaker.open() {
			h.pool.submit(chunk.Digest, data)
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	// This slice is shared with every concurrent caller for the same digest, so callers must treat it as read-only
	return v.([]byte), nil
}

// tryCache reads a chunk from the cache under the configured timeout, updating the
// breaker. A clean miss returns ok=false with the breaker counted as a success
// because the cache was reachable.
func (h *Handler) tryCache(ctx context.Context, digest string) ([]byte, bool) {
	cctx, cancel := context.WithTimeout(ctx, h.opts.CacheTimeout)
	defer cancel()

	rc, hit, err := h.cache.Get(cctx, digest)
	if err != nil {
		h.breaker.recordFailure()
		return nil, false
	}
	h.breaker.recordSuccess()

	if !hit {
		return nil, false
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, false
	}
	return data, true
}

func (h *Handler) populate(digest string, data []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.cache.Put(ctx, digest, bytes.NewReader(data))
}

// parseRange parses an HTTP byte range of the form "bytes=start-end" with an
// inclusive end.
func parseRange(header string) (uint64, uint64, error) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, fmt.Errorf("missing or malformed Range header %q", header)
	}
	spec := strings.TrimPrefix(header, "bytes=")
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, fmt.Errorf("malformed range %q", spec)
	}

	start, err := strconv.ParseUint(strings.TrimSpace(spec[:dash]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bad range start %q: %w", spec, err)
	}

	end, err := strconv.ParseUint(strings.TrimSpace(spec[dash+1:]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bad range end %q: %w", spec, err)
	}
	return start, end, nil
}

func writePartial(w http.ResponseWriter, start, end uint64, parts [][]byte) {
	var total int
	for _, part := range parts {
		total += len(part)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, end))
	w.Header().Set("Content-Length", strconv.Itoa(total))
	w.WriteHeader(http.StatusPartialContent)
	for _, part := range parts {
		_, _ = w.Write(part)
	}
}
