// Command cache-sim replays a serverless invocation trace against a modeled
// cluster chunk cache and reports where every startup byte came from. It
// sweeps the cross product of capacities, eviction policies, and seeds so one
// invocation produces a full comparison table.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"nydus-cluster-cache/pkg/sim"
)

func main() {
	tracePath := flag.String("trace", "", "canonical trace CSV (at_ms,app,dur_ms)")
	workingSetsPath := flag.String("working-sets", "analysis/results/chunks-startup.json", "per-image startup chunk inventory")
	imagesFlag := flag.String("images", "", "comma-separated image subset, default all images in the working-set file")
	mapStrategy := flag.String("map", "volume", "app-to-image mapping strategy: volume or hash")
	seedsFlag := flag.String("seeds", "1", "comma-separated seeds")
	nodes := flag.Int("nodes", 3, "number of nodes")
	keepAliveMs := flag.Int64("keepalive-ms", 60000, "pod keep-alive window in ms")
	concurrency := flag.Int("concurrency", 100, "per-pod concurrency limit")
	capacitiesFlag := flag.String("capacities", "0", "comma-separated cluster cache capacities in bytes, 0 means unbounded")
	policiesFlag := flag.String("policies", "lru", "comma-separated eviction policies: lru,lru2,size,refcount")
	outPath := flag.String("out", "", "output CSV path, stdout when empty")
	timelineOut := flag.String("timeline-out", "", "write cumulative fetch totals over time to this CSV, single run only")
	timelineBucketMs := flag.Int64("timeline-bucket-ms", 3600000, "timeline bucket width in ms")
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
		nodes:            *nodes,
		keepAliveMs:      *keepAliveMs,
		concurrency:      *concurrency,
		capacities:       *capacitiesFlag,
		policies:         *policiesFlag,
		outPath:          *outPath,
		timelineOut:      *timelineOut,
		timelineBucketMs: *timelineBucketMs,
	}); err != nil {
		log.Fatal(err)
	}
}

type sweepOptions struct {
	tracePath        string
	workingSetsPath  string
	images           string
	mapStrategy      string
	seeds            string
	nodes            int
	keepAliveMs      int64
	concurrency      int
	capacities       string
	policies         string
	outPath          string
	timelineOut      string
	timelineBucketMs int64
}

// runSweep loads the inputs once and replays the trace for every combination
// of capacity, policy, and seed, appending one CSV row per run.
func runSweep(opts sweepOptions) error {
	sets, err := sim.LoadWorkingSets(opts.workingSetsPath)
	if err != nil {
		return fmt.Errorf("failed to load working sets: %w", err)
	}

	images, err := selectImages(sets, opts.images)
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

	refCounts := countImageRefs(sets)
	traceName := trimTraceName(opts.tracePath)

	// enumerate every run up front so workers can chew through them in
	// parallel while the output keeps a stable row order
	type job struct {
		seed     int64
		capacity uint64
		policy   string
	}
	var jobs []job
	for _, seed := range seeds {
		for _, capacity := range capacities {
			for _, policyName := range policies {
				jobs = append(jobs, job{seed: seed, capacity: capacity, policy: policyName})
			}
		}
	}

	// each seed reuses one assignment, computed once
	assignBySeed := map[int64]map[string]string{}
	for _, seed := range seeds {
		assign, err := sim.AssignImages(invs, images, opts.mapStrategy, seed)
		if err != nil {
			return fmt.Errorf("failed to assign images: %w", err)
		}
		assignBySeed[seed] = assign
	}

	if opts.timelineOut != "" && len(jobs) != 1 {
		return fmt.Errorf("-timeline-out needs exactly one capacity, policy, and seed, got %d runs", len(jobs))
	}

	rows := make([][]string, len(jobs))
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
					Nodes:         opts.nodes,
					CapacityBytes: j.capacity,
					KeepAliveMs:   opts.keepAliveMs,
					Concurrency:   opts.concurrency,
					Seed:          j.seed,
				}
				if opts.timelineOut != "" {
					cfg.TimelineBucketMs = opts.timelineBucketMs
				}
				res, err := sim.Simulate(cfg, sets, assignBySeed[j.seed], invs, policy)
				if err != nil {
					errs[i] = fmt.Errorf("simulation failed: %w", err)
					continue
				}
				results[i] = res
				rows[i] = sim.ResultRow(traceName, j.policy, opts.mapStrategy, cfg, j.capacity, res)
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

	if opts.timelineOut != "" {
		if err := writeTimeline(opts.timelineOut, results[0].Timeline); err != nil {
			return err
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

// selectImages returns either the requested subset or every image in the
// working-set file, sorted for reproducible assignment
func selectImages(sets map[string]sim.WorkingSet, requested string) ([]string, error) {
	if requested != "" {
		images := strings.Split(requested, ",")
		for _, img := range images {
			if _, ok := sets[img]; !ok {
				return nil, fmt.Errorf("image %s not in working-set file", img)
			}
		}
		return images, nil
	}

	images := make([]string, 0, len(sets))
	for img := range sets {
		images = append(images, img)
	}
	sort.Strings(images)
	return images, nil
}

// countImageRefs counts how many images contain each chunk, which the
// refcount eviction policy uses to spot widely shared chunks
func countImageRefs(sets map[string]sim.WorkingSet) map[string]int {
	refs := map[string]int{}
	for _, ws := range sets {
		for _, c := range ws.Chunks {
			refs[c.Hash]++
		}
	}
	return refs
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
