// The main function to start the cluster cache simulation in /pkg/sim
// Takes the selected trace(s) and simulates it again a simulated cluster with the cluster cache
// Results include metrics about where image data were fetched from (local, cluter cache, remote registry) when not present on the assigned node
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
	tracePath := flag.String("trace", "", "path to a trace CSV")
	workingSetsPath := flag.String("working-sets", "analysis/results/chunks-startup.json", "per-image startup chunk list")
	imagesFlag := flag.String("images", "", "comma-separated list of images that will be used, default is all catalog images")
	mapStrategy := flag.String("map", "volume", "app-to-image mapping strategy, allowed volume (spread by how frequent a function appear in the trace) or hash (random)")
	seedsFlag := flag.String("seeds", "1", "comma-separated seeds")
	assignPath := flag.String("assign", "", "JSON file that maps each function to an image, alternative to mapStrategy (and seeds for hash strat)")
	nodes := flag.Int("nodes", 3, "number of nodes")
	keepAliveMs := flag.Int64("keepalive-ms", 60000, "pod keep-alive window (ms)")
	concurrency := flag.Int("concurrency", 100, "amount of function calls a single pod can handle")
	capacitiesFlag := flag.String("capacities", "0", "comma-separated cluster cache capacities in bytes, 0 means unlimited")
	policiesFlag := flag.String("policies", "lru", "comma-separated eviction policies: lru,lru2,size,refcount,gdsf")
	placementFlag := flag.String("placement", "packed", "pod placement policy: packed (start new pod on least busy node first) or random")
	outPath := flag.String("out", "", "output CSV path")
	classesOut := flag.String("classes-out", "", "output path for csv with function per-frequency-class results")
	timelineOut := flag.String("timeline-out", "", "output csv path for chunk fetch origin metric")
	timelineBucketMs := flag.Int64("timeline-bucket-ms", 3600000, "timeline bucket width in ms")
	latencyOut := flag.String("latency-out", "", "output csv path for per-run response-time quantiles")
	warmFloorMs := flag.Int64("warm-floor-ms", 2050, "the time (ms) an functions needs to start on a warm pod where all data is already there")
	warmFloorsPath := flag.String("warm-floors", "", "JSON file mapping image name to a per-image warm floor in ms => overrides -warm-floor-ms for those images")
	registryChunkMs := flag.Float64("registry-chunk-ms", 22.56, "cost (ms) of one chunk fetched from the registry")
	cacheChunkMs := flag.Float64("cache-chunk-ms", 1.84, "cost (ms) of one chunk served by the cluster cache")
	noClusterCache := flag.Bool("no-cluster-cache", false, "disables any requests to the cluster cache (for comparability resons")
	flag.Parse()

	// require the trace file path
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

// seed, capacity & policy input flags/params are lists of possibly multiple values
// => a job is a combination of one value provided for each of the three params
type job struct {
	seed     int64
	capacity uint64
	policy   string
}

// all params we pass to the whole simulation sweep
// job params will be split up in the runSweep func
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

// loads the input files once before splitting the input params into variations (jobs) in order to run them in parallel
// the respective job results are one row in the result csv file
func runSweep(opts sweepOptions) error {

	/* input validation */
	catalog, err := sim.LoadCatalog(opts.workingSetsPath)
	if err != nil {
		return fmt.Errorf("error while trying to load image catalog: %w", err)
	}

	images, err := selectImages(catalog, opts.images)
	if err != nil {
		return err
	}

	funcInvocations, err := sim.LoadTrace(opts.tracePath)
	if err != nil {
		return fmt.Errorf("error while trying to load trace: %w", err)
	}

	capacities, err := parseUints(opts.capacities)
	if err != nil {
		return fmt.Errorf("error wile reading invalid value for -capacities: %w", err)
	}
	seeds, err := parseInts(opts.seeds)
	if err != nil {
		return fmt.Errorf("error while reading invalid value for -seeds: %w", err)
	}
	policies := strings.Split(opts.policies, ",")

	var warmFloors []int64
	if opts.warmFloorsPath != "" {
		warmFloors, err = loadWarmFloors(opts.warmFloorsPath, catalog)
		if err != nil {
			return err
		}
	}

	// open the output csv
	outCsvFile := os.Stdout
	if opts.outPath != "" {
		newCsvFile, err := os.Create(opts.outPath)
		if err != nil {
			return fmt.Errorf("error while trying to create output csv file: %w", err)
		}
		defer newCsvFile.Close()
		outCsvFile = newCsvFile
	}

	writer := csv.NewWriter(outCsvFile)
	defer writer.Flush()
	if err := writer.Write(sim.ResultHeader()); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	refCounts := catalog.RefCounts()
	traceName := trimTraceName(opts.tracePath)
	workingSets := workingSetsLabel(opts.workingSetsPath)

	// create a job for each combination of (seed, capacity, eviction policy)
	var jobs []job
	for _, seed := range seeds {
		for _, capacity := range capacities {
			for _, policyName := range policies {
				jobs = append(jobs, job{seed: seed, capacity: capacity, policy: policyName})
			}
		}
	}

	// assign images to each trace function once per seed
	assignBySeed := map[int64][]int32{}
	for _, seed := range seeds {
		assign, err := sim.AssignImages(funcInvocations, images, opts.mapStrategy, seed)
		if err != nil {
			return fmt.Errorf("error while assign images to trace functions: %w", err)
		}
		assignBySeed[seed] = assign
	}

	// if a explicit mapping for image - trace function is given via file, we use that mapping
	// also we only need a single seed in that case
	if opts.assignPath != "" {
		fixed, err := readAssignment(opts.assignPath, funcInvocations, catalog)
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

	// only one job is allowed when -timeline-out is specified
	if opts.timelineOut != "" && len(jobs) != 1 {
		return fmt.Errorf("for -timeline-out only one capacity, policy, and seed value is allowed, but got %d runs", len(jobs))
	}

	// for each job create an empty slot for its row, settings, errors & result
	rows := make([][]string, len(jobs))
	cfgs := make([]sim.Config, len(jobs))
	results := make([]sim.Result, len(jobs))
	errs := make([]error, len(jobs))
	next := make(chan int, len(jobs))

	// queue that marks todo jobs
	// also enumerate the jobs
	for i := range jobs {
		next <- i
	}
	// close queue in order for the workers to know when nothing is left to do
	close(next)

	// we run this with 8 parallel workers that run the job variations
	workers := 8
	if workers > len(jobs) {
		workers = len(jobs)
	}

	// setup each job with the respective settings and run them
	var waitGrp sync.WaitGroup
	for currWorker := 0; currWorker < workers; currWorker++ {
		waitGrp.Add(1)
		go func() {
			defer waitGrp.Done()
			for i := range next {
				currJob := jobs[i]
				policy, err := sim.NewPolicy(currJob.policy, refCounts)
				if err != nil {
					errs[i] = fmt.Errorf("error while building policy: %w", err)
					continue
				}

				cfg := sim.Config{
					Nodes:          opts.nodes,
					CapacityBytes:  currJob.capacity,
					KeepAliveMs:    opts.keepAliveMs,
					Concurrency:    opts.concurrency,
					Seed:           currJob.seed,
					Placement:      opts.placement,
					NoClusterCache: opts.noClusterCache,
				}

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

				res, err := sim.Simulate(cfg, catalog, assignBySeed[currJob.seed], funcInvocations, policy)
				if err != nil {
					errs[i] = fmt.Errorf("error during a simulation run: %w", err)
					continue
				}
				results[i] = res

				// annotate the job variation in the csv
				label := sim.RunLabel{
					Trace:       traceName,
					WorkingSets: workingSets,
					Policy:      currJob.policy,
					MapStrategy: opts.mapStrategy,
					Placement:   opts.placement,
				}

				// set the job results for the corresponding result row of this job
				// the row itself is written to file later...
				rows[i] = sim.ResultRow(label, cfg, currJob.capacity, res)
			}
		}()
	}
	waitGrp.Wait()

	for i, row := range rows {
		if errs[i] != nil {
			return errs[i]
		}
		if err := writer.Write(row); err != nil {
			return fmt.Errorf("error while writing result row: %w", err)
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

// Write the result csv for the specific function invocation-frequency class
// => helps separate results in case a few very busy applications mask the results of the majority of not so often invoked ones
func writeClasses(path, trace, workingSets string, opts sweepOptions, jobs []job, cfgs []sim.Config, results []sim.Result) error {
	outCsvFile, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("error while creating func freq class result file: %w", err)
	}
	defer outCsvFile.Close()

	outCsvFileWriter := csv.NewWriter(outCsvFile)
	defer outCsvFileWriter.Flush()
	if err := outCsvFileWriter.Write(sim.ClassHeader()); err != nil {
		return fmt.Errorf("failed to write func freq class result csv  header: %w", err)
	}

	// write results for each class
	for i, res := range results {
		label := sim.RunLabel{
			Trace:       trace,
			WorkingSets: workingSets,
			Policy:      jobs[i].policy,
			MapStrategy: opts.mapStrategy,
			Placement:   opts.placement,
		}

		for _, row := range sim.ClassRows(label, cfgs[i], jobs[i].capacity, res) {
			if err := outCsvFileWriter.Write(row); err != nil {
				return fmt.Errorf("error while trying to write func freq class row: %w", err)
			}
		}
	}
	return nil
}

// write the latency results for -latency-out file for each job in a separate row
func writeLatency(path, trace, workingSets string, opts sweepOptions, jobs []job, results []sim.Result) error {
	latencyOutCsvFile, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create latency file: %w", err)
	}
	defer latencyOutCsvFile.Close()

	latencyOutFileWriter := csv.NewWriter(latencyOutCsvFile)
	defer latencyOutFileWriter.Flush()

	// csv header/columns
	header := []string{
		"trace", "working_sets", "cluster_cache", "policy", "capacity_bytes", "nodes",
		"placement", "map_strategy", "seed", "registry_chunk_ms", "cache_chunk_ms",
		"invocations", "cold_starts", "warm_starts",
		"cold_p50_ms", "cold_p95_ms", "cold_p99_ms",
		"p50_ms", "p95_ms", "p99_ms", "p999_ms", "p9999_ms", "max_ms",
		"affected_share",
	}

	if err := latencyOutFileWriter.Write(header); err != nil {
		return fmt.Errorf("error while writing latency headers: %w", err)
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

		// amount of function calls that had to cold start on a pod instead of being placed on a warm pod
		// => explains in how many cases the cluster cache can even help, because being placed on a warm pod is cheap regardless
		// of the cluster cache
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
		if err := latencyOutFileWriter.Write(row); err != nil {
			return fmt.Errorf("failed to write latency row: %w", err)
		}
	}

	return nil
}

// write the results in the -timeline-out file => one row per time step, which is in our experiment 1 hr
// contains how many bytes came for the current totals since the start from local node cache, cluster cache and remote registry
func writeTimeline(path string, points []sim.TimelinePoint) error {
	timelineOutCsvFile, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("error while trying to create timeline output file: %w", err)
	}
	defer timelineOutCsvFile.Close()

	timelineOutFileWriter := csv.NewWriter(timelineOutCsvFile)
	defer timelineOutFileWriter.Flush()
	// write csv header
	if err := timelineOutFileWriter.Write([]string{"at_ms", "registry_bytes", "cluster_hit_bytes", "local_bytes"}); err != nil {
		return fmt.Errorf("error while writing timeline file header: %w", err)
	}
	for _, currPoint := range points {
		row := []string{
			strconv.FormatInt(currPoint.AtMs, 10),
			strconv.FormatUint(currPoint.RegistryBytes, 10),
			strconv.FormatUint(currPoint.ClusterHitBytes, 10),
			strconv.FormatUint(currPoint.LocalBytes, 10),
		}
		if err := timelineOutFileWriter.Write(row); err != nil {
			return fmt.Errorf("failed to write timeline row: %w", err)
		}
	}
	return nil
}

// returns the specified image (via -images) into a list of images the simulation run can use
// it returns it as a list of positions that the corresponding images have in the image catalog
func selectImages(catalog *sim.Catalog, requested string) ([]int32, error) {
	if requested != "" {

		// split comma separated list of image names
		names := strings.Split(requested, ",")
		images := make([]int32, 0, len(names))
		// look up their position in the image catalog that contains all of them
		for _, name := range names {
			idx, ok := catalog.Index(name)
			if !ok {
				return nil, fmt.Errorf("image %s not in catalog", name)
			}
			images = append(images, idx)
		}
		return images, nil
	}

	// no -images names were given => take all from image catalog and return their positions
	names := append([]string{}, catalog.Images...)
	sort.Strings(names)
	images := make([]int32, len(names))
	for i, name := range names {
		idx, _ := catalog.Index(name)
		images[i] = idx
	}

	return images, nil
}

/* helper */
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

// parse comma separated list of string unsigned integers into list of integers (e.g. used for -capacities)
func parseUints(csvList string) ([]uint64, error) {
	var out []uint64
	for _, currString := range strings.Split(csvList, ",") {
		value, err := strconv.ParseUint(strings.TrimSpace(currString), 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// ...same as parseUints() but with ints
func parseInts(csvList string) ([]int64, error) {
	var out []int64
	for _, currString := range strings.Split(csvList, ",") {
		value, err := strconv.ParseInt(strings.TrimSpace(currString), 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// parses the -assign file (json) into a list that maps each function in the trace to one of our images
func readAssignment(path string, trace *sim.Trace, catalog *sim.Catalog) ([]int32, error) {
	assignFile, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading image assignment: %w", err)
	}

	named := map[string]string{}
	if err := json.Unmarshal(assignFile, &named); err != nil {
		return nil, fmt.Errorf("failed to parse image assignment: %w", err)
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

// parses the -warm-floors file (again json) and creates a list of images (via their catalog index) with the warm startup time from the file
func loadWarmFloors(path string, catalog *sim.Catalog) ([]int64, error) {
	startupTimeFile, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading warm floors file: %w", err)
	}

	named := map[string]int64{}
	if err := json.Unmarshal(startupTimeFile, &named); err != nil {
		return nil, fmt.Errorf("error while trying to parse warm floors file: %w", err)
	}

	floors := make([]int64, len(catalog.Images))
	for name, ms := range named {
		idx, ok := catalog.Index(name)
		if !ok {
			return nil, fmt.Errorf("corresponding warm floor image %s not in catalog", name)
		}
		floors[idx] = ms
	}
	return floors, nil
}
