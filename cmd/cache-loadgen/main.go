// Command cache-loadgen measures what one cache service pod can serve. It
// seeds a working set, then sweeps a list of concurrency levels against it and
// writes one CSV row per level. Run it inside the cluster so the measurement
// crosses the same path a chunk proxy crosses.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"nydus-cluster-cache/pkg/loadgen"
)

func main() {
	var (
		addr        = flag.String("addr", "http://10.43.0.50:8080", "Base URL of the cache service")
		chunks      = flag.Int("chunks", 512, "Number of distinct chunks in the working set")
		chunkSize   = flag.Int("chunk-size", 32*1024, "Size of each chunk in bytes")
		concurrency = flag.String("concurrency", "1,2,4,8,16,32,64,128,256", "Comma-separated concurrency levels to sweep")
		duration    = flag.Duration("duration", 10*time.Second, "How long to read at each concurrency level")
		label       = flag.String("label", "default", "Campaign label recorded in the first CSV column")
	)
	flag.Parse()

	levels, err := parseConcurrency(*concurrency)
	if err != nil {
		log.Fatalf("concurrency: %v", err)
	}

	base := loadgen.Config{
		BaseURL:   strings.TrimSuffix(*addr, "/"),
		Chunks:    *chunks,
		ChunkSize: *chunkSize,
		Duration:  *duration,
	}

	ctx := context.Background()
	log.Printf("seeding %d chunks of %d bytes into %s", base.Chunks, base.ChunkSize, base.BaseURL)
	if err := loadgen.Seed(ctx, base); err != nil {
		log.Fatalf("seed: %v", err)
	}

	writer := csv.NewWriter(os.Stdout)
	if err := writer.Write(loadgen.SummaryHeader()); err != nil {
		log.Fatalf("write header: %v", err)
	}

	for _, level := range levels {
		cfg := base
		cfg.Concurrency = level

		result, err := loadgen.Run(ctx, cfg)
		if err != nil {
			log.Fatalf("run at concurrency %d: %v", level, err)
		}

		summary := loadgen.Summarize(cfg, result)
		if err := writer.Write(summary.Row(*label)); err != nil {
			log.Fatalf("write row: %v", err)
		}
		writer.Flush()

		log.Printf("concurrency %d: %.1f req/s, %.2f MiB/s, p99 %v, %d errors",
			level, summary.RequestsPerSecond, summary.MiBPerSecond, summary.P99, summary.Errors)
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		log.Fatalf("flush: %v", err)
	}
}

// parseConcurrency reads the sweep the operator asked for. An empty or
// malformed list is refused rather than silently reduced, because a sweep that
// quietly drops a level produces a curve with an invisible hole in it.
func parseConcurrency(list string) ([]int, error) {
	fields := strings.Split(list, ",")
	levels := make([]int, 0, len(fields))

	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed == "" {
			continue
		}

		level, err := strconv.Atoi(trimmed)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", trimmed)
		}
		if level < 1 {
			return nil, fmt.Errorf("concurrency %d is below one", level)
		}
		levels = append(levels, level)
	}

	if len(levels) == 0 {
		return nil, fmt.Errorf("no concurrency levels given")
	}
	return levels, nil
}
