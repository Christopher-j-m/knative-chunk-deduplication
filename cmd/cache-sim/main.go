// Command cache-sim replays a serverless invocation trace against a modeled
// cluster chunk cache and reports where every startup byte came from. It
// sweeps the cross product of capacities, eviction policies, and seeds so one
// invocation produces a full comparison table.
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"nydus-cluster-cache/pkg/sim"
)

func main() {
	tracePath := flag.String("trace", "", "canonical trace CSV (at_ms,app,dur_ms)")
	workingSetsPath := flag.String("working-sets", "analysis/results/chunks-startup.json", "per-image startup chunk inventory")
	imagesFlag := flag.String("images", "", "comma-separated image subset, default all images in the catalog")
	mapStrategy := flag.String("map", "volume", "app-to-image mapping strategy: volume or hash")
	seedsFlag := flag.String("seeds", "1", "comma-separated seeds")
	assignPath := flag.String("assign", "", "JSON file with an explicit app-to-image map, overriding -map and -seeds")
	nodes := flag.Int("nodes", 3, "number of nodes")
	keepAliveMs := flag.Int64("keepalive-ms", 60000, "pod keep-alive window in ms")
	concurrency := flag.Int("concurrency", 100, "per-pod concurrency limit")
	capacitiesFlag := flag.String("capacities", "0", "comma-separated cluster cache capacities in bytes, 0 means unbounded")
	policiesFlag := flag.String("policies", "lru", "comma-separated eviction policies: lru,lru2,size,refcount,gdsf")
	placementFlag := flag.String("placement", "packed", "pod placement policy: packed or random")
	outPath := flag.String("out", "", "output CSV path, stdout when empty")
	classesOut := flag.String("classes-out", "", "write per-frequency-class results to this CSV")
	timelineOut := flag.String("timeline-out", "", "write cumulative fetch totals over time to this CSV, single run only")
	timelineBucketMs := flag.Int64("timeline-bucket-ms", 3600000, "timeline bucket width in ms")
	latencyOut := flag.String("latency-out", "", "write per-run response-time quantiles to this CSV; enables the cost model")
	warmFloorMs := flag.Int64("warm-floor-ms", 2050, "application startup floor in ms, from the measured warm cells")
	warmFloorsPath := flag.String("warm-floors", "", "JSON file mapping image name to a per-image warm floor in ms, overriding -warm-floor-ms for those images")
	registryChunkMs := flag.Float64("registry-chunk-ms", 22.56, "cost of one chunk fetched from the registry, in ms")
	cacheChunkMs := flag.Float64("cache-chunk-ms", 1.84, "cost of one chunk served by the cluster cache, in ms")
	noClusterCache := flag.Bool("no-cluster-cache", false, "model the uncached configuration: node-local caching only")
	flag.Parse()

	if *tracePath == "" {
		log.Fatal("missing -trace")
	}

	if err := runSweep(sweepOptions{
		tracePath:        *tracePath,
		workingSetsPath:  *workingSetsPath,
		images:           *imagesFlag,
		mapStrategy:      *mapStrategy,
		seeds:            *seedsFlag,
		assignPath:       *assignPath,
		nodes:            *nodes,
		keepAliveMs:      *keepAliveMs,
		concurrency:      *concurrency,
		capacities:       *capacitiesFlag,
		policies:         *policiesFlag,
		placement:        *placementFlag,
		outPath:          *outPath,
		classesOut:       *classesOut,
		timelineOut:      *timelineOut,
		timelineBucketMs: *timelineBucketMs,
		latencyOut:       *latencyOut,
		warmFloorMs:      *warmFloorMs,
		warmFloorsPath:   *warmFloorsPath,
		registryChunkMs:  *registryChunkMs,
		cacheChunkMs:     *cacheChunkMs,
		noClusterCache:   *noClusterCache,
	}); err != nil {
		log.Fatal(err)
	}
}

// job is one run of the sweep: a capacity, a policy and a mapping seed
type job struct {
	seed     int64
	capacity uint64
	policy   string
}

type sweepOptions struct {
	tracePath        string
	workingSetsPath  string
	images           string
	mapStrategy      string
	seeds            string
	assignPath       string
	nodes            int
	keepAliveMs      int64
	concurrency      int
	capacities       string
	policies         string
	placement        string
	outPath          string
	classesOut       string
	timelineOut      string
	timelineBucketMs int64
	latencyOut       string
	warmFloorMs      int64
	warmFloorsPath   string
	registryChunkMs  float64
	cacheChunkMs     float64
	noClusterCache   bool
}

// runSweep loads the inputs once and replays the trace for every combination
// of capacity, policy, and seed, appending one CSV row per run.
func runSweep(opts sweepOptions) error {
	catalog, err := sim.LoadCatalog(opts.workingSetsPath)
	if err != nil {
		return fmt.Errorf("failed to load catalog: %w", err)
	}

	images, err := selectImages(catalog, opts.images)
	if err != nil {
		return err
	}

	invs, err := sim.LoadTrace(opts.tracePath)
	if err != nil {
		return fmt.Errorf("failed to load trace: %w", err)
	}

	capacities, err := parseUints(opts.capacities)
	if err != nil {
		return fmt.Errorf("bad -capacities: %w", err)
	}
	seeds, err := parseInts(opts.seeds)
	if err != nil {
		return fmt.Errorf("bad -seeds: %w", err)
	}
	policies := strings.Split(opts.policies, ",")

	var warmFloors []int64
	if opts.warmFloorsPath != "" {
		warmFloors, err = loadWarmFloors(opts.warmFloorsPath, catalog)
		if err != nil {
			return err
		}
	}

	out := os.Stdout
	if opts.outPath != "" {
		f, err := os.Create(opts.outPath)
		if err != nil {
			return fmt.Errorf("failed to create output: %w", err)
		}
		defer f.Close()
		out = f
	}

	writer := csv.NewWriter(out)
	defer writer.Flush()
	if err := writer.Write(sim.ResultHeader()); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	refCounts := catalog.RefCounts()
	traceName := trimTraceName(opts.tracePath)
	workingSets := workingSetsLabel(opts.workingSetsPath)

	// enumerate every run up front so workers can chew through them in
	// parallel while the output keeps a stable row order
	var jobs []job
	for _, seed := range seeds {
		for _, capacity := range capacities {
			for _, policyName := range policies {
				jobs = append(jobs, job{seed: seed, capacity: capacity, policy: policyName})
			}
		}
	}

	// each seed reuses one assignment, computed once
	assignBySeed := map[int64][]int32{}
	for _, seed := range seeds {
		assign, err := sim.AssignImages(invs, images, opts.mapStrategy, seed)
		if err != nil {
			return fmt.Errorf("failed to assign images: %w", err)
		}
		assignBySeed[seed] = assign
	}

	// a caller-supplied mapping replaces the generated ones outright, which
	// also makes the seeds meaningless: every seed would run the same job
	if opts.assignPath != "" {
		fixed, err := readAssignment(opts.assignPath, invs, catalog)
		if err != nil {
			return err
		}
		seeds = seeds[:1]
		assignBySeed = map[int64][]int32{seeds[0]: fixed}

		kept := jobs[:0]
		for _, j := range jobs {
			if j.seed == seeds[0] {
				kept = append(kept, j)
			}
		}
		jobs = kept
	}

	if opts.timelineOut != "" && len(jobs) != 1 {
		return fmt.Errorf("-timeline-out needs exactly one capacity, policy, and seed, got %d runs", len(jobs))
	}

	rows := make([][]string, len(jobs))
	cfgs := make([]sim.Config, len(jobs))
	results := make([]sim.Result, len(jobs))
	errs := make([]error, len(jobs))
	next := make(chan int, len(jobs))
	for i := range jobs {
		next <- i
	}
	close(next)

	workers := 8
	if workers > len(jobs) {
		workers = len(jobs)
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				j := jobs[i]
				policy, err := sim.NewPolicy(j.policy, refCounts)
				if err != nil {
					errs[i] = fmt.Errorf("failed to build policy: %w", err)
					continue
				}

				cfg := sim.Config{
					Nodes:          opts.nodes,
					CapacityBytes:  j.capacity,
					KeepAliveMs:    opts.keepAliveMs,
					Concurrency:    opts.concurrency,
					Seed:           j.seed,
					Placement:      opts.placement,
					NoClusterCache: opts.noClusterCache,
				}
				// the cost model is only built when the caller asks for
				// response times, so every existing capacity run keeps its
				// fetch-accounting-only behaviour
				if opts.latencyOut != "" {
					cfg.Cost = &sim.CostModel{
						WarmFloorMs:        warmFloors,
						DefaultWarmFloorMs: opts.warmFloorMs,
						RegistryChunkMs:    opts.registryChunkMs,
						CacheChunkMs:       opts.cacheChunkMs,
					}
				}
				if opts.timelineOut != "" {
					cfg.TimelineBucketMs = opts.timelineBucketMs
				}
				cfgs[i] = cfg

				res, err := sim.Simulate(cfg, catalog, assignBySeed[j.seed], invs, policy)
				if err != nil {
					errs[i] = fmt.Errorf("simulation failed: %w", err)
					continue
				}
				results[i] = res

				label := sim.RunLabel{
					Trace:       traceName,
					WorkingSets: workingSets,
					Policy:      j.policy,
					MapStrategy: opts.mapStrategy,
					Placement:   opts.placement,
				}
				rows[i] = sim.ResultRow(label, cfg, j.capacity, res)
			}
		}()
	}
	wg.Wait()

	for i, row := range rows {
		if errs[i] != nil {
			return errs[i]
		}
		if err := writer.Write(row); err != nil {
			return fmt.Errorf("failed to write row: %w", err)
		}
	}

	if opts.classesOut != "" {
		if err := writeClasses(opts.classesOut, traceName, workingSets, opts, jobs, cfgs, results); err != nil {
			return err
		}
	}

	if opts.latencyOut != "" {
		if err := writeLatency(opts.latencyOut, traceName, workingSets, opts, jobs, results); err != nil {
			return err
		}
	}

	if opts.timelineOut != "" {
		if err := writeTimeline(opts.timelineOut, results[0].Timeline); err != nil {
			return err
		}
	}
	return nil
}

// writeClasses reports what the cache did for each invocation-frequency
// class separately, so a handful of busy applications cannot hide what
// happens to the many rarely invoked ones
func writeClasses(path, trace, workingSets string, opts sweepOptions, jobs []job, cfgs []sim.Config, results []sim.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create classes file: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write(sim.ClassHeader()); err != nil {
		return fmt.Errorf("failed to write classes header: %w", err)
	}

	for i, res := range results {
		label := sim.RunLabel{
			Trace:       trace,
			WorkingSets: workingSets,
			Policy:      jobs[i].policy,
			MapStrategy: opts.mapStrategy,
			Placement:   opts.placement,
		}
		for _, row := range sim.ClassRows(label, cfgs[i], jobs[i].capacity, res) {
			if err := w.Write(row); err != nil {
				return fmt.Errorf("failed to write classes row: %w", err)
			}
		}
	}
	return nil
}

// writeLatency reports what the replay did to the requester rather than to the
// registry: one row per run with the response-time distribution over every
// invocation in the trace, warm and cold alike.
func writeLatency(path, trace, workingSets string, opts sweepOptions, jobs []job, results []sim.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create latency file: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"trace", "working_sets", "cluster_cache", "policy", "capacity_bytes", "nodes",
		"placement", "map_strategy", "seed", "registry_chunk_ms", "cache_chunk_ms",
		"invocations", "cold_starts", "warm_starts",
		"cold_p50_ms", "cold_p95_ms", "cold_p99_ms",
		"p50_ms", "p95_ms", "p99_ms", "p999_ms", "p9999_ms", "max_ms",
		"affected_share",
	}
	if err := w.Write(header); err != nil {
		return fmt.Errorf("failed to write latency header: %w", err)
	}

	clusterCache := "yes"
	if opts.noClusterCache {
		clusterCache = "no"
	}
	registryChunkMs := strconv.FormatFloat(opts.registryChunkMs, 'f', -1, 64)
	cacheChunkMs := strconv.FormatFloat(opts.cacheChunkMs, 'f', -1, 64)

	for i, res := range results {
		if res.Latency == nil {
			continue
		}

		// an invocation is affected when it waited on a cold start rather than
		// being served straight away. The share is small enough that the
		// difference between configurations lives above the 99th percentile,
		// which is why the deeper quantiles are reported alongside it
		affected := 0.0
		if res.Invocations > 0 {
			affected = float64(res.ColdStarts) / float64(res.Invocations)
		}

		row := []string{
			trace,
			workingSets,
			clusterCache,
			jobs[i].policy,
			strconv.FormatUint(jobs[i].capacity, 10),
			strconv.Itoa(opts.nodes),
			opts.placement,
			opts.mapStrategy,
			strconv.FormatInt(jobs[i].seed, 10),
			registryChunkMs,
			cacheChunkMs,
			strconv.Itoa(res.Invocations),
			strconv.Itoa(res.ColdStarts),
			strconv.Itoa(res.WarmStarts),
			strconv.FormatInt(res.ColdLatency.Quantile(0.50), 10),
			strconv.FormatInt(res.ColdLatency.Quantile(0.95), 10),
			strconv.FormatInt(res.ColdLatency.Quantile(0.99), 10),
			strconv.FormatInt(res.Latency.Quantile(0.50), 10),
			strconv.FormatInt(res.Latency.Quantile(0.95), 10),
			strconv.FormatInt(res.Latency.Quantile(0.99), 10),
			strconv.FormatInt(res.Latency.Quantile(0.999), 10),
			strconv.FormatInt(res.Latency.Quantile(0.9999), 10),
			strconv.FormatInt(res.Latency.Quantile(1.00), 10),
			strconv.FormatFloat(affected, 'f', 6, 64),
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("failed to write latency row: %w", err)
		}
	}

	return nil
}

// writeTimeline dumps the hourly cumulative fetch totals of a single run
func writeTimeline(path string, points []sim.TimelinePoint) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create timeline output: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"at_ms", "registry_bytes", "cluster_hit_bytes", "local_bytes"}); err != nil {
		return fmt.Errorf("failed to write timeline header: %w", err)
	}
	for _, p := range points {
		row := []string{
			strconv.FormatInt(p.AtMs, 10),
			strconv.FormatUint(p.RegistryBytes, 10),
			strconv.FormatUint(p.ClusterHitBytes, 10),
			strconv.FormatUint(p.LocalBytes, 10),
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("failed to write timeline row: %w", err)
		}
	}
	return nil
}

// selectImages returns catalog indices for the requested image subset, or
// every image in the catalog sorted by name when none is requested. Sorting
// keeps the volume mapping strategy reproducible across runs regardless of
// how the catalog file happens to list its images; a generated catalog
// names images img000, img001, and so on, so its file order and name order
// already agree
func selectImages(catalog *sim.Catalog, requested string) ([]int32, error) {
	if requested != "" {
		names := strings.Split(requested, ",")
		images := make([]int32, 0, len(names))
		for _, name := range names {
			idx, ok := catalog.Index(name)
			if !ok {
				return nil, fmt.Errorf("image %s not in catalog", name)
			}
			images = append(images, idx)
		}
		return images, nil
	}

	names := append([]string{}, catalog.Images...)
	sort.Strings(names)
	images := make([]int32, len(names))
	for i, name := range names {
		idx, _ := catalog.Index(name)
		images[i] = idx
	}
	return images, nil
}

// workingSetsLabel names the working-set input the way the result tables
// record it: the file's own stem, except a generic working-sets.json takes
// its name from the parent directory instead, since every catalog uses that
// same file name
func workingSetsLabel(path string) string {
	base := filepath.Base(path)
	if base == "working-sets.json" {
		return filepath.Base(filepath.Dir(path))
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func trimTraceName(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, ".csv")
}

func parseUints(csvList string) ([]uint64, error) {
	var out []uint64
	for _, s := range strings.Split(csvList, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func parseInts(csvList string) ([]int64, error) {
	var out []int64
	for _, s := range strings.Split(csvList, ",") {
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// readAssignment resolves an explicit app-to-image mapping written as a flat
// JSON object of app name to image name. Every app in the trace must resolve
// to a cataloged image; a failure on either side is reported with the app
// name so the caller can find the gap in its assignment file
func readAssignment(path string, trace *sim.Trace, catalog *sim.Catalog) ([]int32, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read assignment: %w", err)
	}

	named := map[string]string{}
	if err := json.Unmarshal(raw, &named); err != nil {
		return nil, fmt.Errorf("failed to parse assignment: %w", err)
	}
	if len(named) == 0 {
		return nil, fmt.Errorf("assignment %s maps no apps", path)
	}

	fixed := make([]int32, len(trace.Apps))
	for i, appName := range trace.Apps {
		imageName, ok := named[appName]
		if !ok {
			return nil, fmt.Errorf("assignment has no image for app %s", appName)
		}
		idx, ok := catalog.Index(imageName)
		if !ok {
			return nil, fmt.Errorf("assignment image %s for app %s not in catalog", imageName, appName)
		}
		fixed[i] = idx
	}
	return fixed, nil
}

// loadWarmFloors reads an optional image name to millisecond map and turns it
// into a slice ordered the way the catalog indexes images, so the cost model
// can look a warm floor up by index instead of walking a map on every cold
// start
func loadWarmFloors(path string, catalog *sim.Catalog) ([]int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read warm floors: %w", err)
	}

	named := map[string]int64{}
	if err := json.Unmarshal(raw, &named); err != nil {
		return nil, fmt.Errorf("failed to parse warm floors: %w", err)
	}

	floors := make([]int64, len(catalog.Images))
	for name, ms := range named {
		idx, ok := catalog.Index(name)
		if !ok {
			return nil, fmt.Errorf("warm floor image %s not in catalog", name)
		}
		floors[idx] = ms
	}
	return floors, nil
}
