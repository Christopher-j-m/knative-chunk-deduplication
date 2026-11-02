// Stresstest the cluster cache by putting a certain amount of random chunks in the cache and then run concurren goroutines to
// request these chunks (round-robin) from the cache
// The random chunks are of fixed size (chunkSize) and actually random generated (not part of an existing image e.g.)
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

type Config struct {
	BaseURL     string        // addr of the cluster cache
	Chunks      int           // amount of random unique chunks that will be inserted to the cluster cache
	ChunkSize   int           // bytes per chunk
	Concurrency int           // number of concurrent goroutines
	Duration    time.Duration // runtime of the stresstest
	Client      *http.Client  // http client for requests to the cache
}

// Results of the stresstest against the cluster cache
type Result struct {
	Requests  int             // amount of successful Gets (200) from the cache
	Errors    int             // amount of failed requests to the cache (!= 200 ok responses)
	Bytes     int64           // total bytes received by all successful requests
	Wall      time.Duration   // time from start until the last worker stopped (might be a bit longer than config.duration)
	Latencies []time.Duration // response times for each successful request to the cache until the response body was completly read
}

// Create a fake chunk hash that will be used for the cache
// => No need to clear the cache between runs, since they can't collide (due to how the same keys generate the same bytes, see Seed func)
func ChunkHash(chunkSize, index int) string {
	return fmt.Sprintf("sha256:loadgen-%d-%06d", chunkSize, index)
}

func (cfg Config) client() *http.Client {
	if cfg.Client != nil {
		return cfg.Client
	}

	// Increase connection limits for higher concurrency
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 4096
	transport.MaxIdleConnsPerHost = 4096
	transport.MaxConnsPerHost = 0 // Remove max connection limit

	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

// Generates random chunks of fixed size and puts them in the cluster cache
// => Random chunks are different within one run, but the same for each run due to same seed
func Seed(ctx context.Context, cfg Config) error {
	client := cfg.client()
	payload := make([]byte, cfg.ChunkSize)
	source := rand.New(rand.NewSource(1))

	// Generate payload with new random bytes each iteration & send it to cache
	for index := 0; index < cfg.Chunks; index++ {

		// Read the first amount of chunkSize bytes from the random stream
		_, _ = source.Read(payload)

		// Send random chunk to cache
		url := fmt.Sprintf("%s/chunks/%s", cfg.BaseURL, ChunkHash(cfg.ChunkSize, index))
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("error during building the seed request for chunk %d: %w", index, err)
		}

		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("error during seeding the chunk %d: %w", index, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()

		if response.StatusCode >= 300 {
			return fmt.Errorf("error while trying to seed chunk %d: cache answered %s", index, response.Status)
		}
	}

	return nil
}

// Start the concurrent goroutines with the same deadline of config.Duration
// Each worker will try to fetch all the chunks until the run ends,
// but starts from its own offset in order to spread across chunks (so not all want the same chunk at the same time)
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Concurrency < 1 || cfg.Chunks < 1 {
		return Result{}, fmt.Errorf("this doesnt work as a run needs at least one worker and one chunk")
	}

	client := cfg.client()
	// Set deadline/runtime for this run for all workers
	deadline, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	type workerResult struct {
		requests  int             // successful 200 responses
		errors    int             // amount of != 200 responses
		bytes     int64           // bytes from response bodies read
		latencies []time.Duration // response time for each successful request
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

			// Deadline not reached yet
			for deadline.Err() == nil {
				url := fmt.Sprintf("%s/chunks/%s", cfg.BaseURL, ChunkHash(cfg.ChunkSize, index))
				// Shift index to next chunk (based on starting offset)
				index = (index + 1) % cfg.Chunks

				// Send request to cache
				request, err := http.NewRequestWithContext(deadline, http.MethodGet, url, nil)
				if err != nil {
					local.errors++
					continue
				}

				at := time.Now() // Response sent time
				response, err := client.Do(request)
				if err != nil {
					// This request got interrupted by the deadline being hit,
					// so dont count this as failed request
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

	// Sum up all individual results
	aggregate := Result{Wall: time.Since(started)}
	for _, local := range results {
		aggregate.Requests += local.requests
		aggregate.Errors += local.errors
		aggregate.Bytes += local.bytes
		aggregate.Latencies = append(aggregate.Latencies, local.latencies...)
	}

	return aggregate, nil
}
