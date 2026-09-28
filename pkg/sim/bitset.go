package sim

// bitset is a fixed-size set of chunk ids, one bit each. A node's local
// cache is one of these over the whole catalog, which keeps ten thousand
// nodes at a couple of gigabytes where a map per node would need ten
type bitset []uint64

func newBitset(n int) bitset {
	return make(bitset, (n+63)/64)
}

func (b bitset) has(i uint32) bool {
	return b[i/64]&(1<<(i%64)) != 0
}

func (b bitset) set(i uint32) {
	b[i/64] |= 1 << (i % 64)
}
