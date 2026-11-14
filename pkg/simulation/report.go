package simulation

import "strconv"

// Writer for building the csv rows of simulation results

// summary labels to recognize what run this rows refer to
type RunLabel struct {
	Trace       string // which traces were used
	WorkingSets string // what set of images were used
	Policy      string // ... eviction were used
	MapStrategy string // how each function is pared with one of our images
	Placement   string // pod placement strat
}

// simulation result headers
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

// format the simulations results to a single csv row
// also calculates the cluster hit rate based on the simulation result (since we have everything we need for it)
// for data that wasnt already present on the node
func ResultRow(label RunLabel, cfg Config, capacity uint64, simulationResult Result) []string {
	eligible := simulationResult.ClusterHitBytes + simulationResult.RegistryBytes
	rate := 0.0
	if eligible > 0 {
		rate = float64(simulationResult.ClusterHitBytes) / float64(eligible)
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
		strconv.Itoa(simulationResult.Invocations),
		strconv.Itoa(simulationResult.ColdStarts),
		strconv.Itoa(simulationResult.SeedColdStarts),
		strconv.Itoa(simulationResult.WarmStarts),
		strconv.FormatUint(simulationResult.LocalBytes, 10),
		strconv.FormatUint(simulationResult.ClusterHitBytes, 10),
		strconv.FormatUint(simulationResult.RegistryBytes, 10),
		strconv.FormatFloat(rate, 'f', 4, 64),
		strconv.Itoa(simulationResult.Evictions),
		strconv.FormatUint(simulationResult.PeakStoredBytes, 10),
		strconv.Itoa(simulationResult.NodesUsed),
		strconv.Itoa(simulationResult.RegistryRequests()),
		strconv.Itoa(simulationResult.PeakCacheRPS),
	}
}

// csv header for the function call metrics by frequency classes
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

// format the simulation run results into one row for each of the 6 frequency classes
// will be blank if no cost model estimation was specified
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

func clusterCacheColumn(cfg Config) string {
	if cfg.NoClusterCache {
		return "no"
	}
	return "yes"
}

func costColumns(cfg Config) (string, string) {
	if cfg.Cost == nil {
		return "", ""
	}
	return strconv.FormatFloat(cfg.Cost.RegistryChunkMs, 'f', -1, 64),
		strconv.FormatFloat(cfg.Cost.CacheChunkMs, 'f', -1, 64)
}

// returns a list of waiting times from a run based on the specific quantiles
// blank if nothing was recorded
func quantileColumns(recordedLatency *Latency, quantiles ...float64) []string {
	columns := make([]string, 0, len(quantiles))
	for _, currQuantile := range quantiles {
		if recordedLatency == nil {
			columns = append(columns, "")
			continue
		}
		columns = append(columns, strconv.FormatInt(recordedLatency.Quantile(currQuantile), 10))
	}
	return columns
}
