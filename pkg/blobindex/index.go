// Package blobindex groups Nydus chunks by blob and resolves byte ranges to the chunks they cover.
package blobindex

import (
	"sort"

	"nydus-cluster-cache/pkg/nydus"
)

// Chunk is a single contiguous region within a blob, identified by its content digest.
type Chunk struct {
	Offset uint64
	Size   uint64
	Digest string
}

// blobEntry holds the image repository name and the sorted chunks for one blob.
type blobEntry struct {
	repo   string
	chunks []Chunk
}

// Index maps blob digests to their sorted chunk lists for fast range queries.
type Index struct {
	blobs map[string]*blobEntry
}

// FromChunks builds an Index from a flat list of ChunkInfo values, grouping by blob
// and sorting each blob's chunks by their compressed offset.
func FromChunks(chunks []nydus.ChunkInfo) *Index {
	idx := &Index{blobs: make(map[string]*blobEntry)}

	for _, c := range chunks {
		entry := idx.blobs[c.BlobDigest]
		if entry == nil {
			entry = &blobEntry{repo: c.Image}
			idx.blobs[c.BlobDigest] = entry
		}
		entry.chunks = append(entry.chunks, Chunk{
			Offset: c.CompressedOffset,
			Size:   c.CompressedSize,
			Digest: c.Hash,
		})
	}

	for _, entry := range idx.blobs {
		sort.Slice(entry.chunks, func(i, j int) bool {
			return entry.chunks[i].Offset < entry.chunks[j].Offset
		})
	}

	return idx
}

// Resolve returns all chunks that overlap the inclusive byte range [start, end] within
// the given blob, along with a flag indicating whether the range is exactly chunk-aligned.
// A range is aligned when it starts at a chunk boundary, ends at a chunk boundary, and the
// covered chunks are contiguous with no gaps.
func (idx *Index) Resolve(blobID string, start, end uint64) ([]Chunk, bool) {
	entry := idx.blobs[blobID]
	if entry == nil {
		return nil, false
	}

	// Collect all chunks that overlap the requested range
	var covered []Chunk
	for _, c := range entry.chunks {
		chunkEnd := c.Offset + c.Size // exclusive
		if chunkEnd > start && c.Offset <= end {
			covered = append(covered, c)
		}
	}

	if len(covered) == 0 {
		return nil, false
	}

	// Check that the range boundaries fall exactly on chunk boundaries
	first := covered[0]
	last := covered[len(covered)-1]
	if first.Offset != start || last.Offset+last.Size != end+1 {
		return covered, false
	}

	// Verify there are no gaps between consecutive chunks
	for i := 1; i < len(covered); i++ {
		if covered[i-1].Offset+covered[i-1].Size != covered[i].Offset {
			return covered, false
		}
	}

	return covered, true
}

// BlobRepo returns the image repository name associated with the given blob digest,
// and reports whether the blob is known to this index.
func (idx *Index) BlobRepo(blobID string) (string, bool) {
	entry := idx.blobs[blobID]
	if entry == nil {
		return "", false
	}
	return entry.repo, true
}
