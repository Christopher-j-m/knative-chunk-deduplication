package cache

import (
	"container/list"
	"sync"

	"nydus-cluster-cache/pkg/store"
)

// Least-Recently-Used (LRU) eviction policy
// => Drop the least recently used chunk in the cache when the specified cache capacity is exceeded
type LRU struct {
	mutex      sync.Mutex
	capacity   uint64                   // maximum number of bytes until LRU starts dropping chunks from cache
	order      *list.List               // linked list that contains lruEntry objects => most recently requested at front
	entries    map[string]*list.Element // key = chunk hash, value = pointer to the corresponding entry in recency order
	chunkStore store.Store
}

// Representation of a chunk in the cluster cache that is used for the ordering of chunks by recency of access
type lruEntry struct {
	hash string
	size uint64
}

func NewLRU(chunkStore store.Store, capacity uint64) *LRU {
	return &LRU{
		capacity:   capacity,
		order:      list.New(),
		entries:    make(map[string]*list.Element),
		chunkStore: chunkStore,
	}
}

// Adds a chunk that was just inserted to the cluster cache (via PUT) to the LRU tracking list
func (lru *LRU) RecordInsert(hash string, size uint64) {
	lru.mutex.Lock()
	defer lru.mutex.Unlock()

	// Chunk hash already existed before => Update chink size and put corresponding existing lruEntry on top of recency
	if existing, ok := lru.entries[hash]; ok {
		lru.order.MoveToFront(existing)
		existing.Value.(*lruEntry).size = size

		// Add new lruEntry as new chunk representation directly to the top of recency list
	} else {
		element := lru.order.PushFront(&lruEntry{hash: hash, size: size})
		lru.entries[hash] = element
	}

	// Check if we need to drop the bottom lruEntry elem (and the corresponding entry in cache store)
	lru.evictUntilWithinCapacityLocked()
}

// Updates the access recency for a chunk that was just read in the cache => always invoked after cache hits
func (lru *LRU) RecordAccess(hash string) {
	lru.mutex.Lock()
	defer lru.mutex.Unlock()

	if element, ok := lru.entries[hash]; ok {
		lru.order.MoveToFront(element)
	}
}

// Drops the least recently used chunks in the cluster cache if the cache capacity is exceeded, else does nothing
// => Least-recently = bottom of the lru order list
// requires the caller to hold the lru mutex
func (lru *LRU) evictUntilWithinCapacityLocked() {

	for lru.chunkStore.StoredBytes() > lru.capacity {
		// We're above the allowed cluster cache capacity => Get bottom elem
		oldest := lru.order.Back()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(*lruEntry)

		// Delete bottom elem from lru order list
		lru.order.Remove(oldest)
		// Delete lru elem altogether within LRU
		delete(lru.entries, entry.hash)
		// Delete actual chunk from the cache store
		_ = lru.chunkStore.Delete(entry.hash)
	}
}
