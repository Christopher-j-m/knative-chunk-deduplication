package simulation

import (
	"fmt"
	"math/rand"
	"sort"
)

// The ibm traces only contain a generic app name, so we dont know anything about it
// This function maps every app id from the traces to one of our images by index
// The assignment happens based on the strategy, e.g. by most called apps
func AssignImages(trace *Trace, images []int32, strategy string, seed int64) ([]int32, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("there are no images to assign")
	}

	// Create an app list so we can reorder the apps without changing the apps themselves
	apps := make([]int32, len(trace.Apps))
	for i := range apps {
		apps[i] = int32(i)
	}

	assign := make([]int32, len(apps))
	switch strategy {
	// Sort app by most to less frequently called and sort app id accordingly (by name in case same frequency)
	case "volume":
		sort.SliceStable(apps, func(i, j int) bool {
			first, sec := trace.Counts[apps[i]], trace.Counts[apps[j]]
			if first != sec {
				return first > sec
			}
			return trace.Apps[apps[i]] < trace.Apps[apps[j]]
		})

		// Assigns a container image for each app in the sorted list
		for rank, currApp := range apps {
			assign[currApp] = images[rank%len(images)]
		}

	// Give each app a random image
	case "hash":
		sort.Slice(apps, func(i, j int) bool {
			return trace.Apps[apps[i]] < trace.Apps[apps[j]]
		})

		// Random generator with fixed see for easier debugging (reproduceability)
		rng := rand.New(rand.NewSource(seed))
		for _, currApp := range apps {
			assign[currApp] = images[rng.Intn(len(images))]
		}
	default:
		return nil, fmt.Errorf("unknown image assignment strategy %q", strategy)
	}
	return assign, nil
}
