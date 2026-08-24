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
	cfg := parseFlags()

	backing, err := buildStore(cfg)
	if err != nil {
		log.Fatalf("build store: %v", err)
	}

	registry := newRegistry()
	metrics := cache.NewMetrics(registry)
	c := cache.New(backing, cache.Options{
		CapacityBytes: cfg.capacityBytes,
		Metrics:       metrics,
	})

	mux := http.NewServeMux()
	mux.Handle("/", server.NewHandler(c))
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// the touched-set capture reads this; stores that cannot enumerate their
	// contents simply do not get the route
	if lister, ok := backing.(store.Lister); ok {
		mux.Handle("/debug/chunks", server.NewChunkListHandler(lister))
	}

	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	runUntilSignal(httpServer)
}

// newRegistry builds the metrics registry the service publishes. The cache's
// own counters are registered by the caller; the process and runtime
// collectors are added here so a campaign can tell a saturated cache pod from
// an idle one without reaching into the container.
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
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.listenAddr, "listen", ":8080", "HTTP listen address")
	flag.StringVar(&cfg.storeKind, "store", "memory", "Store backend: memory or disk")
	flag.StringVar(&cfg.storeRoot, "store-root", "/var/lib/nydus-cluster-cache", "Disk store root directory")
	flag.Uint64Var(&cfg.capacityBytes, "capacity-bytes", 0, "Eviction capacity in bytes; zero disables eviction")
	flag.Parse()
	return cfg
}

func buildStore(cfg config) (store.Store, error) {
	switch cfg.storeKind {
	case "memory":
		return store.NewMemoryStore(), nil
	case "disk":
		return store.NewDiskStore(cfg.storeRoot)
	default:
		return nil, errors.New("unknown store backend: " + cfg.storeKind)
	}
}

func runUntilSignal(httpServer *http.Server) {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("cache service listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-signalCtx.Done()
	log.Printf("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
		os.Exit(1)
	}
}
