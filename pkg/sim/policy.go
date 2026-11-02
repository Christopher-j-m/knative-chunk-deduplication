package sim

import (
	"container/heap"
	"container/list"
	"fmt"
)

// Decider, based on the chosen eviction policy for a simulation run, what chunks
// get thrown out when reaching cache capacity
type Policy interface {
	Name() string // Returns name of the current eviction policy

	// When a specific chunk is read or added from/in cache
	OnInsert(id uint32, size uint64)
	OnHit(id uint32)

	Evict() (id uint32, size uint64, ok bool) // Returns a chunk that gets thrown out
}

// Build the eviction policy given by input
// The refCounts slice is only needed one strategy that is based on how many images share chunks
func NewPolicy(name string, refCounts []int) (Policy, error) {
	switch name {
	case "lru":
		return newLRUPolicy(), nil
	case "lru2":
		return newLRU2Policy(), nil
	case "size":
		return newSizePolicy(), nil
	case "gdsf":
		return newGDSFPolicy(), nil
	case "refcount":
		return newRefCountPolicy(refCounts), nil
	default:
		return nil, fmt.Errorf("unknown eviction strat %q", name)
	}
}

// A single chunk in the internal eviction policy order
type heapEntry[T any] struct {
	id    uint32 // chunk number
	value T      // metrics that are relevant for the eviction policy, e.g. last-fetched time
	index int    // Position of this chunk/entry in the internal eviction policy order
}

// Internal order based on the eviction policy
// The top element in items is always the one chunk that will be evicted next
type policyHeap[T any] struct {
	items []*heapEntry[T]               // Chunk entries
	less  func(a, b *heapEntry[T]) bool // Comparison function to determine between two chunk entries, which to evict first
}

func newPolicyHeap[T any](less func(a, b *heapEntry[T]) bool) *policyHeap[T] {
	return &policyHeap[T]{less: less}
}

// Returns amount of chunk entries in the head
func (h *policyHeap[T]) Len() int { return len(h.items) }

// Returns whether chunk at index i should be evicted before the one at index j
func (h *policyHeap[T]) Less(i, j int) bool { return h.less(h.items[i], h.items[j]) }

// Swap the position/index of two chunk entries in the heap
func (h *policyHeap[T]) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].index = i
	h.items[j].index = j
}

// Add a chunk entry to the end of the internal heap
// the go heap lib automatically sorts it (based on less function) after pushing it to the head
func (h *policyHeap[T]) Push(x any) {
	entry := x.(*heapEntry[T])
	entry.index = len(h.items)
	h.items = append(h.items, entry)
}

// Remove and return the last entry from the heap
func (h *policyHeap[T]) Pop() any {

	// Remove the last item from the heap
	last := len(h.items) - 1
	entry := h.items[last]
	h.items[last] = nil

	// Update the heap entry list without the just removed item
	h.items = h.items[:last]
	entry.index = -1

	return entry
}

// Add a new chunk entry to the internal heap
func (h *policyHeap[T]) add(id uint32, value T) *heapEntry[T] {
	entry := &heapEntry[T]{id: id, value: value}
	heap.Push(h, entry)
	return entry
}

// Fix the position of a given chunk entry
func (h *policyHeap[T]) repair(entry *heapEntry[T]) {
	heap.Fix(h, entry.index)
}

// Removes and returns the next chunk entry that needs to be evicted
// => The top elem is always the one that is next for eviction so we just pop it
func (h *policyHeap[T]) takeVictim() (*heapEntry[T], bool) {
	if len(h.items) == 0 {
		return nil, false
	}
	return heap.Pop(h).(*heapEntry[T]), true
}

/* LRU Policy: least recently used gets evicted */

// LRU used a list instead of heap => Top elem is least recently used chunk
type lruPolicy struct {
	order   *list.List
	entries map[uint32]*list.Element
}

type lruItem struct {
	id   uint32
	size uint64
}

func newLRUPolicy() *lruPolicy {
	return &lruPolicy{order: list.New(), entries: map[uint32]*list.Element{}}
}

func (lru *lruPolicy) Name() string { return "lru" }

// Push new elem on top of list
func (lru *lruPolicy) OnInsert(id uint32, size uint64) {
	if element, ok := lru.entries[id]; ok {
		lru.order.MoveToFront(element)
		return
	}
	lru.entries[id] = lru.order.PushFront(&lruItem{id: id, size: size})
}

// Move used chunk to top of list
func (lru *lruPolicy) OnHit(id uint32) {
	if element, ok := lru.entries[id]; ok {
		lru.order.MoveToFront(element)
	}
}

// Evict last item in list
func (lru *lruPolicy) Evict() (uint32, uint64, bool) {
	back := lru.order.Back()
	if back == nil {
		return 0, 0, false
	}
	item := back.Value.(*lruItem)
	lru.order.Remove(back)
	delete(lru.entries, item.id)
	return item.id, item.size, true
}

/* LRU2 Policy: second to last use is oldest */
// => example: insert a, access a, insert b, insert c => a was accessed once, b & c not and c was just inserted last => evict b
// in LRU, a would be evicted even though it was already accessed/hit once (b & c only got inserted, but not hit afterwards)
// prev = previous hit
// last = time of the most recent use
type lru2Policy struct {
	clock   int64
	order   *policyHeap[lru2Item]
	entries map[uint32]*heapEntry[lru2Item]
}

type lru2Item struct {
	size       uint64
	last, prev int64
}

// Sort each chunk entry by second to last use with tiebreaker at last use
func newLRU2Policy() *lru2Policy {
	order := newPolicyHeap(func(a, b *heapEntry[lru2Item]) bool {
		if a.value.prev != b.value.prev {
			return a.value.prev < b.value.prev
		}
		return a.value.last < b.value.last
	})
	return &lru2Policy{order: order, entries: map[uint32]*heapEntry[lru2Item]{}}
}

func (lru2 *lru2Policy) Name() string { return "lru2" }

// A chunk entry got used => Update last & prev and re-order entry
func (lru2 *lru2Policy) touch(id uint32, size uint64) {
	lru2.clock++
	// Existing entry
	if entry, ok := lru2.entries[id]; ok {
		entry.value.prev = entry.value.last
		entry.value.last = lru2.clock
		lru2.order.repair(entry)
		return
	}

	// New entry => no prev hit possible
	lru2.entries[id] = lru2.order.add(id, lru2Item{size: size, last: lru2.clock, prev: 0})
}

func (lru2 *lru2Policy) OnInsert(id uint32, size uint64) { lru2.touch(id, size) }

func (lru2 *lru2Policy) OnHit(id uint32) {
	if entry, ok := lru2.entries[id]; ok {
		lru2.touch(id, entry.value.size)
	}
}

func (lru2 *lru2Policy) Evict() (uint32, uint64, bool) {
	victim, ok := lru2.order.takeVictim()
	if !ok {
		return 0, 0, false
	}
	delete(lru2.entries, victim.id)
	return victim.id, victim.value.size, true
}

/* Remove biggest chunk first */
// Biggest chunk is at top of order
type sizePolicy struct {
	clock   int64 // Tiebreaker in case two chunks are the same size => least-recently used gets evicted first
	order   *policyHeap[sizeItem]
	entries map[uint32]*heapEntry[sizeItem] // Lookup chunk id -> heap entry in order
}

type sizeItem struct {
	size uint64
	last int64 // Last used time
}

func newSizePolicy() *sizePolicy {
	order := newPolicyHeap(func(a, b *heapEntry[sizeItem]) bool {
		if a.value.size != b.value.size {
			return a.value.size > b.value.size
		}
		return a.value.last < b.value.last
	})
	return &sizePolicy{order: order, entries: map[uint32]*heapEntry[sizeItem]{}}
}

func (sizeStrategy *sizePolicy) Name() string { return "size" }

func (sizeStrategy *sizePolicy) OnInsert(id uint32, size uint64) {
	sizeStrategy.clock++
	if entry, ok := sizeStrategy.entries[id]; ok {
		entry.value = sizeItem{size: size, last: sizeStrategy.clock}
		sizeStrategy.order.repair(entry)
		return
	}
	sizeStrategy.entries[id] = sizeStrategy.order.add(id, sizeItem{size: size, last: sizeStrategy.clock})
}

func (sizeStrategy *sizePolicy) OnHit(id uint32) {
	sizeStrategy.clock++
	if entry, ok := sizeStrategy.entries[id]; ok {
		entry.value.last = sizeStrategy.clock
		sizeStrategy.order.repair(entry)
	}
}

func (sizeStrategy *sizePolicy) Evict() (uint32, uint64, bool) {
	victim, ok := sizeStrategy.order.takeVictim()
	if !ok {
		return 0, 0, false
	}
	delete(sizeStrategy.entries, victim.id)
	return victim.id, victim.value.size, true
}

/* Remove the chunk first that is shared by the fewest images */
type refCountPolicy struct {
	clock   int64                          // Tiebreaker when two have the same ref count
	refs    []int                          // list indexed with chunk ids and the amount of images they are part of
	order   *policyHeap[refItem]           // chunks with fewest image refs at top (evicted first)
	entries map[uint32]*heapEntry[refItem] // lookup chunk id - heap entry
}

type refItem struct {
	size uint64
	last int64
	refs int
}

func newRefCountPolicy(refs []int) *refCountPolicy {
	order := newPolicyHeap(func(a, b *heapEntry[refItem]) bool {
		if a.value.refs != b.value.refs {
			return a.value.refs < b.value.refs
		}
		return a.value.last < b.value.last
	})
	return &refCountPolicy{refs: refs, order: order, entries: map[uint32]*heapEntry[refItem]{}}
}

func (refCountStrat *refCountPolicy) Name() string { return "refcount" }

// returns how many images contain the specific chunk (by id)
func (refCountStrat *refCountPolicy) refOf(id uint32) int {
	if int(id) < len(refCountStrat.refs) {
		return refCountStrat.refs[id]
	}
	return 1 // minimum part of one image, otherwise this chunk shouldnt appear at all
}

func (refCountStrat *refCountPolicy) OnInsert(id uint32, size uint64) {
	refCountStrat.clock++
	if entry, ok := refCountStrat.entries[id]; ok {
		entry.value = refItem{size: size, last: refCountStrat.clock, refs: refCountStrat.refOf(id)}
		refCountStrat.order.repair(entry)
		return
	}
	refCountStrat.entries[id] = refCountStrat.order.add(id, refItem{size: size, last: refCountStrat.clock, refs: refCountStrat.refOf(id)})
}

func (refCountStrat *refCountPolicy) OnHit(id uint32) {
	refCountStrat.clock++
	if entry, ok := refCountStrat.entries[id]; ok {
		entry.value.last = refCountStrat.clock
		refCountStrat.order.repair(entry)
	}
}

func (refCountStrat *refCountPolicy) Evict() (uint32, uint64, bool) {
	victim, ok := refCountStrat.order.takeVictim()
	if !ok {
		return 0, 0, false
	}
	delete(refCountStrat.entries, victim.id)
	return victim.id, victim.value.size, true
}

/* greedy-dual size-frequency: scores each chunk via clock + hits / size */
// Big and rarely used chunks are evicted before small and often requested chunks
// => Factor size in to the access frequency, since removing bigger ones first allows us to cache more smaller ones,
// but removing big chunks is costly as well, since this is where the cache can speed up things the most...
type gdsfPolicy struct {
	clock    float64
	admitted int64                 // counter that increases for each insertion => used as tiebreaker
	order    *policyHeap[gdsfItem] // lowest score is at top
	entries  map[uint32]*heapEntry[gdsfItem]
}

type gdsfItem struct {
	size     uint64
	hits     float64
	priority float64 // score -> clock + hits / size, lowest gets evicted first
	admitted int64   // insertion order for this chunk
}

func newGDSFPolicy() *gdsfPolicy {
	order := newPolicyHeap(func(a, b *heapEntry[gdsfItem]) bool {
		if a.value.priority != b.value.priority {
			return a.value.priority < b.value.priority
		}
		return a.value.admitted < b.value.admitted
	})
	return &gdsfPolicy{order: order, entries: map[uint32]*heapEntry[gdsfItem]{}}
}

func (gdsfStrategy *gdsfPolicy) Name() string { return "gdsf" }

func (gdsfStrategy *gdsfPolicy) OnInsert(id uint32, size uint64) {
	// a zero-size chunk would divide by zero and outrank everything
	if size == 0 {
		size = 1
	}

	gdsfStrategy.admitted++
	item := gdsfItem{size: size, hits: 1, admitted: gdsfStrategy.admitted}
	item.priority = gdsfStrategy.clock + item.hits/float64(item.size)

	if entry, ok := gdsfStrategy.entries[id]; ok {
		entry.value = item
		gdsfStrategy.order.repair(entry)
		return
	}

	gdsfStrategy.entries[id] = gdsfStrategy.order.add(id, item)
}

func (gdsfStrategy *gdsfPolicy) OnHit(id uint32) {
	entry, ok := gdsfStrategy.entries[id]
	if !ok {
		return
	}

	entry.value.hits++
	entry.value.priority = gdsfStrategy.clock + entry.value.hits/float64(entry.value.size)
	gdsfStrategy.order.repair(entry)
}

func (gdsfStrategy *gdsfPolicy) Evict() (uint32, uint64, bool) {
	victim, ok := gdsfStrategy.order.takeVictim()
	if !ok {
		return 0, 0, false
	}

	gdsfStrategy.clock = victim.value.priority
	delete(gdsfStrategy.entries, victim.id)
	return victim.id, victim.value.size, true
}
