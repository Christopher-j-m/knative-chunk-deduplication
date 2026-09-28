package sim

import (
	"fmt"
	"math/rand"
)

// Placement decides which node a cold-starting pod lands on. It is the
// modelling choice that matters most on a large cluster, because it sets
// how often a pod finds its image already on the node.
type Placement interface {
	Name() string
	Place(podsPerNode []int) int
}

// NewPlacement builds a placement rule by name: packed spreads by pod
// count but keeps to the lowest indices on ties, the way a least-allocated
// scheduler behaves on a cluster with spare nodes; random ignores load and
// is the worst case for node-local caching.
func NewPlacement(name string, seed int64) (Placement, error) {
	switch name {
	case "packed":
		return packedPlacement{}, nil
	case "random":
		return &randomPlacement{rng: rand.New(rand.NewSource(seed))}, nil
	default:
		return nil, fmt.Errorf("unknown placement %q", name)
	}
}

type packedPlacement struct{}

func (packedPlacement) Name() string { return "packed" }

func (packedPlacement) Place(podsPerNode []int) int {
	node := 0
	for n := 1; n < len(podsPerNode); n++ {
		if podsPerNode[n] < podsPerNode[node] {
			node = n
		}
	}
	return node
}

type randomPlacement struct {
	rng *rand.Rand
}

func (*randomPlacement) Name() string { return "random" }

func (p *randomPlacement) Place(podsPerNode []int) int {
	return p.rng.Intn(len(podsPerNode))
}
