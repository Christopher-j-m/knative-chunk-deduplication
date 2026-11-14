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

	"nydus-cluster-cache/pkg/nydus"
	"nydus-cluster-cache/pkg/overlap"
)

// Contains the list of image with their respective roles (overlap analysis only or knative-workload for later experiments)
// and coverage scenarios (same runtime burst, cold cluster, workload-burst, and more)
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

// Used for comparing deduplication for OCI layers and nydus blobs
type BlobOverlapOutput struct {
	OCIMetrics       overlap.LayerOverlapMetrics    `json:"oci_metrics"`
	NydusMetrics     overlap.LayerOverlapMetrics    `json:"nydus_metrics"`
	OCIPairwise      []overlap.PairwiseLayerOverlap `json:"oci_pairwise,omitempty"`
	NydusPairwise    []overlap.PairwiseLayerOverlap `json:"nydus_pairwise,omitempty"`
	ChunkMinusBlobMB int64                          `json:"chunk_minus_blob_mb"` // How much add. space nydus content-defined chunking can save
}

// Metrics across all the images, such as per image-pair overlap, chunk frequencies,...
type AnalysisOutput struct {
	Corpus      overlap.CorpusMetrics      `json:"corpus_metrics"`
	Pairwise    []overlap.PairwiseOverlap  `json:"pairwise_overlap"`
	Scenarios   []overlap.CoverageScenario `json:"coverage_scenarios"`
	Frequencies []overlap.ChunkFrequency   `json:"chunk_frequencies"`
	BlobOverlap *BlobOverlapOutput         `json:"blob_overlap,omitempty"`
}

func main() {
	// Set the default input params
	input := flag.String("input", "analysis/results/chunks.json", "Input chunks JSON")
	corpusFile := flag.String("corpus", "analysis/corpus.json", "Corpus config with scenarios")
	output := flag.String("output", "analysis/results/overlap.json", "Output analysis JSON")
	filterRole := flag.String("filter-role", "", "Filter to images with this role (e.g. knative-workload)")
	useAccessed := flag.Bool("accessed-only", false, "Use accessed chunks only (during startup)")
	blobOverlap := flag.Bool("blob-overlap", false, "Compute OCI layer and Nydus blob overlap")
	compareFixed := flag.Bool("compare-fixed", false, "Run 512 KiB fixed chunking comparison")
	exportDir := flag.String("export-dir", "", "Directory with exported filesystems (required with --compare-fixed)")
	fixedOutput := flag.String("fixed-output", "", "Output JSON for fixed-chunk analysis (required with --compare-fixed)")
	comparisonOutput := flag.String("comparison-output", "", "Output JSON for Nydus vs fixed summary (required with --compare-fixed)")
	flag.Parse() // Parse user input params

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

	// Filter image list to ones matching the specified role
	if *filterRole != "" {
		corpusData, err := os.ReadFile(*corpusFile)
		if err != nil {
			log.Fatalf("error reading corpus config: %v", err)
		}

		var config CorpusConfig
		if err := json.Unmarshal(corpusData, &config); err != nil {
			log.Fatalf("error parsing corpus config: %v", err)
		}

		// Get the images whose role is the one we're filtering for
		imageNamesWithRole := make(map[string]struct{})
		for _, currImage := range config.Images {
			if currImage.Role == *filterRole {
				imageNamesWithRole[currImage.ID] = struct{}{}
			}
		}

		// Get the chunks of the images that we previously filtered by a specific role
		var filtered []nydus.ImageChunks
		for _, currImageChunks := range corpus {
			if _, ok := imageNamesWithRole[currImageChunks.Image]; ok {
				filtered = append(filtered, currImageChunks)
			}
		}
		fmt.Printf("Filtered to %d images with the specific role %s\n", len(filtered), *filterRole)
		corpus = filtered
	}

	// Choose between all chunks or only those accessed during startup, depending on the input param
	chunkSelector := overlap.AllChunks
	if *useAccessed {
		if err := overlap.ValidateAccessData(corpus); err != nil {
			log.Fatalf("--accessed-only requires startup trace data: %v", err)
		}
		chunkSelector = overlap.AccessedOnly
	}

	fmt.Printf("Analyzing %d images...\n", len(corpus))

	metrics := overlap.ComputeCorpusMetrics(corpus, chunkSelector)
	fmt.Printf("\nCorpus Metrics:\n")
	fmt.Printf("Total chunks: %d\n", metrics.TotalChunks)
	fmt.Printf("Unique chunks: %d\n", metrics.UniqueChunks)
	fmt.Printf("Dedup ratio (chunks): %.1f%%\n", metrics.DedupRatioChunks*100)
	fmt.Printf("Dedup ratio (bytes): %.1f%%\n", metrics.DedupRatioBytes*100)
	fmt.Printf("Total bytes: %d MB\n", metrics.TotalBytes/1024/1024)
	fmt.Printf("Unique bytes: %d MB\n", metrics.UniqueBytes/1024/1024)
	fmt.Printf("Redundant: %d MB\n", metrics.RedundantBytes/1024/1024)

	// Sort so that the pairs with the potentially most overlap appear at top of the list
	pairwise := overlap.ComputePairwiseOverlap(corpus, chunkSelector)
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
		fmt.Printf("%s - %s: %d chunks, %d MB (%.0f%%/%.0f%% by bytes)\n", p.ImageA, p.ImageB, p.SharedChunks, p.SharedBytes/1024/1024, p.OverlapByBytesA*100, p.OverlapByBytesB*100)
	}

	// Run the coverage scenarios
	var scenarios []overlap.CoverageScenario
	corpusData, err := os.ReadFile(*corpusFile)
	if err == nil {
		var config CorpusConfig

		if err := json.Unmarshal(corpusData, &config); err == nil && len(config.CoverageScenarios) > 0 {
			var inputs []overlap.CoverageOrder

			for _, currScenario := range config.CoverageScenarios {

				// Skip scenarios that don't match the active role filter, or skip scoped scenarios when no filter is set
				if *filterRole != "" {
					if currScenario.Scope == nil || *currScenario.Scope != *filterRole {
						continue
					}
				} else {
					if currScenario.Scope != nil {
						continue
					}
				}

				order := currScenario.Order
				if order == nil {

					// Use a seed for easier debugging...
					seed := int64(0)
					for _, currScenarioName := range currScenario.Name {
						seed = seed*31 + int64(currScenarioName)
					}

					rng := rand.New(rand.NewSource(seed))
					order = make([]string, len(corpus))
					for i, imageChunks := range corpus {
						order[i] = imageChunks.Image
					}

					rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
				}
				inputs = append(inputs, overlap.CoverageOrder{Name: currScenario.Name, Order: order})
			}

			// Run the curr scenario by simulating the cluster cache filling up with the given workload order
			var scenarioErr error
			scenarios, scenarioErr = overlap.ComputeCoverageCurveMultiOrder(corpus, inputs, chunkSelector)
			if scenarioErr != nil {
				log.Fatalf("error occured while simulating the scenario: %v", scenarioErr)
			}

			// Only print how much could've been served by the cache instead of registry at the last line
			// => last line = state after every image arrived
			fmt.Printf("\nCoverage:\n")
			for _, currScenario := range scenarios {
				if len(currScenario.Curve) > 0 {
					last := currScenario.Curve[len(currScenario.Curve)-1]
					fmt.Printf("%-20s: %.1f%% (chunks), %.1f%% (bytes)\n", currScenario.Name, last.PotentialHitRateChunks*100, last.PotentialHitRateBytes*100)
				}
			}
		}
	}

	// Count how many images reference each chunk
	frequencies := overlap.ComputeChunkFrequencies(corpus, chunkSelector)
	fmt.Printf("\nChunk frequency distribution:\n")
	refCounts := make(map[int]int)
	for _, currFrequency := range frequencies {
		refCounts[currFrequency.RefCount]++
	}

	for imageCount := len(corpus); imageCount >= 1; imageCount-- {
		if chunkCount, ok := refCounts[imageCount]; ok {
			fmt.Printf("Referenced by %d images: %d chunks\n", imageCount, chunkCount)
		}
	}

	// Compare Nydus chunk-level deduplication against same layers
	// => Layers need to be exact same to be considered duplicates, chunks are more granular
	var blobOutput *BlobOverlapOutput
	if *blobOverlap {
		// Load the layer list for each image from file (contains both nydus & oci format)
		layerPath := strings.TrimSuffix(*input, ".json") + "-layers.json"
		layerData, err := os.ReadFile(layerPath)
		if err != nil {
			log.Fatalf("error reading layer data: %v", err)
		}

		var layers struct {
			OCI   []overlap.ImageLayers `json:"oci_layers"`
			Nydus []overlap.ImageLayers `json:"nydus_layers"`
		}

		if err := json.Unmarshal(layerData, &layers); err != nil {
			log.Fatalf("error parsing layer data: %v", err)
		}

		ociMetrics := overlap.ComputeLayerOverlapMetrics(layers.OCI)
		nydusMetrics := overlap.ComputeLayerOverlapMetrics(layers.Nydus)
		ociPairwise := overlap.ComputePairwiseLayerOverlap(layers.OCI)
		nydusPairwise := overlap.ComputePairwiseLayerOverlap(layers.Nydus)

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

	// Skip writing the results when we're in comparison mode
	if !*compareFixed {
		out, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			log.Fatalf("failed to marshal output: %v", err)
		}

		if err := os.WriteFile(*output, out, 0o644); err != nil {
			log.Fatalf("error during writing output: %v", err)
		}

		fmt.Printf("\nAnalysis saved to %s\n", *output)
	}

	// Re-chunk every image with a fixed 512 KiB boundary by joining all image files into one byte stream (order by name) and
	// then chunk at 512 KiB boundaries
	if *compareFixed {

		fmt.Printf("\nFixed 512 KiB Chunking Comparison...\n")

		var fixedCorpus []nydus.ImageChunks
		for _, allImageChunks := range corpus {
			imgDir := filepath.Join(*exportDir, allImageChunks.Image)
			chunks, err := nydus.FlattenImage(imgDir, 512*1024, allImageChunks.Image)
			if err != nil {
				log.Fatalf("error during flattening %s: %v", allImageChunks.Image, err)
			}

			var totalBytes uint64
			for _, currChunk := range chunks {
				totalBytes += currChunk.Size
			}

			fixedCorpus = append(fixedCorpus, nydus.ImageChunks{
				Image:      allImageChunks.Image,
				TotalBytes: totalBytes,
				Chunks:     chunks,
			})
			fmt.Printf("%s: %d fixed chunks\n", allImageChunks.Image, len(chunks))
		}

		// Run overlap analysis with the new chunking
		fixedMetrics := overlap.ComputeCorpusMetrics(fixedCorpus, chunkSelector)
		fixedPairwise := overlap.ComputePairwiseOverlap(fixedCorpus, chunkSelector)

		// Sort pairs by most bytes first for easier debugging
		sort.Slice(fixedPairwise, func(i, j int) bool {
			if fixedPairwise[i].SharedBytes != fixedPairwise[j].SharedBytes {
				return fixedPairwise[i].SharedBytes > fixedPairwise[j].SharedBytes
			}
			if fixedPairwise[i].ImageA != fixedPairwise[j].ImageA {
				return fixedPairwise[i].ImageA < fixedPairwise[j].ImageA
			}
			return fixedPairwise[i].ImageB < fixedPairwise[j].ImageB
		})
		fixedFrequencies := overlap.ComputeChunkFrequencies(fixedCorpus, chunkSelector)

		fixedResult := AnalysisOutput{
			Corpus:      fixedMetrics,
			Pairwise:    fixedPairwise,
			Scenarios:   nil, // We dont have access logs for which chunks are accessed during startup for the new 512 KiB chunking
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

		// Metrics for both strategies
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
