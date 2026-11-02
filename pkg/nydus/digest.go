package nydus

import (
	"encoding/hex"

	"lukechampine.com/blake3"
)

// MatchesChunkDigest reports whether the bytes hash to the chunk digest. Nydus
// names a chunk by the BLAKE3 hash of its uncompressed content, written as
// plain hex, and the corpus images store their chunks uncompressed.
func MatchesChunkDigest(data []byte, digest string) bool {
	sum := blake3.Sum256(data)
	return hex.EncodeToString(sum[:]) == digest
}
