package nydus

import "fmt"

// Representation of single chunk of a container image
type ChunkInfo struct {
	Hash             string `json:"hash"`                        // hash of this chunk
	Size             uint64 `json:"size"`                        // size of this chunk in unpacked
	CompressedOffset uint64 `json:"compressed_offset,omitempty"` // offset of this chunk in the respective compressed blob
	CompressedSize   uint64 `json:"compressed_size,omitempty"`   // size of this chunk in the compressed blob
	BlobDigest       string `json:"blob_digest,omitempty"`       // blob id that contains the chunk in our registry
	Image            string `json:"image"`                       // container image name where this chunk belongs
	FilePath         string `json:"file_path,omitempty"`         // filepath to the file where this chunk belongs
}

// Helper for local debugging puposes only
func (c ChunkInfo) String() string {
	return fmt.Sprintf("Hash %s with %d bytes from Image %s", c.Hash, c.Size, c.Image)
}

// Representation of a container image and all its chunks
type ImageChunks struct {
	Image          string      `json:"image"`                     // container image name
	TotalBytes     uint64      `json:"total_bytes"`               // size of all uncompressed chunks in the image
	Chunks         []ChunkInfo `json:"chunks"`                    // chunks of this image
	AccessedChunks []ChunkInfo `json:"accessed_chunks,omitempty"` // chunks that are accessed during cold start => only filled via scripts/overlap/run-startup-trace.sh
}

// Return a deduplicated list of chunks
func (allImageChunks ImageChunks) UniqueChunks() []ChunkInfo {
	seen := make(map[string]struct{})
	var uniqueChunks []ChunkInfo

	for _, currChunk := range allImageChunks.Chunks {

		if _, ok := seen[currChunk.Hash]; !ok {
			seen[currChunk.Hash] = struct{}{}
			uniqueChunks = append(uniqueChunks, currChunk)
		}

	}

	return uniqueChunks
}

// Return the uncompressed size of all chunks in a given image chunk list
func (allImageChunks ImageChunks) TotalSize() uint64 {
	var totalSize uint64

	for _, currChunk := range allImageChunks.Chunks {
		totalSize += currChunk.Size
	}

	return totalSize
}
