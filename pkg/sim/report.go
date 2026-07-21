package sim

import "strconv"

// ResultHeader names the columns of the campaign output CSV, in the same
// order ResultRow emits them.
func ResultHeader() []string {
	return []string{
		"trace", "policy", "capacity_bytes", "nodes", "keepalive_ms",
		"concurrency", "map_strategy", "seed", "invocations", "cold_starts",
		"seed_cold_starts", "warm_starts", "local_bytes", "cluster_hit_bytes",
		"registry_bytes", "cluster_hit_rate_bytes", "evictions", "peak_stored_bytes",
	}
}

// ResultRow flattens one simulation run into CSV fields. The byte hit rate
// covers only cluster-eligible traffic, meaning chunks not already on the
// local node, so it isolates what the cluster cache contributed.
func ResultRow(trace, policy, mapStrategy string, cfg Config, capacity uint64, r Result) []string {
	eligible := r.ClusterHitBytes + r.RegistryBytes
	rate := 0.0
	if eligible > 0 {
		rate = float64(r.ClusterHitBytes) / float64(eligible)
	}

	return []string{
		trace,
		policy,
		strconv.FormatUint(capacity, 10),
		strconv.Itoa(cfg.Nodes),
		strconv.FormatInt(cfg.KeepAliveMs, 10),
		strconv.Itoa(cfg.Concurrency),
		mapStrategy,
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
	}
}
