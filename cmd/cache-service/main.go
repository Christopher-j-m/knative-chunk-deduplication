package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"nydus-cluster-cache/pkg/cache"
	"nydus-cluster-cache/pkg/server"
	"nydus-cluster-cache/pkg/store"
)

func main() {
	// Parse input params/flags
	cfg := parseFlags()

	// Create the store layer of the cluster cache (in-memory)
	cacheStore, err := buildStore(cfg)
	if err != nil {
		log.Fatalf("build in-memory cache store: %v", err)
	}

	// Create prometheus registry for our metrics
	registry := newRegistry()
	metrics := cache.NewMetrics(registry)

	// Create the cluster cache with the created cache store as backend layer
	chunkCache := cache.New(cacheStore, cache.Options{
		CapacityBytes: cfg.capacityBytes,
		Metrics:       metrics,
	})

	// Route requests to either cluster cache api (/) or prometheus handler (/metrics)
	mux := http.NewServeMux()
	mux.Handle("/", server.NewHandler(chunkCache, handlerOptions(cfg)...))
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// Check if the store we created earlier actually implements the Chunks() functions (via store.Lister) that would be required for this route
	if lister, ok := cacheStore.(store.Lister); ok {
		// Endpoint for outputting the whole content of the cache store => all chunks including its hashes in json
		mux.Handle("/debug/chunks", server.NewChunkListHandler(lister))
	}

	// Cache service networking
	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Run the cache service until we exit it explicitly...
	runUntilSignal(httpServer)
}

// Create a new prometheus registry for our to be collected metrics
func newRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)
	return registry
}

type config struct {
	listenAddr    string
	storeKind     string
	storeRoot     string
	capacityBytes uint64
	checkDigests  bool // Refuse uploads whose bytes do not hash to the chunk name
}

// Turns the parsed flags into options for the chunk API handler
func handlerOptions(cfg config) []server.Option {
	if !cfg.checkDigests {
		return nil
	}
	return []server.Option{server.WithDigestCheck()}
}

// Set default values for each input param of this binary and replace those if with the values passed to this binary
// E.g. --listen=:8081 as input param causes listenAddr value to be replaced at the end with :8081 (:8080 would only stand if we omitted --listen)
func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.listenAddr, "listen", ":8080", "HTTP listen address")
	flag.StringVar(&cfg.storeKind, "store", "memory", "Store backend: memory or disk")
	flag.StringVar(&cfg.storeRoot, "store-root", "/var/lib/nydus-cluster-cache", "Disk store root directory")
	flag.Uint64Var(&cfg.capacityBytes, "capacity-bytes", 0, "Eviction capacity in bytes; zero disables eviction")
	flag.BoolVar(&cfg.checkDigests, "check-digests", false, "Refuse chunk uploads whose bytes do not hash to their name; leave off for synthetic load tests")
	flag.Parse() // Replace above default values with the specific binary input params
	return cfg
}

// Create either an in-memory or persistend disk store for the cache
// Note: CUrrently we only use in-memory store for all the thesis experiments
func buildStore(cfg config) (store.Store, error) {
	switch cfg.storeKind {
	case "memory":
		return store.NewMemoryStore(), nil
	case "disk":
		return store.NewDiskStore(cfg.storeRoot)
	default:
		return nil, errors.New("unknown store type requested, only 'memory' & 'disk' allowed: " + cfg.storeKind)
	}
}

// Run the cluster cache service/process until kill or ctrl+c
func runUntilSignal(httpServer *http.Server) {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("cluster cache service listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("cluster cache service error while listening: %v", err)
		}
	}()

	<-signalCtx.Done()
	log.Printf("exiting...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("failed to shut down the cache service: %v", err)
		os.Exit(1)
	}
}
