package analysis

import (
	"fmt"
	"sort"

	"nydus-cluster-cache/pkg/nydus"
)

// Returns the total/unique chunks/bytes across the whole set of images we use
func ComputeCorpusMetrics(corpus []nydus.ImageChunks, selector ChunkSelector) CorpusMetrics {
	allChunks := make(map[string]uint64)
	var totalChunks int
	var totalBytes uint64

	for _, currImage := range corpus {
		for _, currChunk := range selector(currImage) {
			totalChunks++
			totalBytes += currChunk.Size
			allChunks[currChunk.Hash] = currChunk.Size
		}
	}

	var uniqueBytes uint64
	for _, size := range allChunks {
		uniqueBytes += size
	}

	metrics := CorpusMetrics{
		TotalChunks:    totalChunks,
		UniqueChunks:   len(allChunks),
		TotalBytes:     totalBytes,
		UniqueBytes:    uniqueBytes,
		RedundantBytes: totalBytes - uniqueBytes,
	}
	metrics.DedupRatioChunks = metrics.DedupRatioByChunks()
	metrics.DedupRatioBytes = metrics.DedupRatioByBytes()
	return metrics
}

// Walks over each image pair and count what they share on chunk-level
// => Does the same as ComputePairwiseLayerOverlap, but on chunk level
func ComputePairwiseOverlap(corpus []nydus.ImageChunks, selector ChunkSelector) []PairwiseOverlap {

	// look-up for each image and its chunk sizes
	type chunkSet struct {
		hashes     map[string]uint64
		totalBytes uint64
	}

	// Build a hash set per image once so the inner pair loop only does only lookups anymore
	sets := make([]chunkSet, len(corpus))
	for i, currImage := range corpus {
		sets[i] = chunkSet{hashes: make(map[string]uint64)}
		for _, currChunk := range selector(currImage) {
			sets[i].hashes[currChunk.Hash] = currChunk.Size
			sets[i].totalBytes += currChunk.Size
		}
	}

	var overlaps []PairwiseOverlap
	// First image
	for i := 0; i < len(corpus); i++ {
		// Second Image
		for j := i + 1; j < len(corpus); j++ {
			var shared int
			var sharedBytes uint64
			for hash, size := range sets[i].hashes {
				// Check for same chunk hash => Shared
				if _, ok := sets[j].hashes[hash]; ok {
					shared++
					sharedBytes += size
				}
			}

			var ratioA, ratioB, byBytesA, byBytesB float64
			// Fraction of chunks within the first image that are also in the second image
			if len(sets[i].hashes) > 0 {
				ratioA = float64(shared) / float64(len(sets[i].hashes))
			}
			// Fraction of chunks within the second image that are also in the first image
			if len(sets[j].hashes) > 0 {
				ratioB = float64(shared) / float64(len(sets[j].hashes))
			}
			// Bytes of ratioA
			if sets[i].totalBytes > 0 {
				byBytesA = float64(sharedBytes) / float64(sets[i].totalBytes)
			}
			// Bytes of ratioB
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

// Simulate a warming cache when images are pulled in the given order
// Each point on the curve records what fraction of that image's chunks were already in the cache,
// showing how quickly reuse is happening as the simulated node sees more workloads
func ComputeCoverageCurve(corpus []nydus.ImageChunks, selector ChunkSelector) []CoveragePoint {
	cached := make(map[string]uint64)
	var curve []CoveragePoint

	for i, currImage := range corpus {
		// Deduplicate within the image itself before checking the shared cache
		imageChunks := make(map[string]uint64)
		for _, currChunk := range selector(currImage) {
			imageChunks[currChunk.Hash] = currChunk.Size
		}

		var hitChunks int
		var hitBytes uint64
		var totalBytes uint64
		// Count the total image chunk size & how many of them are already cached
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
		// Calculate the hit rates
		if totalChunks > 0 {
			hitRateChunks = float64(hitChunks) / float64(totalChunks)
		}
		if totalBytes > 0 {
			hitRateBytes = float64(hitBytes) / float64(totalBytes)
		}

		curve = append(curve, CoveragePoint{
			ImageIndex:             i,
			ImageName:              currImage.Image,
			PotentialHitRateChunks: hitRateChunks,
			PotentialHitRateBytes:  hitRateBytes,
			NewChunks:              totalChunks - hitChunks,
			NewBytes:               totalBytes - hitBytes,
			CachedChunks:           hitChunks,
			CachedBytes:            hitBytes,
		})

		// Add the curr image iteration to the cache (atleast simulate to do so...)
		for hash, size := range imageChunks {
			cached[hash] = size
		}
	}

	return curve
}

// Runs ComputeCoverageCurve with multiple strategies/order (e.g. most shared images first) for comparison
func ComputeCoverageCurveMultiOrder(corpus []nydus.ImageChunks, orders []CoverageOrder, selector ChunkSelector) ([]CoverageScenario, error) {
	// Lookup table for each image by name and its chunks
	imageMap := make(map[string]nydus.ImageChunks, len(corpus))
	for _, currImage := range corpus {
		imageMap[currImage.Image] = currImage
	}

	var results []CoverageScenario
	for _, currOrder := range orders {
		// Reorder the images based on the given input order
		ordered := make([]nydus.ImageChunks, 0, len(currOrder.Order))
		for _, name := range currOrder.Order {
			currImage, ok := imageMap[name]
			if !ok {
				return nil, fmt.Errorf("order %q references image %q not found in the images", currOrder.Name, name)
			}
			ordered = append(ordered, currImage)
		}

		// Run the simulated run
		curve := ComputeCoverageCurve(ordered, selector)
		results = append(results, CoverageScenario{
			Name:  currOrder.Name,
			Order: currOrder.Order,
			Curve: curve,
		})
	}
	return results, nil
}

// Pair a scenario name with the image pull order it represents
type CoverageOrder struct {
	Name  string
	Order []string
}

// Returns how many of our images reference each chunk
// Sorted by descending frequency, so first elem is the most used chunk
func ComputeChunkFrequencies(corpus []nydus.ImageChunks, selector ChunkSelector) []ChunkFrequency {
	// Store for a given chunk its size and usage across multiple images
	type chunkMeta struct {
		size   uint64
		images map[string]struct{}
	}

	chunks := make(map[string]*chunkMeta)

	for _, currImage := range corpus {
		for _, currChunk := range selector(currImage) {
			if _, ok := chunks[currChunk.Hash]; !ok {
				chunks[currChunk.Hash] = &chunkMeta{size: currChunk.Size, images: make(map[string]struct{})}
			}
			// Use a set by image name so a chunk seen twice in the same image counts only once
			chunks[currChunk.Hash].images[currImage.Image] = struct{}{}
		}
	}

	freqs := make([]ChunkFrequency, 0, len(chunks))
	for hash, meta := range chunks {
		freqs = append(freqs, ChunkFrequency{
			Hash:     hash,
			Size:     meta.size,
			RefCount: len(meta.images), // Amount of images that contain this chunk
		})
	}

	// Sort the result by frequency descending order
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
