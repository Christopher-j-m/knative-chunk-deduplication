package cache

import (
	"container/list"
	"sync"

	"nydus-cluster-cache/pkg/store"
)

// LRU enforces a byte budget on top of a Store. The store is the source of
// truth for payloads; this policy only owns ordering and eviction triggers.
type LRU struct {
	mu       sync.Mutex
	capacity uint64

	order   *list.List
	entries map[string]*list.Element

	backing store.Store
}

type lruEntry struct {
	hash string
	size uint64
}

// NewLRU creates an LRU policy that evicts from backing when the total bytes
// inserted via RecordInsert exceed capacity.
func NewLRU(backing store.Store, capacity uint64) *LRU {
	return &LRU{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element),
		backing:  backing,
	}
}

// RecordInsert notifies the policy that a chunk of the given size was just
// inserted. Triggers eviction synchronously until total tracked bytes fits
// within capacity.
func (l *LRU) RecordInsert(hash string, size uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if existing, ok := l.entries[hash]; ok {
		l.order.MoveToFront(existing)
		existing.Value.(*lruEntry).size = size
	} else {
		element := l.order.PushFront(&lruEntry{hash: hash, size: size})
		l.entries[hash] = element
	}

	l.evictUntilWithinCapacityLocked()
}

// RecordAccess refreshes recency for a hash if present. No-op otherwise.
func (l *LRU) RecordAccess(hash string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if element, ok := l.entries[hash]; ok {
		l.order.MoveToFront(element)
	}
}

// Forget drops a chunk from tracking and from the backing store.
func (l *LRU) Forget(hash string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if element, ok := l.entries[hash]; ok {
		l.order.Remove(element)
		delete(l.entries, hash)
		_ = l.backing.Delete(hash)
	}
}

func (l *LRU) evictUntilWithinCapacityLocked() {
	for l.backing.StoredBytes() > l.capacity {
		oldest := l.order.Back()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(*lruEntry)
		l.order.Remove(oldest)
		delete(l.entries, entry.hash)
		_ = l.backing.Delete(entry.hash)
	}
}
