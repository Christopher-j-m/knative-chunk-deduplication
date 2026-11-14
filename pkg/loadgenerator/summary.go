package loadgenerator

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// Contains the final results of a stresstest run => How fast was the cache when a certain amount of concurrent workers were requesting
type Summary struct {
	Concurrency       int           // number of concurrent workers (via goroutines)
	ChunkSize         int           // bytes of each chunk in the cache (fixed)
	Requests          int           // successful 200 responses
	Errors            int           // failed requests => either transmission errors or != 200 responses
	Bytes             int64         // amount of bytes read from successful response bodies
	Wall              time.Duration // Time between starting the workers - until last one stopped
	RequestsPerSecond float64       // request throughput
	MiBPerSecond      float64       // throughput in data
	// Request latency
	P50 time.Duration
	P95 time.Duration
	P99 time.Duration
	Max time.Duration
}

// Takes a slice of request latency values and returns the specific quantile based on that slice
func Percentile(latencies []time.Duration, quantile float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}

	// Sort by latency values
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// Get the position in the slice of the specific percentile
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}

	if index >= len(sorted) {
		index = len(sorted) - 1
	}

	return sorted[index]
}

func Summarize(cfg Config, runResult Result) Summary {
	summary := Summary{
		Concurrency: cfg.Concurrency,
		ChunkSize:   cfg.ChunkSize,
		Requests:    runResult.Requests,
		Errors:      runResult.Errors,
		Bytes:       runResult.Bytes,
		Wall:        runResult.Wall,
		P50:         Percentile(runResult.Latencies, 0.50),
		P95:         Percentile(runResult.Latencies, 0.95),
		P99:         Percentile(runResult.Latencies, 0.99),
		Max:         Percentile(runResult.Latencies, 1.0),
	}

	seconds := runResult.Wall.Seconds()
	if seconds > 0 {
		summary.RequestsPerSecond = float64(runResult.Requests) / seconds
		summary.MiBPerSecond = float64(runResult.Bytes) / (1024 * 1024) / seconds
	}

	return summary
}

// Result csv column names
func SummaryHeader() []string {
	return []string{
		"label", "concurrency", "chunk_size_bytes", "wall_seconds",
		"requests", "errors", "bytes", "requests_per_second",
		"mib_per_second", "p50_ms", "p95_ms", "p99_ms", "max_ms",
	}
}

// Parse a summary obj to csv fields in correct order
func (s Summary) Row(label string) []string {

	// Round ms with 3 decimals
	milliseconds := func(d time.Duration) string {
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
	}

	// Rates rounded for 1 or two decimals
	return []string{
		label,
		strconv.Itoa(s.Concurrency),
		strconv.Itoa(s.ChunkSize),
		strconv.FormatFloat(s.Wall.Seconds(), 'f', 3, 64),
		strconv.Itoa(s.Requests),
		strconv.Itoa(s.Errors),
		strconv.FormatInt(s.Bytes, 10),
		strconv.FormatFloat(s.RequestsPerSecond, 'f', 1, 64),
		strconv.FormatFloat(s.MiBPerSecond, 'f', 2, 64),
		milliseconds(s.P50),
		milliseconds(s.P95),
		milliseconds(s.P99),
		milliseconds(s.Max),
	}
}
