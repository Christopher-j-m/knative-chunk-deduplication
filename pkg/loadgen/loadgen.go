// Package loadgen drives the cache service hard enough to find its ceiling.
// It seeds a working set of chunks, then reads them back at a fixed
// concurrency for a fixed time, so throughput and latency can be reported as a
// function of how many requesters the pod is serving at once.
package loadgen

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// Config describes one measurement point against one cache service.
type Config struct {
	BaseURL     string
	Chunks      int
	ChunkSize   int
	Concurrency int
	Duration    time.Duration
	Client      *http.Client
}

// Result is the raw accounting of one run, before it is turned into rates.
type Result struct {
	Requests  int
	Errors    int
	Bytes     int64
	Wall      time.Duration
	Latencies []time.Duration
}

// ChunkHash names the chunk at the given index. The name encodes the chunk
// size as well, so two runs at different sizes never collide in a cache that
// was not emptied in between.
func ChunkHash(chunkSize, index int) string {
	return fmt.Sprintf("sha256:loadgen-%d-%06d", chunkSize, index)
}

func (cfg Config) client() *http.Client {
	if cfg.Client != nil {
		return cfg.Client
	}

	// the default transport keeps two idle connections per host, which
	// throttles a high-concurrency run to reconnecting rather than to the
	// server it is meant to measure
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 4096
	transport.MaxIdleConnsPerHost = 4096
	transport.MaxConnsPerHost = 0

	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

// Seed fills the cache with the run's working set. The payload is random so
// the numbers are not flattered by anything that might compress it.
func Seed(ctx context.Context, cfg Config) error {
	client := cfg.client()
	payload := make([]byte, cfg.ChunkSize)
	source := rand.New(rand.NewSource(1))

	for index := 0; index < cfg.Chunks; index++ {
		_, _ = source.Read(payload)

		url := fmt.Sprintf("%s/chunks/%s", cfg.BaseURL, ChunkHash(cfg.ChunkSize, index))
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("build seed request for chunk %d: %w", index, err)
		}

		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("seed chunk %d: %w", index, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()

		if response.StatusCode >= 300 {
			return fmt.Errorf("seed chunk %d: cache answered %s", index, response.Status)
		}
	}

	return nil
}

// Run reads the seeded working set back at the configured concurrency until
// the duration elapses, and reports what it observed. Each worker walks the
// working set from its own offset, so the requests spread across chunks
// rather than hammering one.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Concurrency < 1 || cfg.Chunks < 1 {
		return Result{}, fmt.Errorf("a run needs at least one worker and one chunk")
	}

	client := cfg.client()
	deadline, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	type workerResult struct {
		requests  int
		errors    int
		bytes     int64
		latencies []time.Duration
	}

	results := make([]workerResult, cfg.Concurrency)
	var waitGroup sync.WaitGroup
	started := time.Now()

	for worker := 0; worker < cfg.Concurrency; worker++ {
		waitGroup.Add(1)
		go func(worker int) {
			defer waitGroup.Done()
			local := &results[worker]
			index := worker % cfg.Chunks

			for deadline.Err() == nil {
				url := fmt.Sprintf("%s/chunks/%s", cfg.BaseURL, ChunkHash(cfg.ChunkSize, index))
				index = (index + 1) % cfg.Chunks

				request, err := http.NewRequestWithContext(deadline, http.MethodGet, url, nil)
				if err != nil {
					local.errors++
					continue
				}

				at := time.Now()
				response, err := client.Do(request)
				if err != nil {
					// a request cut off by the deadline is the run ending,
					// not the cache failing
					if deadline.Err() != nil {
						return
					}
					local.errors++
					continue
				}

				read, _ := io.Copy(io.Discard, response.Body)
				response.Body.Close()
				elapsed := time.Since(at)

				if response.StatusCode != http.StatusOK {
					local.errors++
					continue
				}

				local.requests++
				local.bytes += read
				local.latencies = append(local.latencies, elapsed)
			}
		}(worker)
	}

	waitGroup.Wait()

	aggregate := Result{Wall: time.Since(started)}
	for _, local := range results {
		aggregate.Requests += local.requests
		aggregate.Errors += local.errors
		aggregate.Bytes += local.bytes
		aggregate.Latencies = append(aggregate.Latencies, local.latencies...)
	}

	return aggregate, nil
}
