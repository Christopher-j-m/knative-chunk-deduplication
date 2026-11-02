// Groups chunks by blobs and resolves byte ranges to chunks => THe cache stores chunks by digest, but nydus asks our cache for byte ranges
package blobindex

import (
	"nydus-cluster-cache/pkg/nydus"
	"sort"
)

// Generated visualization about this resolvers data structure connection:
// Index
// └─ blobs: map[blobDigest] → *blobEntry
//                              ├─ repo:   "python-ml"
//                              └─ chunks: []Chunk  (sorted by Offset)
//                                          ├─ Offset, Size, Digest
//                                          └─ ...

type Chunk struct {
	Offset uint64
	Size   uint64
	Digest string
}

type blobEntry struct {
	repo   string
	chunks []Chunk
}

type Index struct {
	blobs map[string]*blobEntry
}

// Convert an unordered list of chunks (possibly) many different blobs and sorts them by corresponding blob and position/offset
func FromChunks(chunks []nydus.ChunkInfo) *Index {
	sortedIndexList := &Index{blobs: make(map[string]*blobEntry)}
	seenChunks := make(map[string]map[uint64]struct{})

	// Create a new blob entry for the first time we see a chunk for a specific corresponding blob
	for _, currChunk := range chunks {
		entry := sortedIndexList.blobs[currChunk.BlobDigest]
		if entry == nil {
			entry = &blobEntry{repo: currChunk.Image}
			sortedIndexList.blobs[currChunk.BlobDigest] = entry
			seenChunks[currChunk.BlobDigest] = make(map[uint64]struct{})
		}

		// Duplicate chunk found => discard
		if _, duplicate := seenChunks[currChunk.BlobDigest][currChunk.CompressedOffset]; duplicate {
			continue
		}
		seenChunks[currChunk.BlobDigest][currChunk.CompressedOffset] = struct{}{}

		// Add the current chunk to its corresponding blob entry (not sorted yet)
		entry.chunks = append(entry.chunks, Chunk{
			Offset: currChunk.CompressedOffset,
			Size:   currChunk.CompressedSize,
			Digest: currChunk.Hash,
		})
	}

	// Sort each chunk in all blob entries by offset
	for _, entry := range sortedIndexList.blobs {
		sort.Slice(entry.chunks, func(i, j int) bool {
			return entry.chunks[i].Offset < entry.chunks[j].Offset
		})
	}

	return sortedIndexList
}

// Return the chunk(s) cover the given range of the given blob
// May be multiple chunks when the range doesnt start and/or end the exact borders of a single chunk
// Returns the corresponding chunks in the range + a boolean that indicates whether the chunks are fitting perfectly or not (e.g. gaps inbetween, no chunks at all)
func (indexList *Index) Resolve(blobID string, start, end uint64) ([]Chunk, bool) {
	// Get the corresponding blob entry
	foundBlobEntry := indexList.blobs[blobID]
	if foundBlobEntry == nil {
		return nil, false
	}

	// Get all chunks within that blob entry that have overlap of the requested range
	var covered []Chunk
	for _, currChunk := range foundBlobEntry.chunks {
		chunkEnd := currChunk.Offset + currChunk.Size // exclusive
		if chunkEnd > start && currChunk.Offset <= end {
			covered = append(covered, currChunk)
		}
	}

	// No chunk within the given range found...
	if len(covered) == 0 {
		return nil, false
	}

	// Check that the range boundaries fall exactly on chunk boundaries
	first := covered[0]
	last := covered[len(covered)-1]
	if first.Offset != start || last.Offset+last.Size != end+1 {
		return covered, false
	}

	// Verify that between two consecutive chunks are no gaps in-between
	for i := 1; i < len(covered); i++ {
		if covered[i-1].Offset+covered[i-1].Size != covered[i].Offset {
			return covered, false
		}
	}

	return covered, true
}

// Return the container registry repo for a specific blob => Where this blob is located at
func (indexList *Index) BlobRepo(blobID string) (string, bool) {
	entry := indexList.blobs[blobID]
	if entry == nil {
		return "", false
	}
	return entry.repo, true
}
