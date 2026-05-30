package store

import (
	"bytes"
	"io"
	"sync"
)

// MemoryStore keeps chunk payloads in a map. Safe for concurrent readers and
// writers via a single RWMutex; replace with sharded locks if profiling shows
// contention on the chunk hash space.
type MemoryStore struct {
	mu     sync.RWMutex
	chunks map[string][]byte
	bytes  uint64
}

// NewMemoryStore creates an empty in-memory store. Callers can populate it via Put.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{chunks: make(map[string][]byte)}
}

// Get returns a reader for the chunk identified by hash, or ErrNotFound.
func (m *MemoryStore) Get(hash string) (io.ReadCloser, error) {
	m.mu.RLock()
	payload, ok := m.chunks[hash]
	m.mu.RUnlock()

	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(payload)), nil
}

// Put stores the bytes from payload under hash. Overwrites are allowed and
// adjust the stored-bytes accounting accordingly.
func (m *MemoryStore) Put(hash string, payload io.Reader) error {
	buffer, err := io.ReadAll(payload)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.chunks[hash]; ok {
		m.bytes -= uint64(len(existing))
	}
	m.chunks[hash] = buffer
	m.bytes += uint64(len(buffer))
	return nil
}

// Exists reports whether the chunk is currently stored, without reading bytes.
func (m *MemoryStore) Exists(hash string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.chunks[hash]
	return ok
}

// Delete removes a chunk. Returns nil if the chunk did not exist.
func (m *MemoryStore) Delete(hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.chunks[hash]; ok {
		m.bytes -= uint64(len(existing))
		delete(m.chunks, hash)
	}
	return nil
}

// StoredBytes reports the total number of payload bytes currently held.
func (m *MemoryStore) StoredBytes() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bytes
}

// ChunkCount reports the number of distinct chunks currently held.
func (m *MemoryStore) ChunkCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.chunks)
}
