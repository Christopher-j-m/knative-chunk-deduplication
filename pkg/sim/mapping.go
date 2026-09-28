package sim

import (
	"fmt"
	"math/rand"
	"sort"
)

// AssignImages maps every app id in the trace to one image index. The
// trace does not say which image an app runs, so this mapping is a modeling
// choice; the study reports results across strategies and seeds to show the
// conclusions do not hinge on it.
func AssignImages(trace *Trace, images []int32, strategy string, seed int64) ([]int32, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("no images to assign")
	}

	apps := make([]int32, len(trace.Apps))
	for i := range apps {
		apps[i] = int32(i)
	}

	assign := make([]int32, len(apps))
	switch strategy {
	case "volume":
		// most-invoked apps first, name tie-break keeps runs reproducible
		sort.SliceStable(apps, func(i, j int) bool {
			ci, cj := trace.Counts[apps[i]], trace.Counts[apps[j]]
			if ci != cj {
				return ci > cj
			}
			return trace.Apps[apps[i]] < trace.Apps[apps[j]]
		})
		for rank, app := range apps {
			assign[app] = images[rank%len(images)]
		}
	case "hash":
		sort.Slice(apps, func(i, j int) bool { return trace.Apps[apps[i]] < trace.Apps[apps[j]] })
		rng := rand.New(rand.NewSource(seed))
		for _, app := range apps {
			assign[app] = images[rng.Intn(len(images))]
		}
	default:
		return nil, fmt.Errorf("unknown mapping strategy %q", strategy)
	}
	return assign, nil
}
