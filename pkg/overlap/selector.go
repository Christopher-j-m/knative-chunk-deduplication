package overlap

import (
	"fmt"
	"nydus-cluster-cache/pkg/nydus"
)

// Selects which chunks from an image are included in the overlap analysis
type ChunkSelector func(nydus.ImageChunks) []nydus.ChunkInfo

// Returns all chunks of an image
func AllChunks(image nydus.ImageChunks) []nydus.ChunkInfo { return image.Chunks }

// Returns the chunks that were fetched during container startup
func AccessedOnly(image nydus.ImageChunks) []nydus.ChunkInfo {
	if image.AccessedChunks == nil {
		panic(fmt.Sprintf("image %q has no startup access data", image.Image))
	}
	return image.AccessedChunks
}

// Checks that all images have AccessedChunks populated => Should be run before using AccessedOnly selector
func ValidateAccessData(allImages []nydus.ImageChunks) error {
	var missing []string
	for _, currImage := range allImages {
		if currImage.AccessedChunks == nil {
			missing = append(missing, currImage.Image)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing startup access data for %d images: %v", len(missing), missing)
	}

	return nil
}
