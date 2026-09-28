package sim

import "strconv"

// RunLabel names the inputs of one run the way the result CSVs record them.
type RunLabel struct {
	Trace       string
	WorkingSets string
	Policy      string
	MapStrategy string
	Placement   string
}

// ResultHeader names the columns of the campaign output CSV, in the same
// order ResultRow emits them.
func ResultHeader() []string {
	return []string{
		"trace", "working_sets", "policy", "capacity_bytes", "nodes",
		"keepalive_ms", "concurrency", "map_strategy", "placement",
		"cluster_cache", "registry_chunk_ms", "cache_chunk_ms", "seed",
		"invocations", "cold_starts", "seed_cold_starts", "warm_starts",
		"local_bytes", "cluster_hit_bytes", "registry_bytes",
		"cluster_hit_rate_bytes", "evictions", "peak_stored_bytes",
		"nodes_used", "registry_requests", "peak_cache_rps",
	}
}

// ResultRow flattens one simulation run into CSV fields. The byte hit rate
// covers only cluster-eligible traffic, meaning chunks not already on the
// local node, so it isolates what the cluster cache contributed.
func ResultRow(label RunLabel, cfg Config, capacity uint64, r Result) []string {
	eligible := r.ClusterHitBytes + r.RegistryBytes
	rate := 0.0
	if eligible > 0 {
		rate = float64(r.ClusterHitBytes) / float64(eligible)
	}

	registryChunkMs, cacheChunkMs := costColumns(cfg)

	return []string{
		label.Trace,
		label.WorkingSets,
		label.Policy,
		strconv.FormatUint(capacity, 10),
		strconv.Itoa(cfg.Nodes),
		strconv.FormatInt(cfg.KeepAliveMs, 10),
		strconv.Itoa(cfg.Concurrency),
		label.MapStrategy,
		label.Placement,
		clusterCacheColumn(cfg),
		registryChunkMs,
		cacheChunkMs,
		strconv.FormatInt(cfg.Seed, 10),
		strconv.Itoa(r.Invocations),
		strconv.Itoa(r.ColdStarts),
		strconv.Itoa(r.SeedColdStarts),
		strconv.Itoa(r.WarmStarts),
		strconv.FormatUint(r.LocalBytes, 10),
		strconv.FormatUint(r.ClusterHitBytes, 10),
		strconv.FormatUint(r.RegistryBytes, 10),
		strconv.FormatFloat(rate, 'f', 4, 64),
		strconv.Itoa(r.Evictions),
		strconv.FormatUint(r.PeakStoredBytes, 10),
		strconv.Itoa(r.NodesUsed),
		strconv.Itoa(r.RegistryRequests()),
		strconv.Itoa(r.PeakCacheRPS),
	}
}

// ClassHeader names the columns of the per-frequency-class CSV.
func ClassHeader() []string {
	return []string{
		"trace", "working_sets", "policy", "capacity_bytes", "nodes",
		"placement", "map_strategy", "seed", "cluster_cache",
		"registry_chunk_ms", "cache_chunk_ms", "class", "apps", "invocations",
		"cold_starts", "cold_share", "local_bytes", "cache_bytes",
		"registry_bytes", "cold_p50_ms", "cold_p95_ms", "cold_p99_ms",
		"resp_p50_ms", "resp_p95_ms", "resp_p99_ms", "resp_p999_ms",
		"resp_p9999_ms",
	}
}

// ClassRows flattens one run into one row per frequency class; the latency
// columns stay empty when the run carried no cost model.
func ClassRows(label RunLabel, cfg Config, capacity uint64, r Result) [][]string {
	registryChunkMs, cacheChunkMs := costColumns(cfg)

	rows := make([][]string, 0, ClassCount)
	for class, stats := range r.Classes {
		share := 0.0
		if stats.Invocations > 0 {
			share = float64(stats.ColdStarts) / float64(stats.Invocations)
		}

		row := []string{
			label.Trace,
			label.WorkingSets,
			label.Policy,
			strconv.FormatUint(capacity, 10),
			strconv.Itoa(cfg.Nodes),
			label.Placement,
			label.MapStrategy,
			strconv.FormatInt(cfg.Seed, 10),
			clusterCacheColumn(cfg),
			registryChunkMs,
			cacheChunkMs,
			ClassLabel(class),
			strconv.Itoa(stats.Apps),
			strconv.Itoa(stats.Invocations),
			strconv.Itoa(stats.ColdStarts),
			strconv.FormatFloat(share, 'f', 6, 64),
			strconv.FormatUint(stats.LocalBytes, 10),
			strconv.FormatUint(stats.CacheBytes, 10),
			strconv.FormatUint(stats.RegistryBytes, 10),
		}
		row = append(row, quantileColumns(stats.ColdLatency, 0.5, 0.95, 0.99)...)
		row = append(row, quantileColumns(stats.Response, 0.5, 0.95, 0.99, 0.999, 0.9999)...)
		rows = append(rows, row)
	}
	return rows
}

// clusterCacheColumn says whether this run had a cluster tier at all
func clusterCacheColumn(cfg Config) string {
	if cfg.NoClusterCache {
		return "no"
	}
	return "yes"
}

// costColumns prints the two per-chunk coefficients, or nothing when the run
// charged no cost model and so reports fetch counts rather than times
func costColumns(cfg Config) (string, string) {
	if cfg.Cost == nil {
		return "", ""
	}
	return strconv.FormatFloat(cfg.Cost.RegistryChunkMs, 'f', -1, 64),
		strconv.FormatFloat(cfg.Cost.CacheChunkMs, 'f', -1, 64)
}

// quantileColumns reads the requested quantiles off a histogram, leaving the
// columns empty when the run recorded none
func quantileColumns(l *Latency, quantiles ...float64) []string {
	columns := make([]string, 0, len(quantiles))
	for _, q := range quantiles {
		if l == nil {
			columns = append(columns, "")
			continue
		}
		columns = append(columns, strconv.FormatInt(l.Quantile(q), 10))
	}
	return columns
}
