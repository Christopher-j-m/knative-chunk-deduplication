package store

import (
	"errors"
	"io"
)

// Returned by Get function when no chunk with the hash was found/stored yet
var ErrNotFound = errors.New("chunk not found")

// Underlying layer of the cluster cache for storing the actual bytes of a chunk
// => This layer is not responsible for cache metrics or eviction strats
type Store interface {

	// Returns a reader for the bytes stored under hash, or ErrNotFound if no such chunk exists
	// Note: Apparently returning a reader is the best practice (?) because it allows the caller to stream the data instead of reading it all into memory at once
	Get(hash string) (io.ReadCloser, error)

	// Writes a chunk (overwriting allowed) and the readers of the chunk will see either the old or the new payload, but never a mix of both
	Put(hash string, payload io.Reader) error

	// Returns whether a chunk with a given hash exist or not, but it does not read any bytes
	Exists(hash string) bool

	// Removes a chunk or returns nil if the chunk did not exist
	Delete(hash string) error

	// Returns the total number of bytes currently held in the store
	StoredBytes() uint64

	// Returns the number of unique chunks currently in the store
	ChunkCount() int
}

// A single chunk info object returned by the Lister interface
type ChunkInfo struct {
	Hash  string `json:"hash"`
	Bytes uint64 `json:"size"`
}

// Interface to allow returning a list of all chunks currently in the store
// => This is mandatory for determining which chunks are used e.g. during cold starts when we isolate to one pod and a clean store
type Lister interface {
	Chunks() []ChunkInfo
}
