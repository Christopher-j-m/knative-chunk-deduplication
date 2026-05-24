// cmd/overlap-analyzer/main.go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"nydus-cluster-cache/pkg/analysis"
	"nydus-cluster-cache/pkg/nydus"
)

// CorpusConfig holds the image registry and coverage scenario definitions from the corpus JSON file.
// Each image has a role label used for filtering, and scenarios describe the order in which
// images are "seen" to simulate different cache warm-up strategies.
type CorpusConfig struct {
	Images []struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"images"`
	CoverageScenarios []struct {
		Name  string   `json:"name"`
		Scope *string  `json:"scope"`
		Order []string `json:"order"`
	} `json:"coverage_scenarios"`
}

// BlobOverlapOutput captures the side-by-side deduplication results for OCI layers and Nydus blobs.
// ChunkMinusBlobMB shows how much additional space Nydus content-defined chunking saves
// compared to the OCI blob granularity.
type BlobOverlapOutput struct {
	OCIMetrics       analysis.LayerOverlapMetrics    `json:"oci_metrics"`
	NydusMetrics     analysis.LayerOverlapMetrics    `json:"nydus_metrics"`
	OCIPairwise      []analysis.PairwiseLayerOverlap `json:"oci_pairwise,omitempty"`
	NydusPairwise    []analysis.PairwiseLayerOverlap `json:"nydus_pairwise,omitempty"`
	ChunkMinusBlobMB int64                           `json:"chunk_minus_blob_mb"`
}

// Top-level JSON written to file with corpus-wide metrics, per-image-pair sharing, coverage curves, and chunk frequency data.
type AnalysisOutput struct {
	Corpus      analysis.CorpusMetrics      `json:"corpus_metrics"`
	Pairwise    []analysis.PairwiseOverlap  `json:"pairwise_overlap"`
	Scenarios   []analysis.CoverageScenario `json:"coverage_scenarios"`
	Frequencies []analysis.ChunkFrequency   `json:"chunk_frequencies"`
	BlobOverlap *BlobOverlapOutput          `json:"blob_overlap,omitempty"`
}

func main() {
	input := flag.String("input", "analysis/results/chunks.json", "Input chunks JSON")
	corpusFile := flag.String("corpus", "analysis/corpus.json", "Corpus config with scenarios")
	output := flag.String("output", "analysis/results/overlap.json", "Output analysis JSON")
	filterRole := flag.String("filter-role", "", "Filter to images with this role (e.g. knative-workload)")
	useAccessed := flag.Bool("accessed-only", false, "Use accessed chunks only (startup working set)")
	blobOverlap := flag.Bool("blob-overlap", false, "Compute OCI layer and Nydus blob overlap baseline")
	compareFixed := flag.Bool("compare-fixed", false, "Run lambda-style 512 KiB chunking comparison")
	exportDir := flag.String("export-dir", "", "Directory with exported filesystems (required with --compare-fixed)")
	fixedOutput := flag.String("fixed-output", "", "Output JSON for fixed-chunk analysis (required with --compare-fixed)")
	comparisonOutput := flag.String("comparison-output", "", "Output JSON for Nydus vs fixed summary (required with --compare-fixed)")
	flag.Parse()

	if *compareFixed && *useAccessed {
		log.Fatal("--compare-fixed and --accessed-only are incompatible, since fixed corpus have no startup access traces")
	}

	if *compareFixed {
		if *exportDir == "" || *fixedOutput == "" || *comparisonOutput == "" {
			log.Fatal("--export-dir, --fixed-output, and --comparison-output are required in combination with --compare-fixed")
		}
	}

	data, err := os.ReadFile(*input)
	if err != nil {
		log.Fatalf("error reading input: %v", err)
	}

	var corpus []nydus.ImageChunks
	if err := json.Unmarshal(data, &corpus); err != nil {
		log.Fatalf("error parsing input: %v", err)
	}

	// Filter the corpus to images matching the requested role so role-specific analysis stays isolated
	if *filterRole != "" {
		corpusData, err := os.ReadFile(*corpusFile)
		if err != nil {
			log.Fatalf("error reading corpus config: %v", err)
		}

		var config CorpusConfig
		if err := json.Unmarshal(corpusData, &config); err != nil {
			log.Fatalf("error parsing corpus config: %v", err)
		}

		allowedNames := make(map[string]struct{})
		for _, img := range config.Images {
			if img.Role == *filterRole {
				allowedNames[img.ID] = struct{}{}
			}
		}

		var filtered []nydus.ImageChunks
		for _, ic := range corpus {
			if _, ok := allowedNames[ic.Image]; ok {
				filtered = append(filtered, ic)
			}
		}
		fmt.Printf("Filtered to %d images with role=%s\n", len(filtered), *filterRole)
		corpus = filtered
	}

	// Choose between all chunks or only those touched during startup, depending on what we're measuring
	sel := analysis.AllChunks
	if *useAccessed {
		if err := analysis.ValidateAccessData(corpus); err != nil {
			log.Fatalf("--accessed-only requires startup trace data: %v", err)
		}
		sel = analysis.AccessedOnly
	}

	fmt.Printf("Analyzing %d images...\n", len(corpus))

	metrics := analysis.ComputeCorpusMetrics(corpus, sel)
	fmt.Printf("\nCorpus Metrics:\n")
	fmt.Printf("Total chunks: %d\n", metrics.TotalChunks)
	fmt.Printf("Unique chunks: %d\n", metrics.UniqueChunks)
	fmt.Printf("Dedup ratio (chunks): %.1f%%\n", metrics.DedupRatioChunks*100)
	fmt.Printf("Dedup ratio (bytes): %.1f%%\n", metrics.DedupRatioBytes*100)
	fmt.Printf("Total bytes: %d MB\n", metrics.TotalBytes/1024/1024)
	fmt.Printf("Unique bytes: %d MB\n", metrics.UniqueBytes/1024/1024)
	fmt.Printf("Redundant: %d MB\n", metrics.RedundantBytes/1024/1024)

	// Sort pairs by shared bytes descending so the pairs with the potentially most overlap appear first in the output
	pairwise := analysis.ComputePairwiseOverlap(corpus, sel)
	sort.Slice(pairwise, func(i, j int) bool {
		if pairwise[i].SharedBytes != pairwise[j].SharedBytes {
			return pairwise[i].SharedBytes > pairwise[j].SharedBytes
		}
		if pairwise[i].ImageA != pairwise[j].ImageA {
			return pairwise[i].ImageA < pairwise[j].ImageA
		}
		return pairwise[i].ImageB < pairwise[j].ImageB
	})

	fmt.Printf("\nTop 5 image pairs by shared bytes:\n")
	for i, p := range pairwise {
		if i >= 5 {
			break
		}
		fmt.Printf("  %s - %s: %d chunks, %d MB (%.0f%%/%.0f%% by bytes)\n",
			p.ImageA, p.ImageB, p.SharedChunks, p.SharedBytes/1024/1024,
			p.OverlapByBytesA*100, p.OverlapByBytesB*100)
	}

	// Run each coverage scenario to see how quickly a cache fills up as images arrive in different orders
	var scenarios []analysis.CoverageScenario
	corpusData, err := os.ReadFile(*corpusFile)
	if err == nil {
		var config CorpusConfig

		if err := json.Unmarshal(corpusData, &config); err == nil && len(config.CoverageScenarios) > 0 {
			var inputs []analysis.CoverageScenarioInput

			for _, s := range config.CoverageScenarios {

				// Skip scenarios that don't match the active role filter, or skip scoped scenarios when no filter is set
				if *filterRole != "" {
					if s.Scope == nil || *s.Scope != *filterRole {
						continue
					}
				} else {
					if s.Scope != nil {
						continue
					}
				}

				order := s.Order
				if order == nil {

					// Use a seed so results are reproducible without storing the full order somewhere
					seed := int64(0)
					for _, c := range s.Name {
						seed = seed*31 + int64(c)
					}

					rng := rand.New(rand.NewSource(seed))
					order = make([]string, len(corpus))
					for i, ic := range corpus {
						order[i] = ic.Image
					}

					rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
				}
				inputs = append(inputs, analysis.CoverageScenarioInput{Name: s.Name, Order: order})
			}
			var scenarioErr error
			scenarios, scenarioErr = analysis.ComputeCoverageCurveMultiOrder(corpus, inputs, sel)
			if scenarioErr != nil {
				log.Fatalf("error occured : %v", scenarioErr)
			}

			fmt.Printf("\nCoverage:\n")
			for _, s := range scenarios {
				if len(s.Curve) > 0 {
					last := s.Curve[len(s.Curve)-1]
					fmt.Printf("%-20s: %.1f%% (chunks), %.1f%% (bytes)\n",
						s.Name, last.PotentialHitRateChunks*100, last.PotentialHitRateBytes*100)
				}
			}
		}
	}

	// Count how many images reference each chunk to measuring sharing depth across the corpus
	frequencies := analysis.ComputeChunkFrequencies(corpus, sel)
	fmt.Printf("\nChunk frequency distribution:\n")
	refCounts := make(map[int]int)
	for _, f := range frequencies {
		refCounts[f.RefCount]++
	}
	for count := len(corpus); count >= 1; count-- {
		if n, ok := refCounts[count]; ok {
			fmt.Printf("Referenced by %d images: %d chunks\n", count, n)
		}
	}

	// Compare Nydus chunk-level deduplication against the coarser OCI layer and Nydus blob granularity
	var blobOutput *BlobOverlapOutput
	if *blobOverlap {
		// Expect the layer file alongside the chunks file
		layerPath := strings.TrimSuffix(*input, ".json") + "-layers.json"
		layerData, err := os.ReadFile(layerPath)
		if err != nil {
			log.Fatalf("error reading layer data: %v", err)
		}

		var layers struct {
			OCI   []analysis.ImageLayers `json:"oci_layers"`
			Nydus []analysis.ImageLayers `json:"nydus_layers"`
		}
		if err := json.Unmarshal(layerData, &layers); err != nil {
			log.Fatalf("error parsing layer data: %v", err)
		}

		ociMetrics := analysis.ComputeLayerOverlapMetrics(layers.OCI)
		nydusMetrics := analysis.ComputeLayerOverlapMetrics(layers.Nydus)
		ociPairwise := analysis.ComputePairwiseLayerOverlap(layers.OCI)
		nydusPairwise := analysis.ComputePairwiseLayerOverlap(layers.Nydus)

		chunkRedundant := int64(metrics.RedundantBytes)
		blobRedundant := int64(nydusMetrics.RedundantBytes)

		blobOutput = &BlobOverlapOutput{
			OCIMetrics:       ociMetrics,
			NydusMetrics:     nydusMetrics,
			OCIPairwise:      ociPairwise,
			NydusPairwise:    nydusPairwise,
			ChunkMinusBlobMB: (chunkRedundant - blobRedundant) / 1024 / 1024,
		}

		fmt.Printf("\nOCI Layer Overlap...\n")
		fmt.Printf("Total layers: %d, Unique: %d, Dedup ratio: %.1f%%\n", ociMetrics.TotalLayers, ociMetrics.UniqueLayers, ociMetrics.DedupRatio*100)
		fmt.Printf("\nNydus Blob Overlap\n")
		fmt.Printf("Total blobs: %d, Unique: %d, Dedup ratio: %.1f%%\n", nydusMetrics.TotalLayers, nydusMetrics.UniqueLayers, nydusMetrics.DedupRatio*100)
		fmt.Printf("\nChunk vs Blob\n")
		fmt.Printf("Delta (chunk - blob): %+d MB\n", blobOutput.ChunkMinusBlobMB)
	}

	result := AnalysisOutput{
		Corpus:      metrics,
		Pairwise:    pairwise,
		Scenarios:   scenarios,
		Frequencies: frequencies,
		BlobOverlap: blobOutput,
	}

	// Skip writing the Nydus results when we're in comparison mode
	if !*compareFixed {
		out, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			log.Fatalf("failed to marshal output: %v", err)
		}

		if err := os.WriteFile(*output, out, 0o644); err != nil {
			log.Fatalf("error during writing output: %v", err)
		}

		fmt.Printf("\nAnalysis written to %s\n", *output)
	}

	// Re-chunk every image with a fixed 512 KiB boundary
	if *compareFixed {

		fmt.Printf("\nFixed 512 KiB Chunking Comparison...\n")

		var fixedCorpus []nydus.ImageChunks
		for _, ic := range corpus {
			imgDir := filepath.Join(*exportDir, ic.Image)
			chunks, err := nydus.FlattenImage(imgDir, 512*1024, ic.Image)
			if err != nil {
				log.Fatalf("error during flattening %s: %v", ic.Image, err)
			}
			var totalBytes uint64
			for _, c := range chunks {
				totalBytes += c.Size
			}
			fixedCorpus = append(fixedCorpus, nydus.ImageChunks{
				Image:      ic.Image,
				TotalBytes: totalBytes,
				Chunks:     chunks,
			})
			fmt.Printf("%s: %d fixed chunks\n", ic.Image, len(chunks))
		}

		fixedMetrics := analysis.ComputeCorpusMetrics(fixedCorpus, sel)
		fixedPairwise := analysis.ComputePairwiseOverlap(fixedCorpus, sel)
		sort.Slice(fixedPairwise, func(i, j int) bool {
			if fixedPairwise[i].SharedBytes != fixedPairwise[j].SharedBytes {
				return fixedPairwise[i].SharedBytes > fixedPairwise[j].SharedBytes
			}
			if fixedPairwise[i].ImageA != fixedPairwise[j].ImageA {
				return fixedPairwise[i].ImageA < fixedPairwise[j].ImageA
			}
			return fixedPairwise[i].ImageB < fixedPairwise[j].ImageB
		})
		fixedFrequencies := analysis.ComputeChunkFrequencies(fixedCorpus, sel)

		// Coverage scenarios are omitted because fixed chunks have no access traces to simulate startup behaviour
		fixedResult := AnalysisOutput{
			Corpus:      fixedMetrics,
			Pairwise:    fixedPairwise,
			Scenarios:   nil,
			Frequencies: fixedFrequencies,
		}

		fixedJSON, err := json.MarshalIndent(fixedResult, "", "  ")
		if err != nil {
			log.Fatalf("error during marshalling fixed chunk output: %v", err)
		}
		if err := os.WriteFile(*fixedOutput, fixedJSON, 0o644); err != nil {
			log.Fatalf("error writing fixed chunk output: %v", err)
		}
		fmt.Printf("\nFixed-chunk analysis saved to: %s\n", *fixedOutput)

		// Summary JSON of metrics for both strategies
		type StrategySummary struct {
			Name            string  `json:"name"`
			DedupRatioBytes float64 `json:"dedup_ratio_bytes"`
			UniqueBytes     uint64  `json:"unique_bytes"`
			TotalBytes      uint64  `json:"total_bytes"`
			RedundantBytes  uint64  `json:"redundant_bytes"`
			UniqueChunks    int     `json:"unique_chunks"`
			TotalChunks     int     `json:"total_chunks"`
		}
		comparison := struct {
			Strategies []StrategySummary `json:"strategies"`
		}{
			Strategies: []StrategySummary{
				{
					Name:            "nydus",
					DedupRatioBytes: metrics.DedupRatioBytes,
					UniqueBytes:     metrics.UniqueBytes,
					TotalBytes:      metrics.TotalBytes,
					RedundantBytes:  metrics.RedundantBytes,
					UniqueChunks:    metrics.UniqueChunks,
					TotalChunks:     metrics.TotalChunks,
				},
				{
					Name:            "fixed_512k",
					DedupRatioBytes: fixedMetrics.DedupRatioBytes,
					UniqueBytes:     fixedMetrics.UniqueBytes,
					TotalBytes:      fixedMetrics.TotalBytes,
					RedundantBytes:  fixedMetrics.RedundantBytes,
					UniqueChunks:    fixedMetrics.UniqueChunks,
					TotalChunks:     fixedMetrics.TotalChunks,
				},
			},
		}

		compJSON, err := json.MarshalIndent(comparison, "", "  ")
		if err != nil {
			log.Fatalf("marshal comparison: %v", err)
		}
		if err := os.WriteFile(*comparisonOutput, compJSON, 0o644); err != nil {
			log.Fatalf("write comparison: %v", err)
		}
		fmt.Printf("Chunking comparison results saved to: %s\n", *comparisonOutput)

		fmt.Printf("\nComparison:\n")
		fmt.Printf("Nydus: dedup %.1f%%, %d unique chunks, %d MB unique\n", metrics.DedupRatioBytes*100, metrics.UniqueChunks, metrics.UniqueBytes/1024/1024)
		fmt.Printf("Fixed 512K: dedup %.1f%%, %d unique chunks, %d MB unique\n", fixedMetrics.DedupRatioBytes*100, fixedMetrics.UniqueChunks, fixedMetrics.UniqueBytes/1024/1024)
	}
}
