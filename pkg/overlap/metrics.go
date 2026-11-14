package overlap

type CorpusMetrics struct {
	TotalChunks      int     `json:"total_chunks"`
	UniqueChunks     int     `json:"unique_chunks"`
	TotalBytes       uint64  `json:"total_bytes"`
	UniqueBytes      uint64  `json:"unique_bytes"`
	RedundantBytes   uint64  `json:"redundant_bytes"`
	DedupRatioChunks float64 `json:"dedup_ratio_chunks"`
	DedupRatioBytes  float64 `json:"dedup_ratio_bytes"`
}

func (m CorpusMetrics) DedupRatioByChunks() float64 {
	if m.TotalChunks == 0 {
		return 0
	}
	return 1.0 - float64(m.UniqueChunks)/float64(m.TotalChunks)
}

func (m CorpusMetrics) DedupRatioByBytes() float64 {
	if m.TotalBytes == 0 {
		return 0
	}
	return 1.0 - float64(m.UniqueBytes)/float64(m.TotalBytes)
}

type PairwiseOverlap struct {
	ImageA          string  `json:"image_a"`
	ImageB          string  `json:"image_b"`
	SharedChunks    int     `json:"shared_chunks"`
	SharedBytes     uint64  `json:"shared_bytes"`
	OverlapRatioA   float64 `json:"overlap_ratio_a"`
	OverlapRatioB   float64 `json:"overlap_ratio_b"`
	OverlapByBytesA float64 `json:"overlap_by_bytes_a"`
	OverlapByBytesB float64 `json:"overlap_by_bytes_b"`
}

type CoveragePoint struct {
	ImageIndex             int     `json:"image_index"`
	ImageName              string  `json:"image_name"`
	PotentialHitRateChunks float64 `json:"potential_hit_rate_chunks"`
	PotentialHitRateBytes  float64 `json:"potential_hit_rate_bytes"`
	NewChunks              int     `json:"new_chunks"`
	NewBytes               uint64  `json:"new_bytes"`
	CachedChunks           int     `json:"cached_chunks"`
	CachedBytes            uint64  `json:"cached_bytes"`
}

type CoverageScenario struct {
	Name  string          `json:"name"`
	Order []string        `json:"order"`
	Curve []CoveragePoint `json:"curve"`
}

type ChunkFrequency struct {
	Hash     string `json:"hash"`
	Size     uint64 `json:"size"`
	RefCount int    `json:"ref_count"`
}
