package analysis

type LayerInfo struct {
	Digest         string `json:"digest"`
	Size           uint64 `json:"size"`
	CompressedSize uint64 `json:"compressed_size"`
	Image          string `json:"image"`
	Source         string `json:"source"`
}

type ImageLayers struct {
	Image  string      `json:"image"`
	Source string      `json:"source"`
	Layers []LayerInfo `json:"layers"`
}

type LayerOverlapMetrics struct {
	Source         string  `json:"source"`
	TotalLayers    int     `json:"total_layers"`
	UniqueLayers   int     `json:"unique_layers"`
	TotalBytes     uint64  `json:"total_bytes"`
	UniqueBytes    uint64  `json:"unique_bytes"`
	RedundantBytes uint64  `json:"redundant_bytes"`
	DedupRatio     float64 `json:"dedup_ratio_bytes"`
}

type PairwiseLayerOverlap struct {
	ImageA        string  `json:"image_a"`
	ImageB        string  `json:"image_b"`
	Source        string  `json:"source"`
	SharedLayers  int     `json:"shared_layers"`
	SharedBytes   uint64  `json:"shared_bytes"`
	OverlapRatioA float64 `json:"overlap_ratio_a_bytes"`
	OverlapRatioB float64 `json:"overlap_ratio_b_bytes"`
}

func ComputeLayerOverlapMetrics(corpus []ImageLayers) LayerOverlapMetrics {
	allLayers := make(map[string]uint64)
	var totalLayers int
	var totalBytes uint64
	source := ""

	for _, il := range corpus {
		source = il.Source
		for _, l := range il.Layers {
			totalLayers++
			totalBytes += l.Size
			allLayers[l.Digest] = l.Size
		}
	}

	var uniqueBytes uint64
	for _, size := range allLayers {
		uniqueBytes += size
	}

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

func ComputePairwiseLayerOverlap(corpus []ImageLayers) []PairwiseLayerOverlap {
	type layerSet struct {
		digests    map[string]uint64
		totalBytes uint64
	}

	sets := make([]layerSet, len(corpus))
	for i, il := range corpus {
		sets[i] = layerSet{digests: make(map[string]uint64)}
		for _, l := range il.Layers {
			sets[i].digests[l.Digest] = l.Size
			sets[i].totalBytes += l.Size
		}
	}

	var overlaps []PairwiseLayerOverlap
	for i := 0; i < len(corpus); i++ {
		for j := i + 1; j < len(corpus); j++ {
			var shared int
			var sharedBytes uint64
			for digest, size := range sets[i].digests {
				if _, ok := sets[j].digests[digest]; ok {
					shared++
					sharedBytes += size
				}
			}
			var overlapA, overlapB float64
			if sets[i].totalBytes > 0 {
				overlapA = float64(sharedBytes) / float64(sets[i].totalBytes)
			}
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
