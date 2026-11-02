// Persistent store version of /pkg/store/memory. Each chunk is stored in a file named after its hash, and an in-memory index keeps track of the sizes of each chunk. The index is rebuilt on startup by scanning the directory for existing chunk files
// This implementation is not used yet and is just an idea to do experiments with so we may be able to compare its performance against the in-memory store we used until now...
package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type DiskStore struct {
	rootDir string
	mutex   sync.RWMutex
	sizes   map[string]uint64
	bytes   uint64
}

func NewDiskStore(rootDir string) (*DiskStore, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("create root: %w", err)
	}

	newDiskStore := &DiskStore{rootDir: rootDir, sizes: make(map[string]uint64)}
	if err := newDiskStore.rebuildIndex(); err != nil {
		return nil, err
	}
	return newDiskStore, nil
}

func (store *DiskStore) rebuildIndex() error {
	entries, err := os.ReadDir(store.rootDir)
	if err != nil {
		return fmt.Errorf("error reading the root dir: %w", err)
	}

	for _, entry := range entries {

		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("couldnt get the files metadata for %s: %w", entry.Name(), err)
		}

		store.sizes[entry.Name()] = uint64(info.Size())
		store.bytes += uint64(info.Size())
	}

	return nil
}

func (store *DiskStore) path(hash string) string {
	return filepath.Join(store.rootDir, hash)
}

func (store *DiskStore) Get(hash string) (io.ReadCloser, error) {
	file, err := os.Open(store.path(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("error occured while trying to open chunk: %w", err)
	}

	return file, nil
}

func (store *DiskStore) Put(hash string, payload io.Reader) error {
	chunkDestFilePath := store.path(hash)
	tmp, err := os.CreateTemp(store.rootDir, hash+".part-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}

	newChunkWrittenBytes, err := io.Copy(tmp, payload)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("error while writing chunk: %w", err)
	}

	if err := os.Rename(tmp.Name(), chunkDestFilePath); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("error during renaming chunk: %w", err)
	}

	store.mutex.Lock()
	defer store.mutex.Unlock()
	if prevChunkBytes, ok := store.sizes[hash]; ok {
		store.bytes -= prevChunkBytes
	}

	store.sizes[hash] = uint64(newChunkWrittenBytes)
	store.bytes += uint64(newChunkWrittenBytes)
	return nil
}

func (store *DiskStore) Exists(hash string) bool {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	_, ok := store.sizes[hash]
	return ok
}

func (store *DiskStore) Delete(hash string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	prevChunkBytes, ok := store.sizes[hash]
	if !ok {
		return nil
	}

	if err := os.Remove(store.path(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("error occured while trying to remove chunk: %w", err)
	}

	delete(store.sizes, hash)
	store.bytes -= prevChunkBytes
	return nil
}

func (store *DiskStore) StoredBytes() uint64 {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	return store.bytes
}

func (store *DiskStore) ChunkCount() int {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	return len(store.sizes)
}

func (store *DiskStore) Chunks() []ChunkInfo {
	store.mutex.RLock()
	defer store.mutex.RUnlock()

	listing := make([]ChunkInfo, 0, len(store.sizes))
	for hash, size := range store.sizes {
		listing = append(listing, ChunkInfo{Hash: hash, Bytes: size})
	}
	return listing
}
