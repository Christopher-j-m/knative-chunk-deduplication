package simulation

// Approximate the cold start latency (container starts on a node that hasnt seen the image yet) by adding up the apps startup time
// via ibm trace data + cost for missing image data (based on where they come from, e.g. cache, remote registry, the node itself) or
// the total cost for downloading the whole image

// Recording where the chunks during a cold start came from
type FetchCounts struct {
	LocalChunks    int    // data that was already in the local node cache (cheapest)
	CacheChunks    int    // ... from the cluster cache
	RegistryChunks int    // ... from the remote registry (most expensive)
	RegistryBytes  uint64 // how many bytes we needed to fetch from registry
}

// Contains the estimated cost/latencies in ms based on where the image data was fetched from
type CostModel struct {
	WarmFloorMs        []int64 // Image startup time
	DefaultWarmFloorMs int64   // Baseline startup time if we dont know the image startup time

	RegistryChunkMs float64 // cost (rtt, unpacking, ...) for a chunk fetched from the remote registry
	CacheChunkMs    float64 // ... from the cluster cache

	Eager           bool     // if true, we calculate as if we fetch the whole image first before starting (instead of fetching ondemand)
	ConnectMs       int64    // a delay per eager download before data actually start transmitting (e.g. tcp handshake)
	EagerBytesPerMs float64  // download speed in eager mode
	EagerImageBytes []uint64 // compressed image size that gets downloaded in eager mode
}

// Estimated time for a specific image to start when all its data is already on the node
func (cost CostModel) warmFloor(image int32) int64 {

	// Check if the image number is even within our list or its value is zero (== not measured yet)
	// In that case return the fallback default value
	if image < 0 || int(image) >= len(cost.WarmFloorMs) || cost.WarmFloorMs[image] == 0 {
		return cost.DefaultWarmFloorMs
	}

	return cost.WarmFloorMs[image]
}

// look up the compressed size of an image, which has to be downloaded in case of eager mode
func (cost CostModel) eagerBytes(image int32) uint64 {

	// Return 0 if not measured yet
	if image < 0 || int(image) >= len(cost.EagerImageBytes) {
		return 0
	}

	return cost.EagerImageBytes[image]
}

// estimate how long the pod takes before it can answer a request
// Beispiel: 500ms base, 10 cache chunks ~2ms, 5 registry chunks ~20 ms = 620 ms coldstart latency
func (cost CostModel) StartupMs(image int32, counts FetchCounts) int64 {
	total := cost.warmFloor(image)

	// need to download whole image upfront, so we dont need to calculate by missing chunk
	if cost.Eager {
		total += cost.ConnectMs

		// prevent division by zero, in that case only setup delay costs
		if cost.EagerBytesPerMs > 0 {
			total += int64(float64(cost.eagerBytes(image)) / cost.EagerBytesPerMs)
		}

		return total
	}

	total += int64(float64(counts.CacheChunks) * cost.CacheChunkMs)
	total += int64(float64(counts.RegistryChunks) * cost.RegistryChunkMs)
	return total
}

// custom format for the response time data of the ibm traces (which would take too much mem)
type Latency struct {
	buckets  []uint64 // slots for response times (0,1,2,...) and the count indicating how often e.g. 1ms response times occured (1ms = slot 1)
	overflow uint64   // amount of requests that took longer than the largest slot in the buckets (in our case > 2 mins)
	count    uint64   // amount of requests recorded
	maxMs    int64    // largest response time measured in overflow, rest just increments overflow
}

// histogram with one bucket for every ms from 0 to 2 mins (in ms)
func NewLatency(maxMs int64) *Latency {
	if maxMs < 1 {
		maxMs = 1
	}

	return &Latency{buckets: make([]uint64, maxMs+1), maxMs: maxMs}
}

// records one response time in milliseconds
func (latency *Latency) Observe(ms int64) {
	latency.count++
	if ms < 0 {
		ms = 0
	}

	if ms > latency.maxMs {
		latency.overflow++
		return
	}

	latency.buckets[ms]++
}

func (latency *Latency) Count() uint64 { return latency.count }

// returns the response time at the given quantile
func (latency *Latency) Quantile(quantile float64) int64 {
	if latency.count == 0 {
		return 0
	}

	// get target position of the quantil position
	target := uint64(quantile * float64(latency.count))
	if target < 1 {
		target = 1
	}

	if target > latency.count {
		target = latency.count
	}

	// go over each bucket and add up their count until we reach the target position
	var seen uint64
	for ms, n := range latency.buckets {
		seen += n
		if seen >= target {
			return int64(ms)
		}
	}

	return latency.maxMs
}
