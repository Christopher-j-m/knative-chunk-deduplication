package cache

import (
	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	Hits              prometheus.Counter
	Misses            prometheus.Counter
	PutBytes          prometheus.Counter
	StoredBytes       prometheus.Gauge
	StoredChunks      prometheus.Gauge
	GetLatencySeconds prometheus.Histogram
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	allMetrics := &Metrics{
		Hits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_hits_total",
			Help: "Number of cache hits since start",
		}),
		Misses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_misses_total",
			Help: "Number of cache misses since start",
		}),
		PutBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_put_bytes_total",
			Help: "Total bytes received via Put",
		}),
		StoredBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cache_stored_bytes",
			Help: "Current cache size in bytes",
		}),
		StoredChunks: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cache_stored_chunks",
			Help: "Current number of unique chunks in cache",
		}),
		GetLatencySeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "cache_get_latency_seconds",
			Help:    "Server-side Get latency in seconds",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12),
		}),
	}

	if registerer != nil {
		registerer.MustRegister(
			allMetrics.Hits, allMetrics.Misses, allMetrics.PutBytes,
			allMetrics.StoredBytes, allMetrics.StoredChunks, allMetrics.GetLatencySeconds,
		)
	}

	return allMetrics
}
