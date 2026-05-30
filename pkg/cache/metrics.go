package cache

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics groups every Prometheus collector the cache exposes. A nil receiver
// is supported so unit tests can build a cache without wiring metrics.
type Metrics struct {
	Hits              prometheus.Counter
	Misses            prometheus.Counter
	PutBytes          prometheus.Counter
	StoredBytes       prometheus.Gauge
	StoredChunks      prometheus.Gauge
	GetLatencySeconds prometheus.Histogram
}

// NewMetrics creates a Metrics bundle and registers it on the given registerer.
// Pass prometheus.DefaultRegisterer for production wiring.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	m := &Metrics{
		Hits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_hits_total",
			Help: "Cumulative number of cache hits since process start",
		}),
		Misses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_misses_total",
			Help: "Cumulative number of cache misses since process start",
		}),
		PutBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_put_bytes_total",
			Help: "Cumulative bytes accepted via Put",
		}),
		StoredBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cache_stored_bytes",
			Help: "Current cache size in bytes",
		}),
		StoredChunks: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cache_stored_chunks",
			Help: "Current number of distinct chunks held",
		}),
		GetLatencySeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "cache_get_latency_seconds",
			Help:    "Server-side Get latency in seconds",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12),
		}),
	}

	if registerer != nil {
		registerer.MustRegister(
			m.Hits, m.Misses, m.PutBytes,
			m.StoredBytes, m.StoredChunks, m.GetLatencySeconds,
		)
	}
	return m
}
