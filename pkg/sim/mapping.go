package sim

import (
	"fmt"
	"math/rand"
	"sort"
)

// AssignImages maps every distinct app in the trace to one image id. The
// trace does not say which image an app runs, so this mapping is a modeling
// choice; the study reports results across strategies and seeds to show the
// conclusions do not hinge on it.
func AssignImages(invs []Invocation, images []string, strategy string, seed int64) (map[string]string, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("no images to assign")
	}

	counts := make(map[string]int)
	for _, inv := range invs {
		counts[inv.App]++
	}

	apps := make([]string, 0, len(counts))
	for app := range counts {
		apps = append(apps, app)
	}

	assign := make(map[string]string, len(apps))
	switch strategy {
	case "volume":
		// most-invoked apps first, alphabetical tie-break keeps runs reproducible
		sort.Slice(apps, func(i, j int) bool {
			if counts[apps[i]] != counts[apps[j]] {
				return counts[apps[i]] > counts[apps[j]]
			}
			return apps[i] < apps[j]
		})
		for i, app := range apps {
			assign[app] = images[i%len(images)]
		}
	case "hash":
		sort.Strings(apps)
		rng := rand.New(rand.NewSource(seed))
		for _, app := range apps {
			assign[app] = images[rng.Intn(len(images))]
		}
	default:
		return nil, fmt.Errorf("unknown mapping strategy %q", strategy)
	}
	return assign, nil
}
