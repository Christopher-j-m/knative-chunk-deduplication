package analysis

import (
	"fmt"
	"sort"

	"nydus-cluster-cache/pkg/nydus"
)

// ComputeCorpusMetrics walks every image in the corpus and counts total vs unique chunks and bytes.
// Redundant bytes are whatever is left after subtracting the unique from the total,
// showing how much data a shared cache could theoretically avoid transferring.
func ComputeCorpusMetrics(corpus []nydus.ImageChunks, sel ChunkSelector) CorpusMetrics {
	allChunks := make(map[string]uint64)
	var totalChunks int
	var totalBytes uint64

	for _, ic := range corpus {
		for _, c := range sel(ic) {
			totalChunks++
			totalBytes += c.Size
			allChunks[c.Hash] = c.Size
		}
	}

	var uniqueBytes uint64
	for _, size := range allChunks {
		uniqueBytes += size
	}

	m := CorpusMetrics{
		TotalChunks:    totalChunks,
		UniqueChunks:   len(allChunks),
		TotalBytes:     totalBytes,
		UniqueBytes:    uniqueBytes,
		RedundantBytes: totalBytes - uniqueBytes,
	}
	m.DedupRatioChunks = m.DedupRatioByChunks()
	m.DedupRatioBytes = m.DedupRatioByBytes()
	return m
}

// ComputePairwiseOverlap computes the chunk sharing between every distinct pair of images in the corpus.
// For each pair it measures both a chunk-count ratio and a byte ratio to show how much of each image's content is shared with the other,
// which helps identify which images would benefit most from a shared cache and how much they have in common with each other.
func ComputePairwiseOverlap(corpus []nydus.ImageChunks, sel ChunkSelector) []PairwiseOverlap {
	type chunkSet struct {
		hashes     map[string]uint64
		totalBytes uint64
	}

	// Build a hash set per image once so the inner pair loop only does only lookups anymore
	sets := make([]chunkSet, len(corpus))
	for i, ic := range corpus {
		sets[i] = chunkSet{hashes: make(map[string]uint64)}
		for _, c := range sel(ic) {
			sets[i].hashes[c.Hash] = c.Size
			sets[i].totalBytes += c.Size
		}
	}

	var overlaps []PairwiseOverlap
	for i := 0; i < len(corpus); i++ {
		for j := i + 1; j < len(corpus); j++ {
			var shared int
			var sharedBytes uint64
			for hash, size := range sets[i].hashes {
				if _, ok := sets[j].hashes[hash]; ok {
					shared++
					sharedBytes += size
				}
			}

			// Avoid division by zero for empty images
			var ratioA, ratioB, byBytesA, byBytesB float64
			if len(sets[i].hashes) > 0 {
				ratioA = float64(shared) / float64(len(sets[i].hashes))
			}
			if len(sets[j].hashes) > 0 {
				ratioB = float64(shared) / float64(len(sets[j].hashes))
			}
			if sets[i].totalBytes > 0 {
				byBytesA = float64(sharedBytes) / float64(sets[i].totalBytes)
			}
			if sets[j].totalBytes > 0 {
				byBytesB = float64(sharedBytes) / float64(sets[j].totalBytes)
			}

			overlaps = append(overlaps, PairwiseOverlap{
				ImageA:          corpus[i].Image,
				ImageB:          corpus[j].Image,
				SharedChunks:    shared,
				SharedBytes:     sharedBytes,
				OverlapRatioA:   ratioA,
				OverlapRatioB:   ratioB,
				OverlapByBytesA: byBytesA,
				OverlapByBytesB: byBytesB,
			})
		}
	}

	return overlaps
}

// Simulate a warming cache as images are pulled one by one in the given order.
// Each point on the curve records what fraction of that image's chunks were already in the cache,
// showing how quickly reuse accumulates as the simulated node sees more workloads.
func ComputeCoverageCurve(corpus []nydus.ImageChunks, sel ChunkSelector) []CoveragePoint {
	cached := make(map[string]uint64)
	var curve []CoveragePoint

	for i, ic := range corpus {
		// Deduplicate within the image itself before checking the shared cache
		imageChunks := make(map[string]uint64)
		for _, c := range sel(ic) {
			imageChunks[c.Hash] = c.Size
		}

		var hitChunks int
		var hitBytes uint64
		var totalBytes uint64
		for hash, size := range imageChunks {
			totalBytes += size
			if _, ok := cached[hash]; ok {
				hitChunks++
				hitBytes += size
			}
		}

		totalChunks := len(imageChunks)
		hitRateChunks := 0.0
		hitRateBytes := 0.0
		if totalChunks > 0 {
			hitRateChunks = float64(hitChunks) / float64(totalChunks)
		}
		if totalBytes > 0 {
			hitRateBytes = float64(hitBytes) / float64(totalBytes)
		}

		curve = append(curve, CoveragePoint{
			ImageIndex:             i,
			ImageName:              ic.Image,
			PotentialHitRateChunks: hitRateChunks,
			PotentialHitRateBytes:  hitRateBytes,
			NewChunks:              totalChunks - hitChunks,
			NewBytes:               totalBytes - hitBytes,
			CachedChunks:           hitChunks,
			CachedBytes:            hitBytes,
		})

		for hash, size := range imageChunks {
			cached[hash] = size
		}
	}

	return curve
}

// Runs ComputeCoverageCurve for each scenario using its own image ordering.
// This lets us compare strategies like "most common images first" against random or workload-specific orderings
// to find which warm-up sequence maximises early cache hit rates.
func ComputeCoverageCurveMultiOrder(corpus []nydus.ImageChunks, scenarios []CoverageScenarioInput, sel ChunkSelector) ([]CoverageScenario, error) {
	imageMap := make(map[string]nydus.ImageChunks, len(corpus))
	for _, ic := range corpus {
		imageMap[ic.Image] = ic
	}

	var results []CoverageScenario
	for _, s := range scenarios {
		// Reorder the corpus according to this scenario's sequence
		ordered := make([]nydus.ImageChunks, 0, len(s.Order))
		for _, name := range s.Order {
			ic, ok := imageMap[name]
			if !ok {
				return nil, fmt.Errorf("scenario %q references image %q not found in corpus", s.Name, name)
			}
			ordered = append(ordered, ic)
		}
		curve := ComputeCoverageCurve(ordered, sel)
		results = append(results, CoverageScenario{
			Name:  s.Name,
			Order: s.Order,
			Curve: curve,
		})
	}
	return results, nil
}

// Pair a scenario name with the image pull order it represents
type CoverageScenarioInput struct {
	Name  string
	Order []string
}

// ComputeChunkFrequencies counts how many distinct images reference each chunk across the corpus.
func ComputeChunkFrequencies(corpus []nydus.ImageChunks, sel ChunkSelector) []ChunkFrequency {
	type chunkMeta struct {
		size   uint64
		images map[string]struct{}
	}

	chunks := make(map[string]*chunkMeta)

	for _, ic := range corpus {
		for _, c := range sel(ic) {
			if _, ok := chunks[c.Hash]; !ok {
				chunks[c.Hash] = &chunkMeta{size: c.Size, images: make(map[string]struct{})}
			}
			// Use a set keyed by image name so a chunk seen twice in the same image counts only once
			chunks[c.Hash].images[ic.Image] = struct{}{}
		}
	}

	freqs := make([]ChunkFrequency, 0, len(chunks))
	for hash, meta := range chunks {
		freqs = append(freqs, ChunkFrequency{
			Hash:     hash,
			Size:     meta.size,
			RefCount: len(meta.images),
		})
	}

	sort.Slice(freqs, func(i, j int) bool {
		if freqs[i].RefCount != freqs[j].RefCount {
			return freqs[i].RefCount > freqs[j].RefCount
		}
		if freqs[i].Size != freqs[j].Size {
			return freqs[i].Size > freqs[j].Size
		}
		return freqs[i].Hash < freqs[j].Hash
	})

	return freqs
}
