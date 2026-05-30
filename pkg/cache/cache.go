package cache

import (
	"errors"
	"io"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"nydus-cluster-cache/pkg/store"
)

// ErrNotFound is returned when no chunk is associated with the requested hash
// and no loader was supplied
var ErrNotFound = errors.New("chunk not found")

// Options controls cache-wide behavior. Zero values mean "no policy".
type Options struct {
	// capacity in bytes; zero disables eviction so the cache grows unbounded
	CapacityBytes uint64

	// Metrics receives the running counters; may be nil for tests
	Metrics *Metrics
}

// Cache is the transport-agnostic facade that protocol code talks to.
// It coordinates the backing store, the eviction policy, and the metrics.
type Cache struct {
	store   store.Store
	lru     *LRU
	metrics *Metrics

	loader singleflight.Group

	hits   atomic.Uint64
	misses atomic.Uint64
}

// Loader fetches the payload for a hash from an upstream source (typically the
// container registry). Returning a reader leaves byte handling to
// the cache.
type Loader func(hash string) (io.Reader, error)

// New creates a cache facade backed by the given store. Eviction is wired only
// when CapacityBytes > 0.
func New(backing store.Store, opts Options) *Cache {
	c := &Cache{store: backing, metrics: opts.Metrics}
	if opts.CapacityBytes > 0 {
		c.lru = NewLRU(backing, opts.CapacityBytes)
	}
	return c
}

// tryGet performs a raw store lookup and observes latency. It does NOT touch
// hit/miss counters, making it safe to call from GetOrLoad without inflating
// metrics.
func (c *Cache) tryGet(hash string) (io.ReadCloser, error) {
	start := time.Now()
	reader, err := c.store.Get(hash)
	c.observeLatency(start)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	return reader, err
}

// recordHit increments the hit counters and notifies the LRU policy.
func (c *Cache) recordHit(hash string) {
	c.hits.Add(1)
	if c.metrics != nil {
		c.metrics.Hits.Inc()
	}
	if c.lru != nil {
		c.lru.RecordAccess(hash)
	}
}

// recordMiss increments the miss counters.
func (c *Cache) recordMiss() {
	c.misses.Add(1)
	if c.metrics != nil {
		c.metrics.Misses.Inc()
	}
}

// Get returns the stored payload for hash. On a miss it returns ErrNotFound.
func (c *Cache) Get(hash string) (io.ReadCloser, error) {
	reader, err := c.tryGet(hash)
	if errors.Is(err, ErrNotFound) {
		c.recordMiss()
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.recordHit(hash)
	return reader, nil
}

// Put stores payload under hash and notifies the eviction policy and metrics.
func (c *Cache) Put(hash string, payload io.Reader) error {
	counter := &countingReader{r: payload}
	if err := c.store.Put(hash, counter); err != nil {
		return err
	}
	if c.metrics != nil {
		c.metrics.PutBytes.Add(float64(counter.bytes))
		c.metrics.StoredBytes.Set(float64(c.store.StoredBytes()))
		c.metrics.StoredChunks.Set(float64(c.store.ChunkCount()))
	}
	if c.lru != nil {
		c.lru.RecordInsert(hash, counter.bytes)
	}
	return nil
}

// Exists reports whether hash is currently held in the backing store.
func (c *Cache) Exists(hash string) bool {
	return c.store.Exists(hash)
}

// GetOrLoad serves cached bytes when available. On a miss it invokes loader
// behind a singleflight group keyed by hash, so concurrent callers share one
// upstream fetch.
//
// Metric semantics:
//   - Fast path (cache already warm): 1 hit per caller.
//   - Singleflight winner (actually runs the loader): 1 miss.
//   - Singleflight waiters (share the winner's result): 1 hit each.
func (c *Cache) GetOrLoad(hash string, loader Loader) (io.ReadCloser, error) {
	// fast path — chunk already in store
	if reader, err := c.tryGet(hash); err == nil {
		c.recordHit(hash)
		return reader, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// slow path — collapse concurrent misses via singleflight
	// ranLoader is set inside the closure, which executes exactly once on the
	// winning goroutine; waiters never execute the closure, so their ranLoader
	// stays false and they correctly record a hit after the shared fetch
	ranLoader := false
	_, err, _ := c.loader.Do(hash, func() (any, error) {
		ranLoader = true
		c.recordMiss()
		payload, lerr := loader(hash)
		if lerr != nil {
			return nil, lerr
		}
		return nil, c.Put(hash, payload)
	})
	if err != nil {
		return nil, err
	}
	if !ranLoader {
		// waited for the winner — chunk now in store
		c.recordHit(hash)
	}
	// tryGet here does NOT touch counters — hit/miss was recorded above
	return c.tryGet(hash)
}

// HitCount returns the total number of cache hits recorded since creation.
func (c *Cache) HitCount() uint64 { return c.hits.Load() }

// MissCount returns the total number of cache misses recorded since creation.
func (c *Cache) MissCount() uint64 { return c.misses.Load() }

func (c *Cache) observeLatency(start time.Time) {
	if c.metrics == nil {
		return
	}
	elapsed := time.Since(start).Seconds()
	c.metrics.GetLatencySeconds.Observe(elapsed)
}

type countingReader struct {
	r     io.Reader
	bytes uint64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.bytes += uint64(n)
	return n, err
}
