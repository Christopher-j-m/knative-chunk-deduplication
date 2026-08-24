package loadgen

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// Summary is one measured point: what a fixed concurrency achieved against
// the cache service, with the latency distribution the client observed.
type Summary struct {
	Concurrency       int
	ChunkSize         int
	Requests          int
	Errors            int
	Bytes             int64
	Wall              time.Duration
	RequestsPerSecond float64
	MiBPerSecond      float64
	P50               time.Duration
	P95               time.Duration
	P99               time.Duration
	Max               time.Duration
}

// Percentile returns the latency at the given quantile, by nearest rank: the
// value reported is one a request actually observed, and a tail figure is
// never softened by interpolating toward a faster neighbour. The input is
// copied before sorting, so a caller's slice keeps its order.
func Percentile(latencies []time.Duration, quantile float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}

	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

// Summarize turns one run's raw counters into rates and percentiles.
func Summarize(cfg Config, r Result) Summary {
	summary := Summary{
		Concurrency: cfg.Concurrency,
		ChunkSize:   cfg.ChunkSize,
		Requests:    r.Requests,
		Errors:      r.Errors,
		Bytes:       r.Bytes,
		Wall:        r.Wall,
		P50:         Percentile(r.Latencies, 0.50),
		P95:         Percentile(r.Latencies, 0.95),
		P99:         Percentile(r.Latencies, 0.99),
		Max:         Percentile(r.Latencies, 1.0),
	}

	seconds := r.Wall.Seconds()
	if seconds > 0 {
		summary.RequestsPerSecond = float64(r.Requests) / seconds
		summary.MiBPerSecond = float64(r.Bytes) / (1024 * 1024) / seconds
	}

	return summary
}

// SummaryHeader names the CSV columns Row emits, in the same order.
func SummaryHeader() []string {
	return []string{
		"label", "concurrency", "chunk_size_bytes", "wall_seconds",
		"requests", "errors", "bytes", "requests_per_second",
		"mib_per_second", "p50_ms", "p95_ms", "p99_ms", "max_ms",
	}
}

// Row flattens the summary into CSV fields under the given campaign label.
func (s Summary) Row(label string) []string {
	milliseconds := func(d time.Duration) string {
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
	}

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
