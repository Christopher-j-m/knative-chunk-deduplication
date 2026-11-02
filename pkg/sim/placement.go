package sim

import (
	"fmt"
	"math/rand"
)

// Decider on which simulated node a cold starting pod lands on
type Placement interface {
	Name() string                // Name of the placement strat, can be either "packed" or "random"
	Place(podsPerNode []int) int // Retuns the amount of pods running on a specific node
}

// Packed = pick the least busy node
// Random = pick any node randomly
func NewPlacement(name string, seed int64) (Placement, error) {
	switch name {
	case "packed":
		return packedPlacement{}, nil
	case "random":
		return &randomPlacement{rng: rand.New(rand.NewSource(seed))}, nil
	default:
		return nil, fmt.Errorf("unknown placement strat %q", name)
	}
}

type packedPlacement struct{}

func (packedPlacement) Name() string { return "packed" }

// Returns the node that has the fewest pods, tiebreaker goes to the node with the lowest id
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

func (radomStrat *randomPlacement) Place(podsPerNode []int) int {
	return radomStrat.rng.Intn(len(podsPerNode))
}
