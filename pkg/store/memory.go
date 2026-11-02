package store

import (
	"bytes"
	"io"
	"sync"
)

// The in-memory store for actually storing the chunks of the cluster cache
type MemoryStore struct {
	mutex  sync.RWMutex      // Mutex to enable concurrent access to the store
	chunks map[string][]byte // Map to store the image chunks => key is the hash of the chunk, value is the chunk itself
	bytes  uint64            // Sum of the size of all chunks in the store
}

// Create a new store
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{chunks: make(map[string][]byte)}
}

// Returns a reader for the chunk of the specific hash or ErrNotFound if not found
func (store *MemoryStore) Get(hash string) (io.ReadCloser, error) {

	// RLock is a shared version of the mutex that allows multiple concurrent readers but only one writer
	// (ofc the writer will block all readers until it is done <=> and readers will block the writer until they are done)
	store.mutex.RLock()
	payload, ok := store.chunks[hash]
	store.mutex.RUnlock()

	if !ok {
		return nil, ErrNotFound
	}

	// Callers close the reader, but memory needs no cleanup => close does nothing
	return io.NopCloser(bytes.NewReader(payload)), nil
}

// Store a given chunk under the given hash
// Since this is a put op, overwriting of the previous chunk may happen if the corresponding hash already exists
func (store *MemoryStore) Put(hash string, payload io.Reader) error {

	// Get the new chunk bytes
	newChunkBytes, err := io.ReadAll(payload)
	if err != nil {
		return err
	}

	// Lock the store before we do any writing to the store
	store.mutex.Lock()

	// Unlock after fkt returns
	defer store.mutex.Unlock()

	// If we already have a chunk with the same hash
	// => remove the byte size of the currently stored chunk from the total byte counter of the store
	if existing, ok := store.chunks[hash]; ok {
		store.bytes -= uint64(len(existing))
	}

	// Store the new chunk at the given hash and add the new chunk size to the total byte counter of our store
	store.chunks[hash] = newChunkBytes
	store.bytes += uint64(len(newChunkBytes))
	return nil
}

// Returns true if the store already has a chunk with the specific hash, otherwise returns false
func (store *MemoryStore) Exists(hash string) bool {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	_, ok := store.chunks[hash]
	return ok
}

// Deteles a chunk that is stored under the given hash, does nothing if such an entry doesnt exist yet
func (store *MemoryStore) Delete(hash string) error {

	// Same as in put, we need to lock the store before we do any writing/deleting
	store.mutex.Lock()
	defer store.mutex.Unlock() // Unlock afterwards...

	// Delete the corresponding chunk if there is one under the given hash
	if existing, ok := store.chunks[hash]; ok {
		store.bytes -= uint64(len(existing))
		delete(store.chunks, hash)
	}

	return nil
}

// Returns the total number of bytes of all chunks in the store combined
func (store *MemoryStore) StoredBytes() uint64 {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	return store.bytes
}

// Returns the number of chunks in the store currently
func (store *MemoryStore) ChunkCount() int {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	return len(store.chunks)
}

// Returns a list of all chunks in the store, including their hash and size in bytes
func (store *MemoryStore) Chunks() []ChunkInfo {
	store.mutex.RLock()
	defer store.mutex.RUnlock()

	listing := make([]ChunkInfo, 0, len(store.chunks))
	for hash, payload := range store.chunks {
		listing = append(listing, ChunkInfo{Hash: hash, Bytes: uint64(len(payload))})
	}

	return listing
}
