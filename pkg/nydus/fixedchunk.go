package nydus

import (
	"crypto/sha256"
	"fmt"
)

// Splits a byte slice into fixed-size blocks and returns each as a ChunkInfo with its hash.
// The last block may be smaller than chunkSize when the data length is not an exact multiple.
func FixedChunk(data []byte, chunkSize int, imageName string) []ChunkInfo {
	var chunks []ChunkInfo

	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}

		block := data[offset:end]
		hash := sha256.Sum256(block)

		chunks = append(chunks, ChunkInfo{
			Hash:       fmt.Sprintf("sha256:%x", hash),
			Size:       uint64(len(block)),
			Image:      imageName,
			ChunkIndex: offset / chunkSize,
		})
	}

	return chunks
}
