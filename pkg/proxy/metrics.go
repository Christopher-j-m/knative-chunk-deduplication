package proxy

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the counters a chunk proxy reports about its cache reads and registry fetches.
type Metrics struct {
	DigestMismatches    prometheus.Counter
	LookupSeconds       *prometheus.HistogramVec
	RegistryFetchErrors *prometheus.CounterVec
}

// NewMetrics creates the proxy counters and registers them when a registerer is given.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	allMetrics := &Metrics{
		DigestMismatches: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cache_digest_mismatch_total",
			Help: "Cached chunks whose bytes did not match their digest and were fetched from the registry instead",
		}),
		LookupSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "proxy_cache_lookup_seconds",
			Help:    "Time one chunk spent asking the cluster cache, by outcome: hit, miss, mismatch or error",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 14),
		}, []string{"outcome"}),
		RegistryFetchErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_fetch_errors_total",
			Help: "Registry fetches that failed, by reason: http 429, another http status, connection refused or other",
		}, []string{"reason"}),
	}

	// The reasons a registry is expected to give start at zero, so a reader can
	// tell a proxy that saw no failure from one that does not count them
	for _, reason := range []string{"http 429", "connection refused", "other"} {
		allMetrics.RegistryFetchErrors.WithLabelValues(reason)
	}

	if registerer != nil {
		registerer.MustRegister(allMetrics.DigestMismatches, allMetrics.LookupSeconds, allMetrics.RegistryFetchErrors)
	}
	return allMetrics
}

var unexpectedStatus = regexp.MustCompile(`unexpected status (\d{3})`)

// registryFailureReason reduces the error of one failed registry fetch to the
// reason an operator acts on. A registry that throttles answers with status
// 429, one that sheds load refuses the connection, and everything else is
// grouped under its status code or as other.
func registryFailureReason(err error) string {
	message := err.Error()
	if strings.Contains(message, "connection refused") {
		return "connection refused"
	}
	if status := unexpectedStatus.FindStringSubmatch(message); status != nil {
		return "http " + status[1]
	}
	return "other"
}

// recordRegistryFailure counts one failed registry fetch. A fetch the proxy
// cancelled itself, because the cache answered first or the caller gave up,
// is not a registry failure and is left out.
func (allMetrics *Metrics) recordRegistryFailure(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	allMetrics.RegistryFetchErrors.WithLabelValues(registryFailureReason(err)).Inc()
}
