package store

import (
	"errors"
	"io"
)

// ErrNotFound is returned by Get when no chunk is associated with the hash
var ErrNotFound = errors.New("chunk not found")

// Store is the byte-level persistence contract used by the cache facade.
// Implementations must be safe for concurrent use.
type Store interface {
	// Get returns a reader for the chunk identified by hash, or ErrNotFound
	// Callers must close the reader when finished
	Get(hash string) (io.ReadCloser, error)

	// Put stores the bytes from payload under hash; overwriting an existing hash
	// is allowed and replaces the previous payload atomically from the
	// reader's point of view
	Put(hash string, payload io.Reader) error

	// Exists reports whether the chunk is currently stored, without reading bytes
	Exists(hash string) bool

	// Delete removes a chunk; returns nil if the chunk did not exist
	Delete(hash string) error

	// StoredBytes reports the total number of payload bytes currently held
	StoredBytes() uint64

	// ChunkCount reports the number of distinct chunks currently held
	ChunkCount() int
}
