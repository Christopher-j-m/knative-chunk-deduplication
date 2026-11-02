package nydus

import (
	"crypto/sha256"
	"fmt"
)

// Split byte slice into fixed size chunks and return the fixed sized chunks
func FixedChunk(data []byte, chunkSize int, imageName string) []ChunkInfo {
	var fixedSizedChunks []ChunkInfo

	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}

		block := data[offset:end]
		hash := sha256.Sum256(block)

		fixedSizedChunks = append(fixedSizedChunks, ChunkInfo{
			Hash:  fmt.Sprintf("sha256:%x", hash),
			Size:  uint64(len(block)),
			Image: imageName,
		})
	}

	return fixedSizedChunks
}
