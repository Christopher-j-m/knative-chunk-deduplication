package cache

import (
	"errors"
	"io"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
	"nydus-cluster-cache/pkg/store"
)

// The error we return when a chunk is not found in the cache
var ErrNotFound = errors.New("chunk not found")

// Configure the cache
type Options struct {
	CapacityBytes uint64   // max bytes capacity of the cache => above this eviction strategy kicks in, 0 = unlimited capacity (and no eviction)
	Metrics       *Metrics // counters of hits/misses/latency/histograms and whatever we may add in the future for Prometheus
}

// Wrapper of the underlying store of the cache with additional metrics and eviction strategy
type Cache struct {
	store   store.Store        // the store that actually holds the chunks
	lru     *LRU               // currently the only eviction strategy implemented
	metrics *Metrics           // metrics for Prometheus
	loader  singleflight.Group // group that contains all current loaders for a given hash, so that concurrent requests for the same hash are collapsed into one upstream fetch
	hits    atomic.Uint64      //
	misses  atomic.Uint64
}

// For a chunk hash, the loader fkt is called to fetch the chunk from the remote registry if not found in cache
// Returns a reader (NOT the chunk slice itself) to the chunk data or an error if fetching failed
type Loader func(hash string) (io.Reader, error)

// Create a new cache including the underlying store and the cache settings (capacity, metrics)
func New(cacheStore store.Store, opts Options) *Cache {
	newCache := &Cache{store: cacheStore, metrics: opts.Metrics}
	if opts.CapacityBytes > 0 {
		newCache.lru = NewLRU(cacheStore, opts.CapacityBytes)
	}

	return newCache
}

// Measurement function to measure the latency of cache lookups
func (cache *Cache) tryGet(hash string) (io.ReadCloser, error) {
	startTime := time.Now()
	reader, err := cache.store.Get(hash)

	cache.observeLatency(startTime)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}

	return reader, err
}

// Increments the cache hit counters and notifies the eviction strategy module that the chunk was accessed
// => We only use LRU, so we dont need to differ between strategies yet
func (cache *Cache) recordHit(hash string) {
	cache.hits.Add(1)
	if cache.metrics != nil {
		cache.metrics.Hits.Inc()
	}
	if cache.lru != nil {
		cache.lru.RecordAccess(hash)
	}
}

func (cache *Cache) recordMiss() {
	cache.misses.Add(1)
	if cache.metrics != nil {
		cache.metrics.Misses.Inc()
	}
}

// Returns a reader object to the chunk for the given hash if existing, else ErrNotFound
func (cache *Cache) Get(hash string) (io.ReadCloser, error) {
	chunkReader, err := cache.tryGet(hash)
	if errors.Is(err, ErrNotFound) {
		cache.recordMiss()
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	cache.recordHit(hash)
	return chunkReader, nil
}

// Inserts a chunk in the cache (overwrites possible)
func (cache *Cache) Put(hash string, newChunk io.Reader) error {
	newChunkBytes := &countingReader{reader: newChunk}
	if err := cache.store.Put(hash, newChunkBytes); err != nil {
		return err
	}

	if cache.metrics != nil {
		cache.metrics.PutBytes.Add(float64(newChunkBytes.bytes))
		cache.metrics.StoredBytes.Set(float64(cache.store.StoredBytes()))
		cache.metrics.StoredChunks.Set(float64(cache.store.ChunkCount()))
	}
	if cache.lru != nil {
		cache.lru.RecordInsert(hash, newChunkBytes.bytes)
	}

	return nil
}

func (cache *Cache) Exists(hash string) bool {
	return cache.store.Exists(hash)
}

// Returns a reader to the chunk for the specified hash
// If the chunk is not found in the cache, the loader function gets triggered to fetch the said missing chunk in order to store it in the cache
// All concurrent requests for the same hash are collapsed into one upstream fetch via singleflight
// => We dont fetch the missing chunk multiple times, but only once (for first arriving request) and the other concurrent requests need to wait for the result of the first one
func (cache *Cache) GetOrLoad(hash string, loader Loader) (io.ReadCloser, error) {
	// Chunk already in store
	if reader, err := cache.tryGet(hash); err == nil {
		cache.recordHit(hash)
		return reader, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// Merge concurrent misses via singleflight so fetch the missing chunk for this iteration only once from the remote registry
	// RanLoader is set to true for the caller that actually triggered the loader to fetch the missing chunk, the others are waiters and its value is false for them
	ranLoader := false
	_, err, _ := cache.loader.Do(hash, func() (any, error) {
		ranLoader = true
		cache.recordMiss()
		payload, lerr := loader(hash)
		if lerr != nil {
			return nil, lerr
		}
		return nil, cache.Put(hash, payload)
	})
	if err != nil {
		return nil, err
	}

	// This is a caller that needed to wait
	if !ranLoader {
		cache.recordHit(hash)
	}

	return cache.tryGet(hash)
}

// Returns the total number of cache hits since creation of this cache
func (cache *Cache) HitCount() uint64 { return cache.hits.Load() }

// ...total number of cache misses...
func (cache *Cache) MissCount() uint64 { return cache.misses.Load() }

// Records the seconds from a given start time to now that this function is invoked
func (cache *Cache) observeLatency(start time.Time) {
	if cache.metrics == nil {
		return
	}
	elapsed := time.Since(start).Seconds()

	// Store the latency in s to the prometheus histogram
	cache.metrics.GetLatencySeconds.Observe(elapsed)
}

// Wrapper around an io.Reader that allows us to count the number of bytes read
type countingReader struct {
	reader io.Reader
	bytes  uint64
}

// Reads from the underlying reader and counts the number of bytes read
func (countingReader *countingReader) Read(payload []byte) (int, error) {
	readBytes, err := countingReader.reader.Read(payload)
	countingReader.bytes += uint64(readBytes)
	return readBytes, err
}
