package main

// Downloads the bootstrap metadata file of each image in a registry.
// This assumes that each image got converted via nydusify & uploaded to the registry beforehand.
// This bootstrap file will be used for getting the list of chunks of the images, used for offline analysis e.g. overlap
// =>  https://github.com/dragonflyoss/nydus/blob/master/docs/nydus-design.md

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

// Nydus images contain one layer that is required to be downloaded before the container can even start
// This bootstrap layer contains the table of content, files/folder and where they are located each, to show
// the container file system after startup
const bootstrapAnnotation = "containerd.io/snapshot/nydus-bootstrap"

// Prevent during unusually big responses to be read => Limit the max bytes we expect for manifest and bootstrap data
const (
	manifestMaxBytes  = 4 << 20   // 4 MiB
	maxBootstrapBytes = 256 << 20 // 256 MiB
)

// The JSON structure returned by a container registry when you ask for an image manifest
// => https://specs.opencontainers.org/image-spec/manifest/
type ociManifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	MediaType     string     `json:"mediaType"`
	Layers        []ociLayer `json:"layers"`
}

// A single layer inside an OCI manifest.
type ociLayer struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

// Parses an image reference url into (registry, repo, tag)
// Example: "localhost:5000/myimage:nydus"
// => registry="localhost:5000", repo="myimage", tag="nydus"
func parseImageRef(ref string) (registry, repo, tag string, err error) {
	// Locate the tag separator
	// The last : must come after the last '/', otherwise its probably a port not the tag...
	lastColon := strings.LastIndex(ref, ":")
	lastSlash := strings.LastIndex(ref, "/")

	// There is no colon or there is a slash after the last colon
	if lastColon < 0 || lastColon < lastSlash {
		return "", "", "", fmt.Errorf("error for image ref %q - missing tag", ref)
	}

	// Cut everything after the last colon => Get the image tag
	tag = ref[lastColon+1:]
	if tag == "" {
		return "", "", "", fmt.Errorf("error for image ref %q - empty tag", ref)
	}

	withoutTag := ref[:lastColon]

	// Split "registry/repo" on the first slash
	firstSlash := strings.Index(withoutTag, "/")
	if firstSlash < 0 {
		return "", "", "", fmt.Errorf("error for image ref %q - missing '/' between registry and repo", ref)
	}

	registry = withoutTag[:firstSlash]
	repo = withoutTag[firstSlash+1:]
	if registry == "" || repo == "" {
		return "", "", "", fmt.Errorf("error for image ref %q - registry or repo is empty", ref)
	}

	// Check if the registry oart starts with a colon => no server addr in that case
	// E.g. :8080/myimage:nydus
	if strings.HasPrefix(registry, ":") {
		return "", "", "", fmt.Errorf("error for image ref %q - registry %q starts with ':'", ref, registry)
	}
	return registry, repo, tag, nil
}

// Helper that returns two registry addresses, since we want to store for each image the nydus and normal image version
//   - nydusRef: address of the Nydus-converted image  (e.g. "localhost:5000/nginx:nydus")
//   - ociRef:   address of the plain OCI image         (e.g. "localhost:5000/nginx:latest")
//
// The input can be either
//
//  1. Just the image name, no registry or tag:
//     arg = "nginx"
//     - nydusRef = "<registry>/nginx:<nydusTag>", e.g. "localhost:5000/nginx:nydus"
//     - ociRef   = "<registry>/nginx:<ociTag>", e.g. "localhost:5000/nginx:latest"
//     The registry and tags come from the "--registry", "--nydus-tag", or "--oci-tag" flags.
//
//  2. Already contains a registry and tag:
//     arg = "localhost:5000/nginx:nydus"
//     - nydusRef = nothing to do here
//     - ociRef   = same registry + repo, but tag swapped to <ociTag>, e.g. "localhost:5000/nginx:latest"
func buildRefs(arg, registry, ociTag, nydusTag string) (nydusRef, ociRef string, err error) {
	if arg == "" {
		return "", "", fmt.Errorf("error: image arg is empty")
	}
	if strings.HasPrefix(arg, "/") {
		return "", "", fmt.Errorf("error at image arg %q: '/' is not allowed at start of --arg", arg)
	}

	// --arg contains registry and tag => keep as return value for nydusRef & swap tag for ociRef
	if strings.Contains(arg, "/") {
		reg, repo, _, perr := parseImageRef(arg)
		if perr != nil {
			return "", "", fmt.Errorf("error while parsing full ref %q: %w", arg, perr)
		}
		nydusRef = arg
		ociRef = fmt.Sprintf("%s/%s:%s", reg, repo, ociTag)
		return nydusRef, ociRef, nil
	}

	// arg is a bare name => add the default registry and append each tag.
	nydusRef = fmt.Sprintf("%s/%s:%s", registry, arg, nydusTag)
	ociRef = fmt.Sprintf("%s/%s:%s", registry, arg, ociTag)
	return nydusRef, ociRef, nil
}

// Converts the layer list from an OCI manifest into our own format for layer sizes and comparison later on
// => the manifests reported size is the compressed on-disk size
// The uncompressed size is not available here, so both size and compressedSize are set to the same value for now
func manifestToLayerInfos(manifest ociManifest, image, source string) []analysis.LayerInfo {
	layerData := make([]analysis.LayerInfo, 0, len(manifest.Layers))
	for _, currLayer := range manifest.Layers {
		size := uint64(currLayer.Size)
		layerData = append(layerData, analysis.LayerInfo{
			Digest:         currLayer.Digest,
			Size:           size,
			CompressedSize: size,
			Image:          image,
			Source:         source,
		})
	}
	return layerData
}

// Scans all layers in a Nydus manifest and returns the one with the bootstrap annotation set to true
func findBootstrapLayer(manifest ociManifest) (digest string, size int64, err error) {
	var matches []ociLayer
	for _, currLayer := range manifest.Layers {
		if currLayer.Annotations[bootstrapAnnotation] == "true" {
			matches = append(matches, currLayer)
		}
	}

	switch len(matches) {
	// There has to be exactly one bootstrap layer => zero means somethings wrong with the current image
	case 0:
		return "", 0, fmt.Errorf("no nydus-bootstrap layer found in manifest")
	// Bootstrap layer found
	case 1:
		return matches[0].Digest, matches[0].Size, nil
	// Same as zero => somethings wrong with the image
	default:
		return "", 0, fmt.Errorf("no nydus-bootstrap layer found in manifest")
	}
}

// Unpack the Nydus bootstrap file (image.boot) from of a bootstrap blob
func extractBootstrapFromTarGz(reader io.Reader, outPath string) error {
	// Decompress gzip
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("open gzip reader: %w", err)
	}
	defer gzipReader.Close()

	// iterate tar entries to find the bootstrap file
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()

		// Couldn't find the file before end of archive
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("error reading tar: %w", err)
		}

		// Found the bootstrap file => extract it to the given output path
		if header.Name == "image/image.boot" {
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return fmt.Errorf("error creating output dir: %w", err)
			}

			outputFile, err := os.Create(outPath)
			if err != nil {
				return fmt.Errorf("error creating output file: %w", err)
			}

			bytesWritten, err := io.Copy(outputFile, io.LimitReader(tarReader, maxBootstrapBytes))
			if err != nil {
				outputFile.Close()
				return fmt.Errorf("error while writing bootstrap file: %w", err)
			}

			// Bootstrap file is unexpectedly big
			if bytesWritten == maxBootstrapBytes {
				outputFile.Close()
				return fmt.Errorf("bootstrap entry exceeds cap of %d bytes", maxBootstrapBytes)
			}
			return outputFile.Close()
		}
	}
	return fmt.Errorf("image/image.boot not found in tar archive")
}

// Returns a directory name based on the image ref like "localhost:5000/nginx:nydus" => "localhost_5000_nginx_nydus".
func sanitizeRefForFilesystem(ref string) string {
	replacedSlash := strings.ReplaceAll(ref, "/", "_")
	return strings.ReplaceAll(replacedSlash, ":", "_")
}

// Returns the protocol for a registry request: "localhost" and "127.0.0.1" => "http",
// else => "https" (except insecure is specified)
func scheme(registry string, insecure bool) string {
	if insecure {
		return "http"
	}

	host := registry
	// Remove port
	if colonPosition := strings.LastIndex(registry, ":"); colonPosition > 0 {
		host = registry[:colonPosition]
	}

	if host == "localhost" || host == "127.0.0.1" {
		return "http"
	}
	return "https"
}

// Returns the OCI image manifest for a given repo+tag from the registry
func fetchManifest(ctx context.Context, client *http.Client, baseURL, repo, ref string) (ociManifest, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ociManifest{}, fmt.Errorf("error while build request for manifest: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")

	resp, err := client.Do(req)
	if err != nil {
		return ociManifest{}, fmt.Errorf("Error while fetching manifest %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ociManifest{}, fmt.Errorf("Error fetching manifest %s: HTTP %d: %s", url, resp.StatusCode, body)
	}

	var manifestData ociManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, manifestMaxBytes)).Decode(&manifestData); err != nil {
		return ociManifest{}, fmt.Errorf("Error decoding manifest JSON: %w", err)
	}
	return manifestData, nil
}

// Downloads a single registry blob by hash and extracts the Nydus bootstrap file out of it
func fetchBlob(ctx context.Context, client *http.Client, baseURL, repo, digest, outPath string) error {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", baseURL, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("error while building blob request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error for GET blob %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("error for GET blob %s: HTTP %d: %s", url, resp.StatusCode, body)
	}

	return extractBootstrapFromTarGz(resp.Body, outPath)
}

// Pulls the Nydus image metadata for a given image reference and extracts the chunks
// Returns the chunks and the OCI manifest
func pullAndExtract(ctx context.Context, client *http.Client, ref, workDir string, insecure bool) (*nydus.ImageChunks, ociManifest, error) {
	registry, repo, tag, err := parseImageRef(ref)
	if err != nil {
		return nil, ociManifest{}, err
	}

	baseUrl := fmt.Sprintf("%s://%s", scheme(registry, insecure), registry)

	// Get the list of layers
	manifest, err := fetchManifest(ctx, client, baseUrl, repo, tag)
	if err != nil {
		return nil, ociManifest{}, fmt.Errorf("error fetching manifest for %s: %w", ref, err)
	}

	// Find the bootstrap layer hash from the manifest
	bootstrapLayerHash, _, err := findBootstrapLayer(manifest)
	if err != nil {
		return nil, ociManifest{}, fmt.Errorf("error locating bootstrap layer for %s: %w", ref, err)
	}

	// Download & unpack the bootstrap
	imageFolderName := sanitizeRefForFilesystem(ref)
	bootPath := filepath.Join(workDir, imageFolderName, "image.boot")

	if err := fetchBlob(ctx, client, baseUrl, repo, bootstrapLayerHash, bootPath); err != nil {
		return nil, ociManifest{}, fmt.Errorf("error fetching bootstrap blob for %s: %w", ref, err)
	}

	// Parse the unpacked bootstrap file into a list of chunks
	imageChunks, err := nydus.ExtractChunks(ctx, bootPath, ref)
	if err != nil {
		return nil, ociManifest{}, fmt.Errorf("error extracting chunks for %s: %w", ref, err)
	}
	return imageChunks, manifest, nil
}

// Returns the file path of an access log (which chunks the image used while it ran) for a given image
func accessedLogPath(dir, imageID string) string {
	return filepath.Join(dir, "accessed-"+imageID+".txt")
}

// Reads the access log for a specific image and returns the chunk hashes that the app read until now
func loadAccessedHashes(dir, imageID string) (map[string]struct{}, error) {
	path := accessedLogPath(dir, imageID)
	logFile, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error opening access-log %s: %w", path, err)
	}
	defer logFile.Close()

	set := make(map[string]struct{})
	scanner := bufio.NewScanner(logFile)

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
		return nil, fmt.Errorf("error reading access-log %s: %w", path, err)
	}
	return set, nil
}

// Compares the chunks that were actually read (see loadAccessedHashes) with the complete chunk list of an image
// Returns how many accessed chunks are actually from this image and how many are not noted in the chunk list
// => This is a helper function to check for out-of sync issues with either the accessed log or the chunk list
func applyAccessedChunks(allImageChunks *nydus.ImageChunks, accessed map[string]struct{}) (matched, unmatched int) {
	allImageChunks.AccessedChunks = make([]nydus.ChunkInfo, 0, len(accessed))
	seen := make(map[string]struct{}, len(accessed))

	// Iterate all chunks in the catalog => if the hash is in the accessed set and we haven't already added it, add it to the lookup
	for _, currChunk := range allImageChunks.Chunks {
		// Skip this chunk if not in the access log
		if _, want := accessed[currChunk.Hash]; !want {
			continue
		}

		// Skip this chunk if we've already added a chunk with the same hash
		if _, dup := seen[currChunk.Hash]; dup {
			continue
		}

		// This chunk is in the access log and we haven't added it yet => add it to lookup and mark it as seen
		seen[currChunk.Hash] = struct{}{}
		allImageChunks.AccessedChunks = append(allImageChunks.AccessedChunks, currChunk)
	}

	// Contains how many unique hashes in the log were found in the catalog
	matched = len(seen)
	// How many log entries had no matching chunk => This indicates issues outside of code
	unmatched = len(accessed) - matched

	return matched, unmatched
}

func layersOutputPath(chunksOutput string) string {
	pathWithoutFileExt := strings.TrimSuffix(chunksOutput, ".json")
	return pathWithoutFileExt + "-layers.json"
}

// Structure of the layer-size file that will be used to compare storage sizes between nydus and oci images
type layersDoc struct {
	OCILayers   []analysis.ImageLayers `json:"oci_layers"`
	NydusLayers []analysis.ImageLayers `json:"nydus_layers"`
}

func main() {
	// input param flags
	// First set default values before parsing specific input params
	outputPath := flag.String("output", "analysis/results/chunks.json", "Output JSON file")
	workDir := flag.String("workdir", "/tmp/chunk-inspector", "Directory for extracted bootstraps")
	timeout := flag.Duration("timeout", 30*time.Second, "Per-image timeout")
	insecure := flag.Bool("insecure", false, "Use HTTP instead of HTTPS for registry")
	extractLayers := flag.Bool("extract-layers", false, "Additionally write a <output>-layers.json with OCI and Nydus manifest layer baselines")
	accessedLogDir := flag.String("accessed-log-dir", "", "Directory containing accessed-<imageId>.txt logs. If set, AccessedChunks is populated from each log (file must exist for every image)")
	registry := flag.String("registry", "localhost:5000", "Default registry host for image names")
	ociTag := flag.String("oci-tag", "latest", "Tag used to resolve the OCI version of an image")
	nydusTag := flag.String("nydus-tag", "nydus", "Tag used to resolve the Nydus version of an image")
	flag.Parse()

	// Check for the required image input list param
	args := flag.Args()
	if len(args) == 0 {
		log.Fatal("Usage: chunk-inspector [flags] <image> [<image> ...]")
	}

	// Create the directory where unpacked bootstrap files are saved to
	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		log.Fatalf("error while creating workdir %s: %v", *workDir, err)
	}

	// HTTP client for the registry requests
	client := &http.Client{Timeout: *timeout}
	var results []nydus.ImageChunks
	var ociLayers, nydusLayers []analysis.ImageLayers

	// Pull & extract each image from the input image list
	for _, arg := range args {
		nydusRef, ociRef, err := buildRefs(arg, *registry, *ociTag, *nydusTag)
		if err != nil {
			log.Fatalf("error resolving refs for %q: %v", arg, err)
		}

		// Each image gets its own context so a slow registry can't block others
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		allImageChunks, nydusManifest, err := pullAndExtract(ctx, client, nydusRef, *workDir, *insecure)
		if err != nil {
			cancel()
			log.Fatalf("error inspecting %s: %v", nydusRef, err)
		}

		// Replace the image value (full adress with registry, e.g. localhost:5000/fn-python-hello:nydus)
		// to the name from the input image list (fn-python-hello)
		allImageChunks.Image = arg
		for currChunk := range allImageChunks.Chunks {
			allImageChunks.Chunks[currChunk].Image = arg
		}
		unique := allImageChunks.UniqueChunks()
		fmt.Printf("Inspecting %s… %d chunks, %d unique, %d bytes\n",
			nydusRef, len(allImageChunks.Chunks), len(unique), allImageChunks.TotalBytes)

		// Get the list of chunks that the app actually was reading at startup
		if *accessedLogDir != "" {
			accessed, loaderr := loadAccessedHashes(*accessedLogDir, arg)
			if loaderr != nil {
				cancel()
				log.Fatalf("error loading accessed log for %s: %v", arg, loaderr)
			}

			matched, unmatched := applyAccessedChunks(allImageChunks, accessed)
			fmt.Printf("accessed: %d/%d unique chunks matched (%d log entries with no matching chunk)\n", matched, len(unique), unmatched)
		}

		results = append(results, *allImageChunks)

		// Collect layer-size for both image formats
		if *extractLayers {
			// Nydus layers come from the manifest we already fetched above
			nydusLayers = append(nydusLayers, analysis.ImageLayers{
				Image:  arg,
				Source: "nydus",
				Layers: manifestToLayerInfos(nydusManifest, arg, "nydus"),
			})

			// Until now we only fetched the nydus image versions, but we need the plain oci versions
			// as well for comparison
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

	// Create the output directory
	outDir := filepath.Dir(*outputPath)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatalf("error while creating output dir %s: %v", outDir, err)
	}

	// Convert chunk list to json
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		log.Fatalf("error converting results: %v", err)
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
