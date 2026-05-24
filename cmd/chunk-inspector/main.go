package main

// chunk-inspector is a CLI tool that connects to a container registry,
// downloads the Nydus metadata file ("bootstrap") for each image, and
// extracts the list of content-addressed data chunks that make up that image.
// The resulting chunk catalog is written as JSON for offline analysis
// (e.g. measuring how many chunks are shared across images in a cluster).
//
// High-level flow per image:
//   1. Fetch the OCI manifest → find the bootstrap layer digest.
//   2. Download the bootstrap blob (a .tar.gz) and unpack image.boot.
//   3. Run nydus.ExtractChunks on image.boot → get a list of ChunkInfo records.
//   4. Optionally: cross-reference with a runtime access log to mark which
//      chunks were actually read during a workload trace.
//   5. Append result to the output JSON file.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nydus-cluster-cache/pkg/analysis"
	"nydus-cluster-cache/pkg/nydus"
)

// The OCI layer annotation key that Nydus uses to mark
// which layer is the bootstrap, where all chunks are described.
// Key is set to true for the bootstrap layer
const bootstrapAnnotation = "containerd.io/snapshot/nydus-bootstrap"

const (
	// Byte caps to prevent accidentally (malformed registry or bug in here) reading a gigantic response body into memory
	manifestMaxBytes  = 4 << 20   // 4 MiB
	maxBootstrapBytes = 256 << 20 // 256 MiB
)

// The JSON structure returned by a container registry when you
// ask for an image manifest. It lists all the layers (blobs) that make up the image.
// => https://specs.opencontainers.org/image-spec/manifest/
type ociManifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	MediaType     string     `json:"mediaType"`
	Layers        []ociLayer `json:"layers"`
}

// Describes a single layer inside an OCI manifest.
type ociLayer struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

// Parses an image reference of the form
// "host[:port]/repo/path:tag" into its three components.
// Example: "localhost:5000/myapp:nydus"
// => registry="localhost:5000", repo="myapp", tag="nydus"
func parseImageRef(ref string) (registry, repo, tag string, err error) {
	// Locate the tag separator: the last ":" must come after the last '/', otherwise the colon is part of a port number, not a tag separator
	lastColon := strings.LastIndex(ref, ":")
	lastSlash := strings.LastIndex(ref, "/")

	if lastColon < 0 || lastColon < lastSlash {
		return "", "", "", fmt.Errorf("image ref %q: missing tag", ref)
	}

	tag = ref[lastColon+1:]
	if tag == "" {
		return "", "", "", fmt.Errorf("image ref %q: empty tag", ref)
	}

	withoutTag := ref[:lastColon]

	// Split "registry/repo" on the first slash
	firstSlash := strings.Index(withoutTag, "/")
	if firstSlash < 0 {
		return "", "", "", fmt.Errorf("image ref %q: missing '/' between registry and repo", ref)
	}
	registry = withoutTag[:firstSlash]
	repo = withoutTag[firstSlash+1:]
	if registry == "" || repo == "" {
		return "", "", "", fmt.Errorf("image ref %q: registry or repo is empty", ref)
	}
	if strings.HasPrefix(registry, ":") {
		return "", "", "", fmt.Errorf("image ref %q: registry %q starts with ':'", ref, registry)
	}
	return registry, repo, tag, nil
}

// Helper that produces two registry addresses from a single CLI argument:
//   - nydusRef: address of the Nydus-converted image  (e.g. "localhost:5000/nginx:nydus")
//   - ociRef:   address of the plain OCI image         (e.g. "localhost:5000/nginx:latest")
//
// The input can be either:
//
//  1. Bare name — just the image name, no registry or tag:
//     arg = "nginx"
//     - nydusRef = "<registry>/nginx:<nydusTag>", e.g. "localhost:5000/nginx:nydus"
//     - ociRef   = "<registry>/nginx:<ociTag>", e.g. "localhost:5000/nginx:latest"
//     The registry and tags come from the "--registry", "--nydus-tag", or "--oci-tag" flags.
//
//  2. Full ref — already contains a registry and tag:
//     arg = "localhost:5000/nginx:nydus"
//     - nydusRef = nothing to do here
//     - ociRef   = same registry + repo, but tag swapped to <ociTag>, e.g. "localhost:5000/nginx:latest"
func buildRefs(arg, registry, ociTag, nydusTag string) (nydusRef, ociRef string, err error) {
	if arg == "" {
		return "", "", fmt.Errorf("image arg is empty")
	}
	if strings.HasPrefix(arg, "/") {
		return "", "", fmt.Errorf("image arg %q: leading '/' is not allowed", arg)
	}
	if strings.Contains(arg, "/") {
		// arg is a full ref => keep it as nydusRef, derive ociRef by swapping the tag
		reg, repo, _, perr := parseImageRef(arg)
		if perr != nil {
			return "", "", fmt.Errorf("parse full ref %q: %w", arg, perr)
		}
		nydusRef = arg
		ociRef = fmt.Sprintf("%s/%s:%s", reg, repo, ociTag)
		return nydusRef, ociRef, nil
	}
	// arg is a bare name => prepend the default registry and append each tag.
	nydusRef = fmt.Sprintf("%s/%s:%s", registry, arg, nydusTag)
	ociRef = fmt.Sprintf("%s/%s:%s", registry, arg, ociTag)
	return nydusRef, ociRef, nil
}

// Converts the layer list from an OCI manifest into analysis.LayerInfo structs.
// These are used as a size baseline as we want to know the compressed size of each layer,
// so downstream analysis can compare Nydus vs OCI storage footprint.
//
// => the manifests reported size is the compressed (on-disk) size.
// The uncompressed size is not available here, so both size and compressedSize
// are set to the same value for now.
func manifestToLayerInfos(m ociManifest, image, source string) []analysis.LayerInfo {
	out := make([]analysis.LayerInfo, 0, len(m.Layers))
	for _, l := range m.Layers {
		size := uint64(l.Size)
		out = append(out, analysis.LayerInfo{
			Digest:         l.Digest,
			Size:           size,
			CompressedSize: size,
			Image:          image,
			Source:         source,
		})
	}
	return out
}

// Scans all layers in a Nydus manifest and returns the one annotated as the bootstrap (see at top the Nydus metadata index key).
func findBootstrapLayer(m ociManifest) (digest string, size int64, err error) {
	var matches []ociLayer
	for _, l := range m.Layers {
		if l.Annotations[bootstrapAnnotation] == "true" {
			matches = append(matches, l)
		}
	}

	switch len(matches) {
	// There has to be exactly one bootstrap layer => zero means somethings wrong with the current image
	case 0:
		return "", 0, fmt.Errorf("no nydus-bootstrap layer found in manifest (checked %d layers)", len(m.Layers))
	// Found it as expected => return the digest and size for downstream fetching
	case 1:
		return matches[0].Digest, matches[0].Size, nil
	// Same as zero => somethings wrong with the image
	default:
		return "", 0, fmt.Errorf("multiple nydus-bootstrap layers found (%d); expected exactly one", len(matches))
	}
}

// Unpacks the Nydus bootstrap file (image.boot) out of a downloaded bootstrap blob.
func extractBootstrapFromTarGz(r io.Reader, outPath string) error {
	// Decompress gzip
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open gzip reader: %w", err)
	}
	defer gz.Close()

	// iterate tar entries to find the bootstrap file
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()

		// Couldn't find the file before end of archive
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		// Found the bootstrap file => extract it to the output path
		if hdr.Name == "image/image.boot" {
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return fmt.Errorf("create output dir: %w", err)
			}

			f, err := os.Create(outPath)
			if err != nil {
				return fmt.Errorf("create output file: %w", err)
			}

			// Throw if the bootstrap file is unexpectedly large
			n, err := io.Copy(f, io.LimitReader(tr, maxBootstrapBytes))
			if err != nil {
				f.Close()
				return fmt.Errorf("write bootstrap file: %w", err)
			}
			if n == maxBootstrapBytes {
				f.Close()
				return fmt.Errorf("bootstrap entry exceeds cap of %d bytes", maxBootstrapBytes)
			}
			return f.Close()
		}
	}
	return fmt.Errorf("image/image.boot not found in tar archive")
}

// Helper to return a directory name based on the image ref like "localhost:5000/nginx:nydus" => "localhost_5000_nginx_nydus".
func sanitizeRefForFilesystem(ref string) string {
	r := strings.ReplaceAll(ref, "/", "_")
	return strings.ReplaceAll(r, ":", "_")
}

// Helper to return the network protocol for a registry request: "localhost" and "127.0.0.1" => "http",
// else => "https" (except --insecure is specified).
func scheme(registry string, insecure bool) string {
	if insecure {
		return "http"
	}

	host := registry
	// Strip port suffix, e.g. "myregistry.io:443" => "myregistry.io")
	if idx := strings.LastIndex(registry, ":"); idx > 0 {
		host = registry[:idx]
	}

	if host == "localhost" || host == "127.0.0.1" {
		return "http"
	}
	return "https"
}

// Retrieves the OCI image manifest for a given repo+tag from the registry
func fetchManifest(ctx context.Context, client *http.Client, baseURL, repo, ref string) (ociManifest, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ociManifest{}, fmt.Errorf("build manifest request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")

	resp, err := client.Do(req)
	if err != nil {
		return ociManifest{}, fmt.Errorf("Error fetching manifest %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ociManifest{}, fmt.Errorf("Error fetching manifest %s: HTTP %d: %s", url, resp.StatusCode, body)
	}

	var m ociManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, manifestMaxBytes)).Decode(&m); err != nil {
		return ociManifest{}, fmt.Errorf("Error decoding manifest JSON: %w", err)
	}
	return m, nil
}

// Downloads a single registry blob (identified by its digest) and extracts the Nydus bootstrap file out of it.
func fetchBlob(ctx context.Context, client *http.Client, baseURL, repo, digest, outPath string) error {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", baseURL, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build blob request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET blob %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET blob %s: HTTP %d: %s", url, resp.StatusCode, body)
	}

	return extractBootstrapFromTarGz(resp.Body, outPath)
}

// Pulls the Nydus image metadata for a given image reference and extracts the chunks.
// Returns the chunk catalog and the original OCI manifest.
func pullAndExtract(ctx context.Context, client *http.Client, ref, workDir string, insecure bool) (*nydus.ImageChunks, ociManifest, error) {
	registry, repo, tag, err := parseImageRef(ref)
	if err != nil {
		return nil, ociManifest{}, err
	}

	base := fmt.Sprintf("%s://%s", scheme(registry, insecure), registry)

	// Get the list of layers
	m, err := fetchManifest(ctx, client, base, repo, tag)
	if err != nil {
		return nil, ociManifest{}, fmt.Errorf("error fetching manifest for %s: %w", ref, err)
	}

	// Find the bootstrap layer digest from the manifest
	digest, _, err := findBootstrapLayer(m)
	if err != nil {
		return nil, ociManifest{}, fmt.Errorf("error locating bootstrap layer for %s: %w", ref, err)
	}

	// Download & unpack the bootstrap
	safe := sanitizeRefForFilesystem(ref)
	bootPath := filepath.Join(workDir, safe, "image.boot")

	if err := fetchBlob(ctx, client, base, repo, digest, bootPath); err != nil {
		return nil, ociManifest{}, fmt.Errorf("error fetching bootstrap blob for %s: %w", ref, err)
	}

	// Parse the unpacked bootstrap file into a list of chunks
	ic, err := nydus.ExtractChunks(ctx, bootPath, ref)
	if err != nil {
		return nil, ociManifest{}, fmt.Errorf("error extracting chunks for %s: %w", ref, err)
	}
	return ic, m, nil
}

// Helper that returns the expected path of the per-image access log.
// These logs are produced by a separate tracing script that records which
// chunk hashes were fetched from the cache during a (simulated) real workload run.
// Format: "<dir>/accessed-<imageID>.txt"
func accessedLogPath(dir, imageID string) string {
	return filepath.Join(dir, "accessed-"+imageID+".txt")
}

// Reads one of the per-image access logs and returns the
// set of chunk hashes that were accessed during the trace.
func loadAccessedHashes(dir, imageID string) (map[string]struct{}, error) {
	path := accessedLogPath(dir, imageID)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open accessed log %s: %w", path, err)
	}
	defer f.Close()

	set := make(map[string]struct{})
	scanner := bufio.NewScanner(f)

	// Scan each line, trim whitespace & ignore empty lines or comments (starting with #)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		set[line] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading accessed log %s: %w", path, err)
	}
	return set, nil
}

// Cross-references the chunk catalog for an image against the set of accessed chunk hashes from the log, and populates the AccessedChunks field in-place.
// Returns how many unique accessed hashes were matched in the catalog, and how many log entries had no matching chunk.
func applyAccessedChunks(ic *nydus.ImageChunks, accessed map[string]struct{}) (matched, unmatched int) {
	ic.AccessedChunks = make([]nydus.ChunkInfo, 0, len(accessed))
	seen := make(map[string]struct{}, len(accessed))

	// Iterate all chunks in the catalog => if the hash is in the accessed set and we haven't already added it, add it to AccessedChunks
	for _, c := range ic.Chunks {
		// Skip this chunk if its hash is not in the access log
		if _, want := accessed[c.Hash]; !want {
			continue
		}

		// Skip this chunk if we've already added a chunk with the same hash (dedup)
		if _, dup := seen[c.Hash]; dup {
			continue
		}

		// This chunk is in the access log and we haven't added it yet => add it to AccessedChunks and mark this hash as seen
		seen[c.Hash] = struct{}{}
		ic.AccessedChunks = append(ic.AccessedChunks, c)
	}

	// Contains how many unique hashes in the log were found in the catalog
	matched = len(seen)
	// How many log entries had no matching chunk
	unmatched = len(accessed) - matched

	return matched, unmatched
}

// Helper to build the companion layer-baseline file path from the
// primary chunks output path
// Example: "analysis/results/chunks.json" → "analysis/results/chunks-LAYERS.json"
func layersOutputPath(chunksOutput string) string {
	stem := strings.TrimSuffix(chunksOutput, ".json")
	return stem + "-layers.json"
}

// JSON structure written to the "-layers.json" file when the flag "--extract-layers" is passed.
// It records the layer manifests for both the plain OCI and the Nydus variants of each image, enabling downstream scripts
// to compare storage sizes between the two formats.
type layersDoc struct {
	OCILayers   []analysis.ImageLayers `json:"oci_layers"`
	NydusLayers []analysis.ImageLayers `json:"nydus_layers"`
}

func main() {
	// CLI flags
	outputPath := flag.String("output", "analysis/results/chunks.json", "Output JSON file")
	workDir := flag.String("workdir", "/tmp/chunk-inspector", "Scratch directory for extracted bootstraps")
	timeout := flag.Duration("timeout", 30*time.Second, "Per-image timeout")
	insecure := flag.Bool("insecure", false, "Use HTTP instead of HTTPS for registry (automatically enabled for localhost/127.0.0.1)")
	extractLayers := flag.Bool("extract-layers", false, "Additionally write a <output-stem>-layers.json with OCI and Nydus manifest layer baselines")
	accessedLogDir := flag.String("accessed-log-dir", "", "Directory containing accessed-<imageId>.txt logs. If set, AccessedChunks is populated from each log (file must exist for every image)")
	registry := flag.String("registry", "localhost:5000", "Default registry host for bare image names")
	ociTag := flag.String("oci-tag", "latest", "Tag used to resolve the OCI variant of an image")
	nydusTag := flag.String("nydus-tag", "nydus", "Tag used to resolve the Nydus variant of an image")
	flag.Parse()

	// Remaining positional args after flags are the image names/refs to inspect
	args := flag.Args()
	if len(args) == 0 {
		log.Fatal("Usage: chunk-inspector [flags] <image> [<image> ...]")
	}

	// Create the directory where unpacked bootstrap files are created
	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		log.Fatalf("error creating workdir %s: %v", *workDir, err)
	}

	// HTTP client for the registry requests
	client := &http.Client{Timeout: *timeout}
	var results []nydus.ImageChunks
	var ociLayers, nydusLayers []analysis.ImageLayers

	// Pull & extract each image
	for _, arg := range args {
		nydusRef, ociRef, err := buildRefs(arg, *registry, *ociTag, *nydusTag)
		if err != nil {
			log.Fatalf("error resolving refs for %q: %v", arg, err)
		}

		// Each image gets its own context so a slow registry can't block others.
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		ic, nydusManifest, err := pullAndExtract(ctx, client, nydusRef, *workDir, *insecure)
		if err != nil {
			cancel()
			log.Fatalf("error inspecting %s: %v", nydusRef, err)
		}

		// Override the image identifier with the bare arg (corpus ID) so
		// downstream analysis can join chunks.json against corpus.json by ID,
		// rather than by the full registry ref which could vary across environments
		ic.Image = arg
		for i := range ic.Chunks {
			ic.Chunks[i].Image = arg
		}
		unique := ic.UniqueChunks()
		fmt.Printf("Inspecting %s… %d chunks, %d unique, %d bytes\n",
			nydusRef, len(ic.Chunks), len(unique), ic.TotalBytes)

		// Annotate which chunks were actually accessed during a trace run
		if *accessedLogDir != "" {
			accessed, lerr := loadAccessedHashes(*accessedLogDir, arg)
			if lerr != nil {
				cancel()
				log.Fatalf("error loading accessed log for %s: %v", arg, lerr)
			}
			matched, unmatched := applyAccessedChunks(ic, accessed)
			fmt.Printf("accessed: %d/%d unique chunks matched (%d log entries with no matching chunk)\n",
				matched, len(unique), unmatched)
		}

		results = append(results, *ic)

		// Collect layer-size baselines for both image formats
		if *extractLayers {
			// Nydus layers come from the manifest we already fetched above
			nydusLayers = append(nydusLayers, analysis.ImageLayers{
				Image:  arg,
				Source: "nydus",
				Layers: manifestToLayerInfos(nydusManifest, arg, "nydus"),
			})

			// The Nydus manifest (fetched above as part of chunk extraction) only
			// describes the Nydus-format layers. The plain OCI image lives under a
			// different tag (e.g. ":latest" vs ":nydus") in the same registry+repo,
			// so its manifest must be fetched separately to get the OCI layer sizes
			ociRegistry, ociRepo, ociRefTag, perr := parseImageRef(ociRef)
			if perr != nil {
				cancel()
				log.Fatalf("error parsing oci ref %q: %v", ociRef, perr)
			}
			ociBase := fmt.Sprintf("%s://%s", scheme(ociRegistry, *insecure), ociRegistry)
			ociManifestData, ferr := fetchManifest(ctx, client, ociBase, ociRepo, ociRefTag)
			if ferr != nil {
				cancel()
				log.Fatalf("error fetching oci manifest for %s: %v", ociRef, ferr)
			}
			ociLayers = append(ociLayers, analysis.ImageLayers{
				Image:  arg,
				Source: "oci",
				Layers: manifestToLayerInfos(ociManifestData, arg, "oci"),
			})
		}

		cancel()
	}

	/* Write results to disk */

	// Create the output directory if necessary.
	outDir := filepath.Dir(*outputPath)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatalf("create output dir %s: %v", outDir, err)
	}

	// Serialize all collected chunk catalogs as a JSON array and write to disk
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		log.Fatalf("error marshaling results: %v", err)
	}
	if err := os.WriteFile(*outputPath, data, 0o644); err != nil {
		log.Fatalf("error writing %s: %v", *outputPath, err)
	}
	fmt.Printf("Wrote %s\n", *outputPath)

	// Write the layer-baseline JSON alongside the chunks file
	if *extractLayers {
		layersPath := layersOutputPath(*outputPath)
		doc := layersDoc{OCILayers: ociLayers, NydusLayers: nydusLayers}
		layersData, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			log.Fatalf("marshal layers: %v", err)
		}
		if err := os.WriteFile(layersPath, layersData, 0o644); err != nil {
			log.Fatalf("error writing %s: %v", layersPath, err)
		}
		fmt.Printf("Wrote %s\n", layersPath)
	}
}
