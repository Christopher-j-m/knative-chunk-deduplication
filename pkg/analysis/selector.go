package analysis

import (
	"fmt"
	"nydus-cluster-cache/pkg/nydus"
)

// Filters which chunks from an image are included in analysis.
// Switch implementations to switch between full-image and startup-only analyses.
type ChunkSelector func(nydus.ImageChunks) []nydus.ChunkInfo

// Returns every chunk belonging to the image regardless of access history.
func AllChunks(ic nydus.ImageChunks) []nydus.ChunkInfo { return ic.Chunks }

// Returns the chunks that were fetched during container startup.
func AccessedOnly(ic nydus.ImageChunks) []nydus.ChunkInfo {
	if ic.AccessedChunks == nil {
		panic(fmt.Sprintf("image %q has no startup access data (AccessedChunks is nil) — run startup tracing first or use AllChunks selector", ic.Image))
	}
	return ic.AccessedChunks
}

// Checks that all images have AccessedChunks populated => Should be run before using AccessedOnly selector
func ValidateAccessData(corpus []nydus.ImageChunks) error {
	var missing []string
	for _, ic := range corpus {
		if ic.AccessedChunks == nil {
			missing = append(missing, ic.Image)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing startup access data for %d images: %v", len(missing), missing)
	}
	return nil
}
