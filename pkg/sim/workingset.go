package sim

import (
	"encoding/json"
	"fmt"
	"os"
)

// ChunkRef identifies one chunk of an image's startup working set
type ChunkRef struct {
	Hash string
	Size uint64
}

// WorkingSet is the deduplicated set of chunks an image reads during startup.
type WorkingSet struct {
	Image  string
	Chunks []ChunkRef
	Bytes  uint64
}

type imageChunksFile struct {
	Image  string `json:"image"`
	Chunks []struct {
		Hash string `json:"hash"`
		Size uint64 `json:"size"`
	} `json:"chunks"`
}

// LoadWorkingSets reads the startup chunk inventory written by the analysis
// pipeline and returns one deduplicated working set per image.
func LoadWorkingSets(path string) (map[string]WorkingSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read working sets: %w", err)
	}

	var images []imageChunksFile
	if err := json.Unmarshal(raw, &images); err != nil {
		return nil, fmt.Errorf("failed to parse working sets: %w", err)
	}

	sets := make(map[string]WorkingSet, len(images))
	for _, img := range images {
		// the same chunk can appear once per file reference, count it once
		seen := make(map[string]bool, len(img.Chunks))
		ws := WorkingSet{Image: img.Image}
		for _, c := range img.Chunks {
			if seen[c.Hash] {
				continue
			}
			seen[c.Hash] = true
			ws.Chunks = append(ws.Chunks, ChunkRef{Hash: c.Hash, Size: c.Size})
			ws.Bytes += c.Size
		}
		sets[img.Image] = ws
	}
	return sets, nil
}
