package analysis

// Layer of an image
type LayerInfo struct {
	Digest         string `json:"digest"`
	Size           uint64 `json:"size"`
	CompressedSize uint64 `json:"compressed_size"`
	Image          string `json:"image"`
	Source         string `json:"source"`
}

// One image with all its layers
type ImageLayers struct {
	Image  string      `json:"image"`
	Source string      `json:"source"`
	Layers []LayerInfo `json:"layers"`
}

// Duplicate layer byte duplicates across all images
type LayerOverlapMetrics struct {
	Source         string  `json:"source"`
	TotalLayers    int     `json:"total_layers"`
	UniqueLayers   int     `json:"unique_layers"`
	TotalBytes     uint64  `json:"total_bytes"`
	UniqueBytes    uint64  `json:"unique_bytes"`
	RedundantBytes uint64  `json:"redundant_bytes"`
	DedupRatio     float64 `json:"dedup_ratio_bytes"`
}

// Overlap data between two specific images
type PairwiseLayerOverlap struct {
	ImageA        string  `json:"image_a"`
	ImageB        string  `json:"image_b"`
	Source        string  `json:"source"`
	SharedLayers  int     `json:"shared_layers"`
	SharedBytes   uint64  `json:"shared_bytes"`
	OverlapRatioA float64 `json:"overlap_ratio_a_bytes"`
	OverlapRatioB float64 `json:"overlap_ratio_b_bytes"`
}

// Returns metrics about layers across all images
func ComputeLayerOverlapMetrics(corpus []ImageLayers) LayerOverlapMetrics {
	allLayers := make(map[string]uint64)
	var totalLayers int
	var totalBytes uint64
	source := "" // Image format (nydus, oci)

	// Go over each image
	for _, currImage := range corpus {
		source = currImage.Source

		// ... and each of its layers
		for _, currLayer := range currImage.Layers {
			totalLayers++
			totalBytes += currLayer.Size
			allLayers[currLayer.Digest] = currLayer.Size // Store distinct layers size
		}
	}

	// Sum up the distinct layer bytes
	var uniqueBytes uint64
	for _, size := range allLayers {
		uniqueBytes += size
	}

	// Fraction of all the layer bytes that are duplicates
	var dedupRatio float64
	if totalBytes > 0 {
		dedupRatio = 1.0 - float64(uniqueBytes)/float64(totalBytes)
	}

	return LayerOverlapMetrics{
		Source:         source,
		TotalLayers:    totalLayers,
		UniqueLayers:   len(allLayers),
		TotalBytes:     totalBytes,
		UniqueBytes:    uniqueBytes,
		RedundantBytes: totalBytes - uniqueBytes,
		DedupRatio:     dedupRatio,
	}
}

// Returns overlap data for pairs of images => How much share distinct layers across all images
func ComputePairwiseLayerOverlap(corpus []ImageLayers) []PairwiseLayerOverlap {

	// look-up for each image and its layer sizes
	type layerSet struct {
		digests    map[string]uint64 // layer hash to size (bytes)
		totalBytes uint64            // total size of the image (also bytes)
	}

	// Create the look-up sets for each image
	sets := make([]layerSet, len(corpus))
	for i, currImage := range corpus {

		sets[i] = layerSet{digests: make(map[string]uint64)}
		for _, currLayer := range currImage.Layers {
			sets[i].digests[currLayer.Digest] = currLayer.Size
			sets[i].totalBytes += currLayer.Size
		}
	}

	// Go over each image pair and count what they share on layer-level
	var overlaps []PairwiseLayerOverlap
	// Pick first image
	for i := 0; i < len(corpus); i++ {
		// Pick second image
		for j := i + 1; j < len(corpus); j++ {
			var shared int
			var sharedBytes uint64

			// Go over each layer of the first image
			for digest, size := range sets[i].digests {
				// Check if the second image shares this layer
				if _, ok := sets[j].digests[digest]; ok {
					shared++
					sharedBytes += size
				}
			}

			// Count the fraction of the first image appearing in the second one
			var overlapA, overlapB float64
			if sets[i].totalBytes > 0 {
				overlapA = float64(sharedBytes) / float64(sets[i].totalBytes)
			}
			// Fraction of the second image being in the first image
			if sets[j].totalBytes > 0 {
				overlapB = float64(sharedBytes) / float64(sets[j].totalBytes)
			}

			overlaps = append(overlaps, PairwiseLayerOverlap{
				ImageA:        corpus[i].Image,
				ImageB:        corpus[j].Image,
				Source:        corpus[i].Source,
				SharedLayers:  shared,
				SharedBytes:   sharedBytes,
				OverlapRatioA: overlapA,
				OverlapRatioB: overlapB,
			})
		}
	}
	return overlaps
}
