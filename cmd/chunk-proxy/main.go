// cmd/chunk-proxy/main.go
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
	"syscall"
	"time"

	"nydus-cluster-cache/pkg/blobindex"
	"nydus-cluster-cache/pkg/cacheclient"
	"nydus-cluster-cache/pkg/nydus"
	"nydus-cluster-cache/pkg/proxy"
	"nydus-cluster-cache/pkg/registry"
)

type config struct {
	socket           string
	cacheEndpoint    string
	registryBase     string
	bootstrapDir     string
	bootstrapName    string
	rescanInterval   time.Duration
	cacheTimeout     time.Duration
	populateWorkers  int
	populateQueue    int
	breakerThreshold int
	breakerCooldown  time.Duration
}

func main() {
	cfg := parseFlags()

	extract := func(ctx context.Context, path, image string) ([]nydus.ChunkInfo, error) {
		ic, err := nydus.ExtractChunks(ctx, path, image)
		if err != nil {
			return nil, err
		}
		return ic.Chunks, nil
	}
	loader := blobindex.NewLoader(cfg.bootstrapDir, cfg.bootstrapName, extract)
	if err := loader.Refresh(context.Background()); err != nil {
		log.Printf("initial index refresh: %v", err)
	}

	handler := proxy.NewHandler(
		loaderResolver{loader},
		cacheclient.New(cfg.cacheEndpoint),
		registry.New(cfg.registryBase),
		proxy.Options{
			CacheTimeout:     cfg.cacheTimeout,
			PopulateWorkers:  cfg.populateWorkers,
			PopulateQueue:    cfg.populateQueue,
			BreakerThreshold: cfg.breakerThreshold,
			BreakerCooldown:  cfg.breakerCooldown,
		},
	)
	defer handler.Close()

	runUntilSignal(cfg, handler, loader)
}

// loaderResolver adapts the loader's swappable index to the proxy's Resolver
// interface, always reading the latest index.
type loaderResolver struct{ loader *blobindex.Loader }

func (lr loaderResolver) Resolve(blobID string, start, end uint64) ([]blobindex.Chunk, bool) {
	return lr.loader.Index().Resolve(blobID, start, end)
}

func (lr loaderResolver) BlobRepo(blobID string) (string, bool) {
	return lr.loader.Index().BlobRepo(blobID)
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.socket, "listen", "/run/chunk-proxy.sock", "Unix socket nydusd connects to")
	flag.StringVar(&cfg.cacheEndpoint, "cache-endpoint", "", "Cluster cache base URL")
	flag.StringVar(&cfg.registryBase, "registry", "", "Origin registry base URL")
	flag.StringVar(&cfg.bootstrapDir, "bootstrap-dir", "", "Snapshotter bootstrap root to scan")
	flag.StringVar(&cfg.bootstrapName, "bootstrap-name", "image.boot", "Bootstrap file name to match")
	flag.DurationVar(&cfg.rescanInterval, "rescan-interval", 5*time.Second, "How often to re-scan for new bootstraps")
	flag.DurationVar(&cfg.cacheTimeout, "cache-timeout", 50*time.Millisecond, "Per-request cache deadline")
	flag.IntVar(&cfg.populateWorkers, "populate-workers", 8, "Async cache populate pool size")
	flag.IntVar(&cfg.populateQueue, "populate-queue", 1024, "Async cache populate queue depth")
	flag.IntVar(&cfg.breakerThreshold, "breaker-threshold", 5, "Consecutive cache failures before opening the breaker")
	flag.DurationVar(&cfg.breakerCooldown, "breaker-cooldown", 2*time.Second, "Breaker open-to-half-open wait")
	flag.Parse()

	if cfg.cacheEndpoint == "" || cfg.registryBase == "" || cfg.bootstrapDir == "" {
		log.Fatal("flags -cache-endpoint, -registry, and -bootstrap-dir are required")
	}
	return cfg
}

func runUntilSignal(cfg config, handler *proxy.Handler, loader *blobindex.Loader) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	_ = os.Remove(cfg.socket)
	listener, err := net.Listen("unix", cfg.socket)
	if err != nil {
		log.Fatalf("listen on %s: %v", cfg.socket, err)
	}
	// Unlink the socket file when we exit so the next start finds a clean path
	defer os.Remove(cfg.socket)

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

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

	// Drain on a signal or a fatal serve error, then shut down gracefully either way
	// so the populate pool and listener are released cleanly
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
