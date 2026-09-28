package sim

import "fmt"

// Config sets the cluster shape and pod lifecycle parameters for one run.
// A nonzero TimelineBucketMs additionally records cumulative fetch totals
// once per bucket of simulated time.
type Config struct {
	Nodes            int
	CapacityBytes    uint64
	KeepAliveMs      int64
	Concurrency      int
	Seed             int64
	TimelineBucketMs int64

	// which node a cold-starting pod lands on; empty means packed
	Placement string

	// With a cost model set, a cold-starting pod is unready until its startup
	// has elapsed and arrivals queue behind it, which is what turns the fetch
	// accounting into a response time. Leaving it nil keeps the fetch
	// accounting reproducible without one
	Cost *CostModel

	// NoClusterCache models the uncached configuration, which keeps Nydus's
	// node-local chunk cache but has no cluster tier, so a chunk another node
	// already fetched is of no use to this one
	NoClusterCache bool
}

// TimelinePoint is a running fetch total at the end of one time bucket
type TimelinePoint struct {
	AtMs            int64
	RegistryBytes   uint64
	ClusterHitBytes uint64
	LocalBytes      uint64
}

// ClassStats is the accounting for every app in one invocation-frequency
// class, which is how the study separates what the cache does for rarely
// invoked functions from what it does for busy ones.
type ClassStats struct {
	Apps          int
	Invocations   int
	ColdStarts    int
	LocalBytes    uint64
	CacheBytes    uint64
	RegistryBytes uint64

	// startup delay of every cold start and response time of every
	// invocation in the class, recorded only with a cost model set
	ColdLatency *Latency
	Response    *Latency
}

// Result aggregates what one replay run observed.
type Result struct {
	Invocations     int
	ColdStarts      int
	SeedColdStarts  int
	WarmStarts      int
	LocalBytes      uint64
	ClusterHitBytes uint64
	RegistryBytes   uint64
	Evictions       int
	PeakStoredBytes uint64
	Timeline        []TimelinePoint

	// per-class chunk counts, which the cost model charges at different rates
	LocalChunks    int
	CacheChunks    int
	RegistryChunks int

	// how many nodes ever hosted a pod, which shows where packed placement
	// stops using the cluster
	NodesUsed int

	// the most chunk requests the cluster cache saw in one simulated second,
	// to be read against the measured serving ceiling
	PeakCacheRPS int

	// response time of every invocation and startup delay of every cold
	// start, recorded only with a cost model set
	Latency     *Latency
	ColdLatency *Latency

	Classes [ClassCount]ClassStats
}

// RegistryRequests is how many chunk fetches reached the registry.
func (r Result) RegistryRequests() int { return r.RegistryChunks }

type pod struct {
	node      int
	readyAt   int64
	busyUntil []int64
	idleSince int64
}

type appState struct {
	pods []*pod
}

// latencyCeilingMs is two minutes, which covers every cold start the
// measured cells produce; a slower one reports at the ceiling
const latencyCeilingMs = 120000

// Simulate replays the invocations against a modeled cluster and returns the
// aggregate fetch accounting. It is single-threaded and deterministic.
func Simulate(cfg Config, catalog *Catalog, assign []int32, trace *Trace, policy Policy) (Result, error) {
	if cfg.Nodes < 1 || cfg.Concurrency < 1 {
		return Result{}, fmt.Errorf("config needs at least one node and slot")
	}
	if len(assign) != len(trace.Apps) {
		return Result{}, fmt.Errorf("assignment covers %d apps, trace has %d", len(assign), len(trace.Apps))
	}
	for app, image := range assign {
		if image < 0 || int(image) >= len(catalog.Sets) {
			return Result{}, fmt.Errorf("app %s assigned to image %d, catalog has %d", trace.Apps[app], image, len(catalog.Sets))
		}
	}
	placementName := cfg.Placement
	if placementName == "" {
		placementName = "packed"
	}
	placement, err := NewPlacement(placementName, cfg.Seed)
	if err != nil {
		return Result{}, err
	}

	var res Result
	apps := make([]*appState, len(trace.Apps))
	classOf := make([]int, len(trace.Apps))
	for app, count := range trace.Counts {
		classOf[app] = ClassOf(count)
		res.Classes[classOf[app]].Apps++
	}

	// node-local sets are allocated the first time a node hosts a pod, so a
	// large cluster that packed placement never reaches costs nothing
	nodeLocal := make([]bitset, cfg.Nodes)
	podsPerNode := make([]int, cfg.Nodes)

	stored := make([]bool, len(catalog.Sizes))
	var storedBytes uint64
	imageSeeded := make([]bool, len(catalog.Sets))

	if cfg.Cost != nil {
		res.Latency = NewLatency(latencyCeilingMs)
		res.ColdLatency = NewLatency(latencyCeilingMs)
		for c := range res.Classes {
			res.Classes[c].ColdLatency = NewLatency(latencyCeilingMs)
			res.Classes[c].Response = NewLatency(latencyCeilingMs)
		}
	}

	// close out a timeline bucket whenever simulated time crosses its edge
	var bucketEnd int64
	if cfg.TimelineBucketMs > 0 && len(trace.Invocations) > 0 {
		bucketEnd = trace.Invocations[0].AtMs + cfg.TimelineBucketMs
	}
	snapshot := func(at int64) {
		res.Timeline = append(res.Timeline, TimelinePoint{
			AtMs:            at,
			RegistryBytes:   res.RegistryBytes,
			ClusterHitBytes: res.ClusterHitBytes,
			LocalBytes:      res.LocalBytes,
		})
	}

	// cache requests are counted per simulated second; the trace is sorted,
	// so one running bucket is enough to find the peak
	var rpsSecond int64 = -1
	rpsCount := 0
	countCacheRequests := func(at int64, n int) {
		second := at / 1000
		if second != rpsSecond {
			if rpsCount > res.PeakCacheRPS {
				res.PeakCacheRPS = rpsCount
			}
			rpsSecond, rpsCount = second, 0
		}
		rpsCount += n
	}

	observe := func(class int, response int64) {
		if res.Latency == nil {
			return
		}
		res.Latency.Observe(response)
		res.Classes[class].Response.Observe(response)
	}

	for _, inv := range trace.Invocations {
		if cfg.TimelineBucketMs > 0 {
			for inv.AtMs >= bucketEnd {
				snapshot(bucketEnd)
				bucketEnd += cfg.TimelineBucketMs
			}
		}
		res.Invocations++
		class := classOf[inv.App]
		res.Classes[class].Invocations++
		image := assign[inv.App]
		dur := int64(inv.DurMs)

		state := apps[inv.App]
		if state == nil {
			state = &appState{}
			apps[inv.App] = state
		}

		// drop pods that sat idle past the keep-alive window
		alive := state.pods[:0]
		for _, p := range state.pods {
			p.reap(inv.AtMs)
			if inv.AtMs < p.readyAt {
				alive = append(alive, p)
				continue
			}
			if len(p.busyUntil) == 0 && inv.AtMs-p.idleSince > cfg.KeepAliveMs {
				podsPerNode[p.node]--
				continue
			}
			alive = append(alive, p)
		}
		state.pods = alive

		// warm path: any pod with a free slot takes the invocation. A pod that
		// is still starting holds the request instead of serving it, which is
		// what the platform's activator does, so the wait counts toward the
		// requester's latency
		if p := freeSlot(state.pods, cfg.Concurrency); p != nil {
			begin := inv.AtMs
			if p.readyAt > begin {
				begin = p.readyAt
			}
			p.busyUntil = append(p.busyUntil, begin+dur)
			res.WarmStarts++
			observe(class, begin-inv.AtMs+dur)
			continue
		}

		// cold path: place a new pod
		node := placement.Place(podsPerNode)
		if nodeLocal[node] == nil {
			nodeLocal[node] = newBitset(len(catalog.Sizes))
			res.NodesUsed++
		}
		podsPerNode[node]++
		newPod := &pod{node: node}
		state.pods = append(state.pods, newPod)
		res.ColdStarts++
		res.Classes[class].ColdStarts++
		if !imageSeeded[image] {
			imageSeeded[image] = true
			res.SeedColdStarts++
		}

		// walk the working set and account for where each chunk comes from
		var counts FetchCounts
		local := nodeLocal[node]
		stats := &res.Classes[class]
		for _, id := range catalog.Sets[image] {
			size := catalog.Sizes[id]
			switch {
			case local.has(id):
				res.LocalBytes += size
				stats.LocalBytes += size
				counts.LocalChunks++
			case !cfg.NoClusterCache && stored[id]:
				res.ClusterHitBytes += size
				stats.CacheBytes += size
				counts.CacheChunks++
				policy.OnHit(id)
			default:
				res.RegistryBytes += size
				stats.RegistryBytes += size
				counts.RegistryChunks++
				counts.RegistryBytes += size
				stored[id] = true
				storedBytes += size
				policy.OnInsert(id, size)
				for cfg.CapacityBytes > 0 && storedBytes > cfg.CapacityBytes {
					victim, victimSize, ok := policy.Evict()
					if !ok {
						break
					}
					stored[victim] = false
					storedBytes -= victimSize
					res.Evictions++
				}
			}
			local.set(id)
		}
		res.LocalChunks += counts.LocalChunks
		res.CacheChunks += counts.CacheChunks
		res.RegistryChunks += counts.RegistryChunks
		countCacheRequests(inv.AtMs, counts.CacheChunks)

		// the fetch accounting above is what the startup cost is charged on,
		// so the pod's readiness can only be set once the walk is done
		var startup int64
		if cfg.Cost != nil {
			startup = cfg.Cost.StartupMs(image, counts)
			res.ColdLatency.Observe(startup)
			stats.ColdLatency.Observe(startup)
		}
		newPod.readyAt = inv.AtMs + startup
		newPod.busyUntil = []int64{newPod.readyAt + dur}
		observe(class, startup+dur)
		if storedBytes > res.PeakStoredBytes {
			res.PeakStoredBytes = storedBytes
		}
	}
	if rpsCount > res.PeakCacheRPS {
		res.PeakCacheRPS = rpsCount
	}
	if cfg.TimelineBucketMs > 0 && len(trace.Invocations) > 0 {
		snapshot(bucketEnd)
	}
	return res, nil
}

// reap clears finished slots and stamps when the pod went idle
func (p *pod) reap(now int64) {
	busy := p.busyUntil[:0]
	for _, until := range p.busyUntil {
		if until > now {
			busy = append(busy, until)
		} else if until > p.idleSince {
			p.idleSince = until
		}
	}
	p.busyUntil = busy
}

// freeSlot returns the first pod that can take one more concurrent invocation
func freeSlot(pods []*pod, concurrency int) *pod {
	for _, p := range pods {
		if len(p.busyUntil) < concurrency {
			return p
		}
	}
	return nil
}
