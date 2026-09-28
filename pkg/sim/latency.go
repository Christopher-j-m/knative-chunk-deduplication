package sim

// FetchCounts records where one cold start's chunks came from. The simulator
// already decides this per chunk; keeping the counts lets a cost model charge
// each class at its own rate.
type FetchCounts struct {
	LocalChunks    int
	CacheChunks    int
	RegistryChunks int
	RegistryBytes  uint64
}

// CostModel turns a cold start's chunk accounting into the delay before the
// pod can serve. Every coefficient is meant to be read off measured cells
// rather than chosen, so the model carries no constant that the evaluation
// has not already reported.
type CostModel struct {
	// application startup, which no distribution strategy can remove,
	// indexed by image
	WarmFloorMs        []int64
	DefaultWarmFloorMs int64

	// what one chunk costs when it comes from the registry, and when it comes
	// from the cluster cache, at the round trip the coefficients were fitted at
	RegistryChunkMs float64
	CacheChunkMs    float64

	// eager pulling ignores the startup working set and moves the whole
	// compressed image instead, so it is charged in bytes over a rate
	Eager           bool
	ConnectMs       int64
	EagerBytesPerMs float64
	EagerImageBytes []uint64
}

// warmFloor is the image's own startup floor, falling back to the default
// for an image the measurements never covered
func (m CostModel) warmFloor(image int32) int64 {
	if image < 0 || int(image) >= len(m.WarmFloorMs) || m.WarmFloorMs[image] == 0 {
		return m.DefaultWarmFloorMs
	}
	return m.WarmFloorMs[image]
}

// eagerBytes is the compressed size an eager pull has to move, or zero for
// an image the measurements never covered
func (m CostModel) eagerBytes(image int32) uint64 {
	if image < 0 || int(image) >= len(m.EagerImageBytes) {
		return 0
	}
	return m.EagerImageBytes[image]
}

// StartupMs is how long the pod takes before it can answer a request.
func (m CostModel) StartupMs(image int32, counts FetchCounts) int64 {
	total := m.warmFloor(image)

	if m.Eager {
		total += m.ConnectMs
		if m.EagerBytesPerMs > 0 {
			total += int64(float64(m.eagerBytes(image)) / m.EagerBytesPerMs)
		}
		return total
	}

	total += int64(float64(counts.CacheChunks) * m.CacheChunkMs)
	total += int64(float64(counts.RegistryChunks) * m.RegistryChunkMs)
	return total
}

// Latency accumulates every invocation's response time in one-millisecond
// buckets, which holds a week of traffic in about a megabyte instead of the
// gigabytes a full sample would need. Observations past the ceiling fall into
// an overflow bucket and report at the ceiling, so a long tail reads as slow
// rather than disappearing.
type Latency struct {
	buckets  []uint64
	overflow uint64
	count    uint64
	maxMs    int64
}

// NewLatency creates a histogram covering zero to maxMs inclusive.
func NewLatency(maxMs int64) *Latency {
	if maxMs < 1 {
		maxMs = 1
	}
	return &Latency{buckets: make([]uint64, maxMs+1), maxMs: maxMs}
}

// Observe records one response time in milliseconds.
func (l *Latency) Observe(ms int64) {
	l.count++
	if ms < 0 {
		ms = 0
	}
	if ms > l.maxMs {
		l.overflow++
		return
	}
	l.buckets[ms]++
}

// Count is how many observations the histogram holds.
func (l *Latency) Count() uint64 { return l.count }

// Quantile returns the response time at the given quantile, by nearest rank.
func (l *Latency) Quantile(q float64) int64 {
	if l.count == 0 {
		return 0
	}

	target := uint64(q * float64(l.count))
	if target < 1 {
		target = 1
	}
	if target > l.count {
		target = l.count
	}

	var seen uint64
	for ms, n := range l.buckets {
		seen += n
		if seen >= target {
			return int64(ms)
		}
	}
	return l.maxMs
}
