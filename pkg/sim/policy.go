package sim

import (
	"container/heap"
	"container/list"
	"fmt"
)

// Policy owns eviction order for the simulated cluster cache. The engine
// calls OnInsert and OnHit as chunks move, and Evict when over capacity.
type Policy interface {
	Name() string
	OnInsert(id uint32, size uint64)
	OnHit(id uint32)
	Evict() (id uint32, size uint64, ok bool)
}

// NewPolicy builds an eviction policy by name. refCounts is indexed by chunk
// id, is only used by the refcount policy and may be nil for the others.
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
		return nil, fmt.Errorf("unknown eviction policy %q", name)
	}
}

// heapEntry is one resident chunk inside an ordered policy. The index is
// where the entry currently sits in the heap, which is what lets a policy
// repair the order after a hit changes the chunk's standing.
type heapEntry[T any] struct {
	id    uint32
	value T
	index int
}

// policyHeap keeps the next victim at the root, so a policy picks one in
// logarithmic time rather than by walking every resident chunk. The ordering
// comes from the policy and has to be total, or two runs could disagree.
type policyHeap[T any] struct {
	items []*heapEntry[T]
	less  func(a, b *heapEntry[T]) bool
}

func newPolicyHeap[T any](less func(a, b *heapEntry[T]) bool) *policyHeap[T] {
	return &policyHeap[T]{less: less}
}

func (h *policyHeap[T]) Len() int { return len(h.items) }

func (h *policyHeap[T]) Less(i, j int) bool { return h.less(h.items[i], h.items[j]) }

func (h *policyHeap[T]) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].index = i
	h.items[j].index = j
}

func (h *policyHeap[T]) Push(x any) {
	entry := x.(*heapEntry[T])
	entry.index = len(h.items)
	h.items = append(h.items, entry)
}

func (h *policyHeap[T]) Pop() any {
	last := len(h.items) - 1
	entry := h.items[last]
	h.items[last] = nil
	h.items = h.items[:last]
	entry.index = -1
	return entry
}

// add puts a chunk into the order and hands back its entry so the policy can
// keep it in its own lookup table
func (h *policyHeap[T]) add(id uint32, value T) *heapEntry[T] {
	entry := &heapEntry[T]{id: id, value: value}
	heap.Push(h, entry)
	return entry
}

// repair restores the order after the caller changed what an entry ranks on
func (h *policyHeap[T]) repair(entry *heapEntry[T]) { heap.Fix(h, entry.index) }

// takeVictim removes and returns the chunk that ranks first for eviction
func (h *policyHeap[T]) takeVictim() (*heapEntry[T], bool) {
	if len(h.items) == 0 {
		return nil, false
	}
	return heap.Pop(h).(*heapEntry[T]), true
}

// lruPolicy mirrors the live cache: least recently used chunk goes first
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

func (p *lruPolicy) Name() string { return "lru" }

func (p *lruPolicy) OnInsert(id uint32, size uint64) {
	if el, ok := p.entries[id]; ok {
		p.order.MoveToFront(el)
		return
	}
	p.entries[id] = p.order.PushFront(&lruItem{id: id, size: size})
}

func (p *lruPolicy) OnHit(id uint32) {
	if el, ok := p.entries[id]; ok {
		p.order.MoveToFront(el)
	}
}

func (p *lruPolicy) Evict() (uint32, uint64, bool) {
	back := p.order.Back()
	if back == nil {
		return 0, 0, false
	}
	item := back.Value.(*lruItem)
	p.order.Remove(back)
	delete(p.entries, item.id)
	return item.id, item.size, true
}

// lru2Policy evicts the chunk whose second-most-recent access lies furthest
// in the past, which protects chunks with a history of reuse from being
// flushed by one-off scans
type lru2Policy struct {
	clock   int64
	order   *policyHeap[lru2Item]
	entries map[uint32]*heapEntry[lru2Item]
}

type lru2Item struct {
	size       uint64
	last, prev int64
}

func newLRU2Policy() *lru2Policy {
	order := newPolicyHeap(func(a, b *heapEntry[lru2Item]) bool {
		if a.value.prev != b.value.prev {
			return a.value.prev < b.value.prev
		}
		return a.value.last < b.value.last
	})
	return &lru2Policy{order: order, entries: map[uint32]*heapEntry[lru2Item]{}}
}

func (p *lru2Policy) Name() string { return "lru2" }

func (p *lru2Policy) touch(id uint32, size uint64) {
	p.clock++
	if entry, ok := p.entries[id]; ok {
		entry.value.prev = entry.value.last
		entry.value.last = p.clock
		p.order.repair(entry)
		return
	}
	// a fresh insert has no previous access, model that as the epoch
	p.entries[id] = p.order.add(id, lru2Item{size: size, last: p.clock, prev: 0})
}

func (p *lru2Policy) OnInsert(id uint32, size uint64) { p.touch(id, size) }

func (p *lru2Policy) OnHit(id uint32) {
	if entry, ok := p.entries[id]; ok {
		p.touch(id, entry.value.size)
	}
}

func (p *lru2Policy) Evict() (uint32, uint64, bool) {
	victim, ok := p.order.takeVictim()
	if !ok {
		return 0, 0, false
	}
	delete(p.entries, victim.id)
	return victim.id, victim.value.size, true
}

// sizePolicy evicts the largest chunk first, keeping as many distinct
// chunks resident as possible
type sizePolicy struct {
	clock   int64
	order   *policyHeap[sizeItem]
	entries map[uint32]*heapEntry[sizeItem]
}

type sizeItem struct {
	size uint64
	last int64
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

func (p *sizePolicy) Name() string { return "size" }

func (p *sizePolicy) OnInsert(id uint32, size uint64) {
	p.clock++
	if entry, ok := p.entries[id]; ok {
		entry.value = sizeItem{size: size, last: p.clock}
		p.order.repair(entry)
		return
	}
	p.entries[id] = p.order.add(id, sizeItem{size: size, last: p.clock})
}

func (p *sizePolicy) OnHit(id uint32) {
	p.clock++
	if entry, ok := p.entries[id]; ok {
		entry.value.last = p.clock
		p.order.repair(entry)
	}
}

func (p *sizePolicy) Evict() (uint32, uint64, bool) {
	victim, ok := p.order.takeVictim()
	if !ok {
		return 0, 0, false
	}
	delete(p.entries, victim.id)
	return victim.id, victim.value.size, true
}

// refCountPolicy evicts the chunk shared by the fewest images, on the theory
// that widely shared chunks are the ones a cluster cache exists for
type refCountPolicy struct {
	clock   int64
	refs    []int
	order   *policyHeap[refItem]
	entries map[uint32]*heapEntry[refItem]
}

type refItem struct {
	size uint64
	last int64
	// how many images hold the chunk, fixed for the chunk's whole life
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

func (p *refCountPolicy) Name() string { return "refcount" }

// refOf is how many images hold the chunk; a chunk the catalog does not
// cover counts as held once, which is the least a resident chunk can be
func (p *refCountPolicy) refOf(id uint32) int {
	if int(id) < len(p.refs) {
		return p.refs[id]
	}
	return 1
}

func (p *refCountPolicy) OnInsert(id uint32, size uint64) {
	p.clock++
	if entry, ok := p.entries[id]; ok {
		entry.value = refItem{size: size, last: p.clock, refs: p.refOf(id)}
		p.order.repair(entry)
		return
	}
	p.entries[id] = p.order.add(id, refItem{size: size, last: p.clock, refs: p.refOf(id)})
}

func (p *refCountPolicy) OnHit(id uint32) {
	p.clock++
	if entry, ok := p.entries[id]; ok {
		entry.value.last = p.clock
		p.order.repair(entry)
	}
}

func (p *refCountPolicy) Evict() (uint32, uint64, bool) {
	victim, ok := p.order.takeVictim()
	if !ok {
		return 0, 0, false
	}
	delete(p.entries, victim.id)
	return victim.id, victim.value.size, true
}

// gdsfPolicy is greedy-dual size-frequency: every chunk carries a priority of
// clock plus frequency over size, so small chunks that keep being re-fetched
// outlive large cold ones. The clock rises to each victim's priority, which
// ages out credit a chunk earned long ago.
type gdsfPolicy struct {
	clock    float64
	admitted int64
	order    *policyHeap[gdsfItem]
	entries  map[uint32]*heapEntry[gdsfItem]
}

type gdsfItem struct {
	size     uint64
	hits     float64
	priority float64
	// insertion order, so a tie does not leave the victim to map iteration
	admitted int64
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

func (p *gdsfPolicy) Name() string { return "gdsf" }

func (p *gdsfPolicy) OnInsert(id uint32, size uint64) {
	// a zero-size chunk would divide by zero and outrank everything
	if size == 0 {
		size = 1
	}

	p.admitted++
	item := gdsfItem{size: size, hits: 1, admitted: p.admitted}
	item.priority = p.clock + item.hits/float64(item.size)

	if entry, ok := p.entries[id]; ok {
		entry.value = item
		p.order.repair(entry)
		return
	}
	p.entries[id] = p.order.add(id, item)
}

func (p *gdsfPolicy) OnHit(id uint32) {
	entry, ok := p.entries[id]
	if !ok {
		return
	}

	entry.value.hits++
	entry.value.priority = p.clock + entry.value.hits/float64(entry.value.size)
	p.order.repair(entry)
}

func (p *gdsfPolicy) Evict() (uint32, uint64, bool) {
	victim, ok := p.order.takeVictim()
	if !ok {
		return 0, 0, false
	}

	p.clock = victim.value.priority
	delete(p.entries, victim.id)
	return victim.id, victim.value.size, true
}
