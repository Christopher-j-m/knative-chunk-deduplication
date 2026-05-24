// Package nydus provides types and tools for reading chunk metadata from Nydus image bootstraps
package nydus

import "fmt"

// Represents a single data block within a Nydus image, identified by its content hash.
// Both compressed and uncompressed sizes are tracked to support storage efficiency analysis.
type ChunkInfo struct {
	Hash                      string `json:"hash"`
	Size                      uint64 `json:"size"`
	CompressedSize            uint64 `json:"compressed_size,omitempty"`
	CompressedSizeGranularity string `json:"compressed_size_granularity,omitempty"` // "chunk" | "blob" | "unknown"
	BlobDigest                string `json:"blob_digest,omitempty"`
	BlobCompressedSize        uint64 `json:"blob_compressed_size,omitempty"`
	Image                     string `json:"image"`
	FileIndex  int    `json:"file_index,omitempty"`
	ChunkIndex int    `json:"chunk_index"`
	FilePath   string `json:"file_path,omitempty"`
}

// Debugging helper to print a chunk in a human-readable format, showing its hash, size, and source image
func (c ChunkInfo) String() string {
	return fmt.Sprintf("%s [%d bytes] from %s", c.Hash, c.Size, c.Image)
}

// Holds all chunks belonging to a single container image, along with optional startup access data.
type ImageChunks struct {
	Image      string      `json:"image"`
	TotalBytes uint64      `json:"total_bytes"`
	Chunks     []ChunkInfo `json:"chunks"`
	// AccessedChunks contains only chunks fetched during a cold container start
	// nil means startup tracing has not run yet for this image
	AccessedChunks []ChunkInfo `json:"accessed_chunks,omitempty"`
}

// Returns deduplicated chunks by hash, keeping the first occurrence of each.
func (ic ImageChunks) UniqueChunks() []ChunkInfo {
	seen := make(map[string]struct{})
	var unique []ChunkInfo
	for _, c := range ic.Chunks {
		if _, ok := seen[c.Hash]; !ok {
			seen[c.Hash] = struct{}{}
			unique = append(unique, c)
		}
	}
	return unique
}

// Returns the total uncompressed size of all chunks in the image.
func (ic ImageChunks) TotalSize() uint64 {
	var total uint64
	for _, c := range ic.Chunks {
		total += c.Size
	}
	return total
}
