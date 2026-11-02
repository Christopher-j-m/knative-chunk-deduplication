package sim

import "fmt"

// Orchestrator of the cache simulation who decides for each function call where it gets placed, what image data is
// already available and where the remaining ones come from

// Configuration for the simulated run
type Config struct {
	Nodes            int    // how many nodes the simulated cluster has
	CapacityBytes    uint64 // capacity of the cluster cache
	KeepAliveMs      int64  // time until an unused pod can be idle before it gets terminated
	Concurrency      int    // amount of function calls a single pod can accept at the same time
	Seed             int64  // seed for the random generator so we get predictable node selection for random selection
	TimelineBucketMs int64  // interval at which we record the total download size up to this simulated point of time

	Placement string // placement strategy (packed, or random allowed) for pod selection

	// simulator for estimating costs (latency)caused by fetching chunks from local cache, cluster cache or remote registry
	// without it, we just count how many data came from which source without any latency estimations
	Cost           *CostModel
	NoClusterCache bool // whether we simulate as if there's no cluster cache (and going to the remote registry instead)
}

// One point in time/snapshot of what we had to download so far from which source
type TimelinePoint struct {
	AtMs            int64
	RegistryBytes   uint64
	ClusterHitBytes uint64
	LocalBytes      uint64
}

// finer grained metrics for grouping functions by frequency class/bucket
// up to 10 calls, 11–100, 101–1000, 1001–10000, 10001–100000, and above 100000
type ClassStats struct {
	Apps          int
	Invocations   int
	ColdStarts    int
	LocalBytes    uint64
	CacheBytes    uint64
	RegistryBytes uint64

	// only when a cost model simulation is set in config
	// cold start latency and function response time during every invocation from function call until it finishes
	ColdLatency *Latency
	Response    *Latency
}

// Aggregated results of the simulated run
type Result struct {
	Invocations     int             // amount of function invocations
	ColdStarts      int             // function calls that started a new pod
	SeedColdStarts  int             // amount of cold starts of an image anywhere in the cluster
	WarmStarts      int             // function calls that an existing pod already handled
	LocalBytes      uint64          // bytes already present on the node local cache
	ClusterHitBytes uint64          // bytes fetched successfully from the cluster cache
	RegistryBytes   uint64          // ... from the remote registry
	Evictions       int             // amount of chunks that got evicted from the cluster cache
	PeakStoredBytes uint64          // the maximum amount of chunks (in bytes) the cluster cache held at some point
	Timeline        []TimelinePoint // list of TimeLinePoint snapshots
	LocalChunks     int             // image chunks fetched from the node local cache
	CacheChunks     int             // ...from the cluster cache
	RegistryChunks  int             // ...from the remote registry
	NodesUsed       int             // how many nodes ran atleast one pod over the time of the simulation
	PeakCacheRPS    int             // the maximum amount of chunk requests that the cluster cache received in any simulated second
	Latency         *Latency        // response times for every function invocation
	ColdLatency     *Latency        // cold start times of every new pod
	Classes         [ClassCount]ClassStats
}

// returns the amount of chunks that got fetched from the remote registry
func (res Result) RegistryRequests() int {
	return res.RegistryChunks
}

// state for each pod
type pod struct {
	node      int     // node id this pod is running on
	readyAt   int64   // simulated time of the pod until its ready
	busyUntil []int64 // end times of each function call on this pod
	idleSince int64   // the time of the last time this pod successfully finished a function call
}

// all running pods for a single function
type appState struct {
	pods []*pod
}

const latencyCeilingMs = 120000

// main function to perform the simulation of traces against a simulated cluster
func Simulate(cfg Config, imageCatalog *Catalog, assign []int32, trace *Trace, policy Policy) (Result, error) {

	/* Input validation */
	if cfg.Nodes < 1 || cfg.Concurrency < 1 {
		return Result{}, fmt.Errorf("error: config needs at least one node and slot/concurrency")
	}

	if len(assign) != len(trace.Apps) {
		return Result{}, fmt.Errorf("error, the assignment covers %d apps, trace only has %d", len(assign), len(trace.Apps))
	}

	for app, image := range assign {
		if image < 0 || int(image) >= len(imageCatalog.Sets) {
			return Result{}, fmt.Errorf("error: app %s assigned to image %d, but the image catalog only has %d", trace.Apps[app], image, len(imageCatalog.Sets))
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

	// Setup the metric counters
	var res Result
	apps := make([]*appState, len(trace.Apps))
	classOf := make([]int, len(trace.Apps))
	for app, count := range trace.Counts {
		classOf[app] = ClassOf(count)
		res.Classes[classOf[app]].Apps++
	}

	nodeLocal := make([]bitset, cfg.Nodes)
	podsPerNode := make([]int, cfg.Nodes)

	stored := make([]bool, len(imageCatalog.Sizes))
	var storedBytes uint64
	imageSeeded := make([]bool, len(imageCatalog.Sets))

	// Setup latency cost estimation for this run
	if cfg.Cost != nil {
		res.Latency = NewLatency(latencyCeilingMs)
		res.ColdLatency = NewLatency(latencyCeilingMs)
		for currClass := range res.Classes {
			res.Classes[currClass].ColdLatency = NewLatency(latencyCeilingMs)
			res.Classes[currClass].Response = NewLatency(latencyCeilingMs)
		}
	}

	// set the time when to take the first snapshot of the aggregated metrics (e.g. after first call one bucket length)
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

	// find the second during the simulation in which the cluster cache requests/s peaked
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

	// record for a single function call the response time in the frequency-class and overall
	observe := func(class int, response int64) {
		if res.Latency == nil {
			return
		}
		res.Latency.Observe(response)
		res.Classes[class].Response.Observe(response)
	}

	// main handler loop for recording function invocations and the corresponding placement on the nodes
	for _, currFuncInvocation := range trace.Invocations {

		// record snapshot if the timeline in the config is enabled
		if cfg.TimelineBucketMs > 0 {
			for currFuncInvocation.AtMs >= bucketEnd {
				snapshot(bucketEnd)
				bucketEnd += cfg.TimelineBucketMs
			}
		}
		res.Invocations++
		class := classOf[currFuncInvocation.App]
		res.Classes[class].Invocations++

		// get the image of the function and how long this runs
		image := assign[currFuncInvocation.App]
		dur := int64(currFuncInvocation.DurMs)

		// create a new record for this function (if first time function call)
		state := apps[currFuncInvocation.App]
		if state == nil {
			state = &appState{}
			apps[currFuncInvocation.App] = state
		}

		// deletes finished function calls and updates the idle time from anew
		alive := state.pods[:0]
		for _, currPod := range state.pods {
			currPod.reap(currFuncInvocation.AtMs)
			if currFuncInvocation.AtMs < currPod.readyAt {
				alive = append(alive, currPod)
				continue
			}
			if len(currPod.busyUntil) == 0 && currFuncInvocation.AtMs-currPod.idleSince > cfg.KeepAliveMs {
				podsPerNode[currPod.node]--
				continue
			}
			alive = append(alive, currPod)
		}
		state.pods = alive

		// check if any of the existing function pods has a free slot (see concurrency config), that pod gets this function call
		// => the warm path
		if availablePod := freeSlot(state.pods, cfg.Concurrency); availablePod != nil {
			begin := currFuncInvocation.AtMs
			if availablePod.readyAt > begin {
				begin = availablePod.readyAt
			}
			availablePod.busyUntil = append(availablePod.busyUntil, begin+dur)
			res.WarmStarts++
			observe(class, begin-currFuncInvocation.AtMs+dur)
			continue
		}

		// ...else cold path with creating a new pod for this function invocation
		node := placement.Place(podsPerNode)
		if nodeLocal[node] == nil {
			nodeLocal[node] = newBitset(len(imageCatalog.Sizes))
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

		// check for every image chunk if it is already availe in node local cache -> cluster cache -> remote registry
		var counts FetchCounts
		local := nodeLocal[node]
		stats := &res.Classes[class]
		for _, id := range imageCatalog.Sets[image] {
			size := imageCatalog.Sizes[id]
			switch {
			// node local cache
			case local.has(id):
				res.LocalBytes += size
				stats.LocalBytes += size
				counts.LocalChunks++

			// cluster cache
			case !cfg.NoClusterCache && stored[id]:
				res.ClusterHitBytes += size
				stats.CacheBytes += size
				counts.CacheChunks++
				policy.OnHit(id)

			// remote registry
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

		// update metrics regarding where we got each chunk
		res.LocalChunks += counts.LocalChunks
		res.CacheChunks += counts.CacheChunks
		res.RegistryChunks += counts.RegistryChunks
		countCacheRequests(currFuncInvocation.AtMs, counts.CacheChunks)

		// estimate the latency costs based on where we got each missing chunk from
		// e.g. remote registry costly, while node local cache very cheap and low latency
		var startup int64
		if cfg.Cost != nil {
			startup = cfg.Cost.StartupMs(image, counts)
			res.ColdLatency.Observe(startup)
			stats.ColdLatency.Observe(startup)
		}
		newPod.readyAt = currFuncInvocation.AtMs + startup
		newPod.busyUntil = []int64{newPod.readyAt + dur}
		observe(class, startup+dur)
		if storedBytes > res.PeakStoredBytes {
			res.PeakStoredBytes = storedBytes
		}
	}

	// check if we peaked during the simulated second now that all calls are done...
	if rpsCount > res.PeakCacheRPS {
		res.PeakCacheRPS = rpsCount
	}

	// save a snapshot to the timeline for this simulated second
	if cfg.TimelineBucketMs > 0 && len(trace.Invocations) > 0 {
		snapshot(bucketEnd)
	}
	return res, nil
}

// cleanup function to delete a pods slots if the last function call exceeded the allowed idle time
func (reapingPod *pod) reap(now int64) {
	busy := reapingPod.busyUntil[:0]
	for _, until := range reapingPod.busyUntil {
		if until > now {
			busy = append(busy, until)
		} else if until > reapingPod.idleSince {
			reapingPod.idleSince = until
		}
	}
	reapingPod.busyUntil = busy
}

// returns an available pod for a function that still has slots (slots < config concurrency)
func freeSlot(pods []*pod, concurrency int) *pod {
	for _, p := range pods {
		if len(p.busyUntil) < concurrency {
			return p
		}
	}
	return nil
}
