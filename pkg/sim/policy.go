package sim

import (
	"container/list"
	"fmt"
)

// Policy owns eviction order for the simulated cluster cache. The engine
// calls OnInsert and OnHit as chunks move, and Evict when over capacity.
type Policy interface {
	Name() string
	OnInsert(hash string, size uint64)
	OnHit(hash string)
	Evict() (hash string, size uint64, ok bool)
}

// NewPolicy builds an eviction policy by name. refCounts is only used by the
// refcount policy and may be nil for the others.
func NewPolicy(name string, refCounts map[string]int) (Policy, error) {
	switch name {
	case "lru":
		return newLRUPolicy(), nil
	case "lru2":
		return newLRU2Policy(), nil
	case "size":
		return newSizePolicy(), nil
	case "refcount":
		return newRefCountPolicy(refCounts), nil
	default:
		return nil, fmt.Errorf("unknown eviction policy %q", name)
	}
}

// lruPolicy mirrors the live cache: least recently used chunk goes first
type lruPolicy struct {
	order   *list.List
	entries map[string]*list.Element
}

type lruItem struct {
	hash string
	size uint64
}

func newLRUPolicy() *lruPolicy {
	return &lruPolicy{order: list.New(), entries: map[string]*list.Element{}}
}

func (p *lruPolicy) Name() string { return "lru" }

func (p *lruPolicy) OnInsert(hash string, size uint64) {
	if el, ok := p.entries[hash]; ok {
		p.order.MoveToFront(el)
		return
	}
	p.entries[hash] = p.order.PushFront(&lruItem{hash: hash, size: size})
}

func (p *lruPolicy) OnHit(hash string) {
	if el, ok := p.entries[hash]; ok {
		p.order.MoveToFront(el)
	}
}

func (p *lruPolicy) Evict() (string, uint64, bool) {
	back := p.order.Back()
	if back == nil {
		return "", 0, false
	}
	item := back.Value.(*lruItem)
	p.order.Remove(back)
	delete(p.entries, item.hash)
	return item.hash, item.size, true
}

// lru2Policy evicts the chunk whose second-most-recent access lies furthest
// in the past, which protects chunks with a history of reuse from being
// flushed by one-off scans
type lru2Policy struct {
	clock   int64
	entries map[string]*lru2Item
}

type lru2Item struct {
	size       uint64
	last, prev int64
}

func newLRU2Policy() *lru2Policy {
	return &lru2Policy{entries: map[string]*lru2Item{}}
}

func (p *lru2Policy) Name() string { return "lru2" }

func (p *lru2Policy) touch(hash string, size uint64) {
	p.clock++
	if item, ok := p.entries[hash]; ok {
		item.prev = item.last
		item.last = p.clock
		return
	}
	// a fresh insert has no previous access, model that as the epoch
	p.entries[hash] = &lru2Item{size: size, last: p.clock, prev: 0}
}

func (p *lru2Policy) OnInsert(hash string, size uint64) { p.touch(hash, size) }

func (p *lru2Policy) OnHit(hash string) {
	if item, ok := p.entries[hash]; ok {
		p.touch(hash, item.size)
	}
}

func (p *lru2Policy) Evict() (string, uint64, bool) {
	var victim string
	var victimItem *lru2Item
	for hash, item := range p.entries {
		if victimItem == nil ||
			item.prev < victimItem.prev ||
			(item.prev == victimItem.prev && item.last < victimItem.last) {
			victim, victimItem = hash, item
		}
	}
	if victimItem == nil {
		return "", 0, false
	}
	delete(p.entries, victim)
	return victim, victimItem.size, true
}

// sizePolicy evicts the largest chunk first, keeping as many distinct
// chunks resident as possible
type sizePolicy struct {
	clock   int64
	entries map[string]*sizeItem
}

type sizeItem struct {
	size uint64
	last int64
}

func newSizePolicy() *sizePolicy {
	return &sizePolicy{entries: map[string]*sizeItem{}}
}

func (p *sizePolicy) Name() string { return "size" }

func (p *sizePolicy) OnInsert(hash string, size uint64) {
	p.clock++
	p.entries[hash] = &sizeItem{size: size, last: p.clock}
}

func (p *sizePolicy) OnHit(hash string) {
	p.clock++
	if item, ok := p.entries[hash]; ok {
		item.last = p.clock
	}
}

func (p *sizePolicy) Evict() (string, uint64, bool) {
	var victim string
	var victimItem *sizeItem
	for hash, item := range p.entries {
		if victimItem == nil ||
			item.size > victimItem.size ||
			(item.size == victimItem.size && item.last < victimItem.last) {
			victim, victimItem = hash, item
		}
	}
	if victimItem == nil {
		return "", 0, false
	}
	delete(p.entries, victim)
	return victim, victimItem.size, true
}

// refCountPolicy evicts the chunk shared by the fewest images, on the theory
// that widely shared chunks are the ones a cluster cache exists for
type refCountPolicy struct {
	clock   int64
	refs    map[string]int
	entries map[string]*refItem
}

type refItem struct {
	size uint64
	last int64
}

func newRefCountPolicy(refs map[string]int) *refCountPolicy {
	if refs == nil {
		refs = map[string]int{}
	}
	return &refCountPolicy{refs: refs, entries: map[string]*refItem{}}
}

func (p *refCountPolicy) Name() string { return "refcount" }

func (p *refCountPolicy) refOf(hash string) int {
	if n, ok := p.refs[hash]; ok {
		return n
	}
	return 1
}

func (p *refCountPolicy) OnInsert(hash string, size uint64) {
	p.clock++
	p.entries[hash] = &refItem{size: size, last: p.clock}
}

func (p *refCountPolicy) OnHit(hash string) {
	p.clock++
	if item, ok := p.entries[hash]; ok {
		item.last = p.clock
	}
}

func (p *refCountPolicy) Evict() (string, uint64, bool) {
	var victim string
	var victimItem *refItem
	for hash, item := range p.entries {
		if victimItem == nil ||
			p.refOf(hash) < p.refOf(victim) ||
			(p.refOf(hash) == p.refOf(victim) && item.last < victimItem.last) {
			victim, victimItem = hash, item
		}
	}
	if victimItem == nil {
		return "", 0, false
	}
	delete(p.entries, victim)
	return victim, victimItem.size, true
}
