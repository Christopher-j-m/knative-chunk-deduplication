// The chunk proxy has to be running on every cluster node in order to translate between nydusd (asks for byte ranges)
// and the cluster chunk cache(stores chunks by hash) => Translation from byte ranges to chunk hashes
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"nydus-cluster-cache/pkg/blobindex"
	"nydus-cluster-cache/pkg/cacheclient"
	"nydus-cluster-cache/pkg/nydus"
	"nydus-cluster-cache/pkg/proxy"
	"nydus-cluster-cache/pkg/registry"
)

type config struct {
	socket           string        // Socket for nydusd to send the requests for blobs to
	cacheEndpoint    string        // Base url of the cluster chunk cache that will be called by this proxy
	registryBase     string        // Base url of the remote registry (in case of cache misses)
	bootstrapDir     string        // Dir where the nydus bootstrap metadata files are located
	bootstrapName    string        // file name of the bootstrap metadata files within the bootstrapDir
	rescanInterval   time.Duration // How often we're re-scanning the bootstrapDir
	cacheTimeout     time.Duration // Max. time the proxy waits for the cluster cache to respond
	populateWorkers  int           // Number of workers that will write chunks for the queue to the cluster cache
	populateQueue    int           // Max. number of chunks the queue can hold at the same time until new additions get dropped
	breakerThreshold int           // Amount of cache failures (not misses) in a row before assuming downtime & going to remote registry first
	breakerCooldown  time.Duration // Time until retrying the cluster cache after assuming downtime
	cacheEnabled     bool          // Control whether to skip the cluster cache entirely & going to the remote registry directly
	parallelMiss     bool          // Ask the registry alongside the cache instead of after a miss
}

func main() {
	cfg := parseFlags()

	// Get the chunk data from a specific image
	extract := func(ctx context.Context, path, image string) ([]nydus.ChunkInfo, error) {
		imageChunks, err := nydus.ExtractChunks(ctx, path, image)
		if err != nil {
			return nil, err
		}
		return imageChunks.Chunks, nil
	}

	// Initial scan in the bootstrapDir for nydus bootstrap metadata files
	loader := blobindex.NewLoader(cfg.bootstrapDir, cfg.bootstrapName, extract, blobindex.RequireRepoHint())
	if err := loader.Refresh(context.Background()); err != nil {
		log.Printf("error during initial bootstrap scan: %v", err)
	}

	// Counters are served next to the blob routes, so a benchmark can read them from the node
	metricsRegistry := prometheus.NewRegistry()

	// Create handler for handling the nydusd requests
	handler := proxy.NewHandler(
		loaderResolver{loader},
		cacheclient.New(cfg.cacheEndpoint),
		registry.New(cfg.registryBase),
		proxy.Options{
			CacheTimeout:      cfg.cacheTimeout,
			PopulateWorkers:   cfg.populateWorkers,
			PopulateQueue:     cfg.populateQueue,
			BreakerThreshold:  cfg.breakerThreshold,
			BreakerCooldown:   cfg.breakerCooldown,
			CacheDisabled:     !cfg.cacheEnabled,
			ParallelMissFetch: cfg.parallelMiss,
			Refresh:           loader.Refresh,
			Metrics:           proxy.NewMetrics(metricsRegistry),
		},
	)
	defer handler.Close()

	runUntilSignal(cfg, withMetricsRoute(handler, metricsRegistry), loader)
}

// Adds a /metrics route in front of the proxy. Blob names are hex digests, so
// they never collide with it.
func withMetricsRoute(proxyHandler http.Handler, metricsRegistry *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{}))
	mux.Handle("/", proxyHandler)
	return mux
}

// Allow the handler to always use the most recent lookup blob index table
type loaderResolver struct{ loader *blobindex.Loader }

// Returns the chunks that belong to a given byte range
func (resolver loaderResolver) Resolve(blobID string, start, end uint64) ([]blobindex.Chunk, bool) {
	return resolver.loader.Index().Resolve(blobID, start, end)
}

// Returns in which registry repository the specific blob is located at
func (resolver loaderResolver) BlobRepo(blobID string) (string, bool) {
	return resolver.loader.Index().BlobRepo(blobID)
}

// Returns the type of connection this proxy listens to (unix, tcp)
func listenNetwork(address string) string {
	// Addr with a slash, e.g. ./chunk-proxy.sock => filepath
	if strings.Contains(address, "/") {
		return "unix"
	}
	// ...else tcp
	return "tcp"
}

// Set the default config values & parse and set the given input flags based on user input
func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.socket, "listen", "127.0.0.1:18000", "Address nydusd connects to, a host:port or a unix socket path")
	flag.StringVar(&cfg.cacheEndpoint, "cache-endpoint", "", "Cluster cache base URL")
	flag.StringVar(&cfg.registryBase, "registry", "", "Origin registry base URL")
	flag.StringVar(&cfg.bootstrapDir, "bootstrap-dir", "", "Snapshotter bootstrap root to scan")
	flag.StringVar(&cfg.bootstrapName, "bootstrap-name", "image.boot", "Bootstrap file name to match")
	flag.DurationVar(&cfg.rescanInterval, "rescan-interval", 5*time.Second, "How often to re-scan for new bootstraps")
	flag.DurationVar(&cfg.cacheTimeout, "cache-timeout", 50*time.Millisecond, "Per-request cache deadline")
	flag.IntVar(&cfg.populateWorkers, "populate-workers", 8, "Async cache populate pool size")
	flag.IntVar(&cfg.populateQueue, "populate-queue", 1024, "Async cache populate queue depth")
	flag.IntVar(&cfg.breakerThreshold, "breaker-threshold", 5, "Consecutive cache failures before temporarily skip the cluster cache path")
	flag.DurationVar(&cfg.breakerCooldown, "breaker-cooldown", 2*time.Second, "Timeout until retrying if cache is available again")
	flag.BoolVar(&cfg.cacheEnabled, "cache-enabled", true, "Serve chunks via the cluster cache => set false to bypass straight to the registry")
	flag.BoolVar(&cfg.parallelMiss, "parallel-miss-fetch", false, "Ask the registry at the same time as the cache; a hit cancels the registry request")
	flag.Parse() // User input

	// Check for required params
	if cfg.cacheEndpoint == "" || cfg.registryBase == "" || cfg.bootstrapDir == "" {
		log.Fatal("flags -cache-endpoint, -registry, and -bootstrap-dir are required")
	}

	return cfg
}

// RUn the proxy until process is killed or terminated manually via strg+c
func runUntilSignal(cfg config, handler http.Handler, loader *blobindex.Loader) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	network := listenNetwork(cfg.socket)
	if network == "unix" {
		_ = os.Remove(cfg.socket)
		// Unlink the socket file when we exit so the next start finds a clean path
		defer os.Remove(cfg.socket)
	}
	listener, err := net.Listen(network, cfg.socket)
	if err != nil {
		log.Fatalf("listen on %s: %v", cfg.socket, err)
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	// Auto-schedule the blob index lookup table rescan
	go func() {
		ticker := time.NewTicker(cfg.rescanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := loader.Refresh(ctx); err != nil {
					log.Printf("index refresh: %v", err)
				}
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("chunk-proxy serving on %s", cfg.socket)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		log.Printf("serve: %v", err)
	}

	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}
