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

	// With a cost model set, a cold-starting pod is unready until its startup
	// has elapsed and arrivals queue behind it, which is what turns the fetch
	// accounting into a response time. Leaving it nil keeps the original
	// behaviour, so the capacity results stay reproducible.
	Cost *CostModel

	// NoClusterCache models the uncached configuration, which keeps Nydus's
	// node-local chunk cache but has no cluster tier, so a chunk another node
	// already fetched is of no use to this one.
	NoClusterCache bool
}

// TimelinePoint is a running fetch total at the end of one time bucket
type TimelinePoint struct {
	AtMs            int64
	RegistryBytes   uint64
	ClusterHitBytes uint64
	LocalBytes      uint64
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

	// response time of every invocation, recorded only with a cost model set
	Latency *Latency
}

type pod struct {
	node      int
	readyAt   int64
	busyUntil []int64
	idleSince int64
}

type appState struct {
	pods []*pod
}

// Simulate replays the invocations against a modeled cluster and returns the
// aggregate fetch accounting. It is single-threaded and deterministic.
func Simulate(cfg Config, sets map[string]WorkingSet, assign map[string]string, invs []Invocation, policy Policy) (Result, error) {
	if cfg.Nodes < 1 || cfg.Concurrency < 1 {
		return Result{}, fmt.Errorf("config needs at least one node and slot")
	}

	var res Result
	apps := map[string]*appState{}
	nodeLocal := make([]map[string]bool, cfg.Nodes)
	podsPerNode := make([]int, cfg.Nodes)
	for i := range nodeLocal {
		nodeLocal[i] = map[string]bool{}
	}

	stored := map[string]uint64{}
	var storedBytes uint64
	imageSeeded := map[string]bool{}

	// two minutes covers every cold start the measured cells produce, and a
	// slower one still reports at the ceiling rather than vanishing
	if cfg.Cost != nil {
		res.Latency = NewLatency(120000)
	}

	// close out a timeline bucket whenever simulated time crosses its edge
	var bucketEnd int64
	if cfg.TimelineBucketMs > 0 && len(invs) > 0 {
		bucketEnd = invs[0].AtMs + cfg.TimelineBucketMs
	}
	snapshot := func(at int64) {
		res.Timeline = append(res.Timeline, TimelinePoint{
			AtMs:            at,
			RegistryBytes:   res.RegistryBytes,
			ClusterHitBytes: res.ClusterHitBytes,
			LocalBytes:      res.LocalBytes,
		})
	}

	for _, inv := range invs {
		if cfg.TimelineBucketMs > 0 {
			for inv.AtMs >= bucketEnd {
				snapshot(bucketEnd)
				bucketEnd += cfg.TimelineBucketMs
			}
		}
		res.Invocations++

		image, ok := assign[inv.App]
		if !ok {
			return Result{}, fmt.Errorf("app %s has no image assignment", inv.App)
		}
		ws, ok := sets[image]
		if !ok {
			return Result{}, fmt.Errorf("image %s has no working set", image)
		}

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
			p.busyUntil = append(p.busyUntil, begin+inv.DurMs)
			res.WarmStarts++
			if res.Latency != nil {
				res.Latency.Observe(begin - inv.AtMs + inv.DurMs)
			}
			continue
		}

		// cold path: place a new pod on the emptiest node
		node := 0
		for n := 1; n < cfg.Nodes; n++ {
			if podsPerNode[n] < podsPerNode[node] {
				node = n
			}
		}
		podsPerNode[node]++
		newPod := &pod{node: node}
		state.pods = append(state.pods, newPod)
		res.ColdStarts++
		if !imageSeeded[image] {
			imageSeeded[image] = true
			res.SeedColdStarts++
		}

		// walk the working set and account for where each chunk comes from
		var counts FetchCounts
		for _, c := range ws.Chunks {
			switch {
			case nodeLocal[node][c.Hash]:
				res.LocalBytes += c.Size
				counts.LocalChunks++
			case !cfg.NoClusterCache && stored[c.Hash] > 0:
				res.ClusterHitBytes += c.Size
				counts.CacheChunks++
				policy.OnHit(c.Hash)
			default:
				res.RegistryBytes += c.Size
				counts.RegistryChunks++
				counts.RegistryBytes += c.Size
				stored[c.Hash] = c.Size
				storedBytes += c.Size
				policy.OnInsert(c.Hash, c.Size)
				for cfg.CapacityBytes > 0 && storedBytes > cfg.CapacityBytes {
					victim, size, ok := policy.Evict()
					if !ok {
						break
					}
					delete(stored, victim)
					storedBytes -= size
					res.Evictions++
				}
			}
			nodeLocal[node][c.Hash] = true
		}
		res.LocalChunks += counts.LocalChunks
		res.CacheChunks += counts.CacheChunks
		res.RegistryChunks += counts.RegistryChunks

		// the fetch accounting above is what the startup cost is charged on,
		// so the pod's readiness can only be set once the walk is done
		var startup int64
		if cfg.Cost != nil {
			startup = cfg.Cost.StartupMs(image, counts)
		}
		newPod.readyAt = inv.AtMs + startup
		newPod.busyUntil = []int64{newPod.readyAt + inv.DurMs}
		if res.Latency != nil {
			res.Latency.Observe(startup + inv.DurMs)
		}
		if storedBytes > res.PeakStoredBytes {
			res.PeakStoredBytes = storedBytes
		}
	}
	if cfg.TimelineBucketMs > 0 && len(invs) > 0 {
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
