package sim

import (
	"encoding/json"
	"fmt"
	"os"
)

// Catalog is the image inventory with every chunk interned to a small
// integer, so the engine compares ids instead of hashes and node-local
// sets can be bitsets.
type Catalog struct {
	Images     []string
	Sizes      []uint64
	Sets       [][]uint32
	ImageBytes []uint64

	index map[string]int32
}

type imageChunksFile struct {
	Image  string `json:"image"`
	Chunks []struct {
		Hash string `json:"hash"`
		Size uint64 `json:"size"`
	} `json:"chunks"`
}

// LoadCatalog reads the per-image chunk inventory written by the capture
// script or the catalog generator and interns it. Chunk ids follow first
// appearance across the file, image indices follow file order.
func LoadCatalog(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read catalog: %w", err)
	}

	var images []imageChunksFile
	if err := json.Unmarshal(raw, &images); err != nil {
		return nil, fmt.Errorf("failed to parse catalog: %w", err)
	}

	catalog := &Catalog{index: map[string]int32{}}
	ids := map[string]uint32{}
	for _, img := range images {
		if _, dup := catalog.index[img.Image]; dup {
			return nil, fmt.Errorf("image %s listed twice", img.Image)
		}
		catalog.index[img.Image] = int32(len(catalog.Images))
		catalog.Images = append(catalog.Images, img.Image)

		// the same chunk can appear once per file reference, count it once
		seen := map[uint32]bool{}
		var set []uint32
		var bytes uint64
		for _, c := range img.Chunks {
			id, ok := ids[c.Hash]
			if !ok {
				id = uint32(len(catalog.Sizes))
				ids[c.Hash] = id
				catalog.Sizes = append(catalog.Sizes, c.Size)
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			set = append(set, id)
			bytes += catalog.Sizes[id]
		}
		catalog.Sets = append(catalog.Sets, set)
		catalog.ImageBytes = append(catalog.ImageBytes, bytes)
	}
	return catalog, nil
}

// Index returns the position of an image by name.
func (c *Catalog) Index(image string) (int32, bool) {
	i, ok := c.index[image]
	return i, ok
}

// RefCounts counts how many images hold each chunk, which the refcount
// eviction policy ranks on.
func (c *Catalog) RefCounts() []int {
	refs := make([]int, len(c.Sizes))
	for _, set := range c.Sets {
		for _, id := range set {
			refs[id]++
		}
	}
	return refs
}
