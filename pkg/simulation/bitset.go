package simulation

// Use a bitset for simulation instead of the real look-up table that the chunk would use in a real setup (to save ram)
type bitset []uint64

// Create a empty bitset that will be able to contain n chunks by reserving 64 bit numbers,
// all with initial state not cached
func newBitset(n int) bitset {
	return make(bitset, (n+63)/64) // Round up
}

// Check for a chunk at i if its cached or not
func (b bitset) has(i uint32) bool {
	// b[i/64] => select the bit number with the requested chunk
	// i%64 => find chunk bit in that number and whether the bit is set or not
	return b[i/64]&(1<<(i%64)) != 0
}

// Mark chunk i as cached
func (b bitset) set(i uint32) {
	// b[i/64] => see has()
	// |= => OR into == set a specific bit on (which is at i % 64)
	b[i/64] |= 1 << (i % 64)
}
