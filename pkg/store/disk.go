package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// DiskStore writes each chunk to a file named after its hash. An in-memory
// index keeps Exists, StoredBytes, and ChunkCount cheap to answer without
// touching disk. The index is rebuilt by scanning the directory on startup.
type DiskStore struct {
	root string

	mu    sync.RWMutex
	sizes map[string]uint64
	bytes uint64
}

// NewDiskStore opens or creates a disk store rooted at the given directory.
// The directory is created if it does not exist. Existing chunk files are
// indexed so that Exists and StoredBytes reflect the persisted state.
func NewDiskStore(root string) (*DiskStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create root: %w", err)
	}

	d := &DiskStore{root: root, sizes: make(map[string]uint64)}
	if err := d.rebuildIndex(); err != nil {
		return nil, err
	}
	return d, nil
}

// rebuildIndex scans root and populates the in-memory index from existing
// chunk files. It is called once during construction and does not need locking
// because the DiskStore is not yet visible to callers.
func (d *DiskStore) rebuildIndex() error {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return fmt.Errorf("read root: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", entry.Name(), err)
		}
		d.sizes[entry.Name()] = uint64(info.Size())
		d.bytes += uint64(info.Size())
	}
	return nil
}

// path returns the absolute path for the file that stores hash.
func (d *DiskStore) path(hash string) string {
	return filepath.Join(d.root, hash)
}

// Get returns a reader for the chunk identified by hash, or ErrNotFound.
// The caller must close the returned reader when finished.
func (d *DiskStore) Get(hash string) (io.ReadCloser, error) {
	f, err := os.Open(d.path(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open chunk: %w", err)
	}
	return f, nil
}

// Put stores the bytes from payload under hash. The payload is first written
// to a temporary file in the same directory, then atomically renamed to the
// final path, ensuring readers always see a complete payload.
func (d *DiskStore) Put(hash string, payload io.Reader) error {
	final := d.path(hash)
	tmp, err := os.CreateTemp(d.root, hash+".part-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}

	written, err := io.Copy(tmp, payload)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write chunk: %w", err)
	}

	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("rename chunk: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if previous, ok := d.sizes[hash]; ok {
		d.bytes -= previous
	}
	d.sizes[hash] = uint64(written)
	d.bytes += uint64(written)
	return nil
}

// Exists reports whether the chunk is currently stored, without reading bytes.
func (d *DiskStore) Exists(hash string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.sizes[hash]
	return ok
}

// Delete removes a chunk. Returns nil if the chunk did not exist.
func (d *DiskStore) Delete(hash string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	previous, ok := d.sizes[hash]
	if !ok {
		return nil
	}
	if err := os.Remove(d.path(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove chunk: %w", err)
	}
	delete(d.sizes, hash)
	d.bytes -= previous
	return nil
}

// StoredBytes reports the total number of payload bytes currently held.
func (d *DiskStore) StoredBytes() uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.bytes
}

// ChunkCount reports the number of distinct chunks currently held.
func (d *DiskStore) ChunkCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.sizes)
}

// Chunks returns one entry per stored chunk together with its payload size.
// The sizes come from the in-memory index, so no files are read.
func (d *DiskStore) Chunks() []ChunkInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()

	listing := make([]ChunkInfo, 0, len(d.sizes))
	for hash, size := range d.sizes {
		listing = append(listing, ChunkInfo{Hash: hash, Bytes: size})
	}
	return listing
}
