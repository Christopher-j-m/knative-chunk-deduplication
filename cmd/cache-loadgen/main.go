// Runs the actual load test on a single cluster pod, with the cluster cache running, can serve under high concurrently
// This runs multiple concurrency levels/difficulties and writes the corresponding results in a csv with a row for each level
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"nydus-cluster-cache/pkg/loadgen"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	var (
		addr        = flag.String("addr", "http://10.43.0.50:8080", "Base URL of the cache service")
		chunks      = flag.Int("chunks", 512, "Number of unique chunks")
		chunkSize   = flag.Int("chunk-size", 32*1024, "Size of each chunk in bytes")
		concurrency = flag.String("concurrency", "1,2,4,8,16,32,64,128,256", "Comma-separated concurrency levels that will be run after each other")
		duration    = flag.Duration("duration", 10*time.Second, "How long to read at each concurrency level")
		label       = flag.String("label", "default", "Experiment label to find this run easier")
	)
	flag.Parse() // Parse user input

	// Parse concurrency input param string to int slice
	levels, err := parseConcurrency(*concurrency)
	if err != nil {
		log.Fatalf("error while parsing concurrency input: %v", err)
	}

	base := loadgen.Config{
		BaseURL:   strings.TrimSuffix(*addr, "/"),
		Chunks:    *chunks,
		ChunkSize: *chunkSize,
		Duration:  *duration,
	}

	ctx := context.Background()
	log.Printf("inserting %d chunks (%d bytes) into cache at %s", base.Chunks, base.ChunkSize, base.BaseURL)
	if err := loadgen.Seed(ctx, base); err != nil {
		log.Fatalf("error while inserting chunk: %v", err)
	}

	// Csv writer for the results
	writer := csv.NewWriter(os.Stdout)
	if err := writer.Write(loadgen.SummaryHeader()); err != nil {
		log.Fatalf("error at csv writer during writing header: %v", err)
	}

	for _, currConcurrencyLevel := range levels {
		cfg := base
		cfg.Concurrency = currConcurrencyLevel

		result, err := loadgen.Run(ctx, cfg)
		if err != nil {
			log.Fatalf("error while running at concurrency %d: %v", currConcurrencyLevel, err)
		}

		summary := loadgen.Summarize(cfg, result)
		if err := writer.Write(summary.Row(*label)); err != nil {
			log.Fatalf("error while writing row to csv: %v", err)
		}
		writer.Flush()

		log.Printf("concurrency %d: %.1f req/s, %.2f MiB/s, p99 %v, %d errors", currConcurrencyLevel, summary.RequestsPerSecond, summary.MiBPerSecond, summary.P99, summary.Errors)
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		log.Fatalf("error while trying to flush remaining buffer to result csv: %v", err)
	}
}

// Parse the concurrency input param string to a int slice that we can iterate over
// Also validates that each value is actually a number & > 0
// => This skips every invalid value between commas, but it throws if at the end no valid values are left
// e.g. " ,,2" => returns [2] and skips " " and ""
func parseConcurrency(list string) ([]int, error) {
	fields := strings.Split(list, ",")
	levels := make([]int, 0, len(fields))

	for _, currField := range fields {
		trimmed := strings.TrimSpace(currField)
		// Skip empty value
		if trimmed == "" {
			continue
		}

		level, err := strconv.Atoi(trimmed)
		// Throw is we found a non-number
		if err != nil {
			return nil, fmt.Errorf("error for %q since it is not a number", trimmed)
		}
		if level < 1 {
			return nil, fmt.Errorf("found %d, but we only allow positive > 0 concurrency level values", level)
		}

		levels = append(levels, level)
	}

	if len(levels) == 0 {
		return nil, fmt.Errorf("error: no remaining valid concurrency value left")
	}
	return levels, nil
}
