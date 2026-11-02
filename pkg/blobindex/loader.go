// Loads the image metadata files that contain chunk information (such as chunk offset) for the images we use
package blobindex

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"nydus-cluster-cache/pkg/nydus"
)

// Function in cmd/chunk-proxy/main.go that returns a chunk list for a given path with these files
// => Loader only knows how to find the metadata files, but not how to read and parse it
type Extractor func(ctx context.Context, bootstrapPath, imageName string) ([]nydus.ChunkInfo, error)

// The whole state of the loader
type Loader struct {
	// Fixed settings that are set once
	root            string    // dir for scanning for the metadata files
	bootstrapName   string    // file name to search in the dir tree (only try to parse /any/depth/<bootstrapName>)
	extract         Extractor // function to read a single metadata file and return its chunk list
	requireRepoHint bool

	// Flexible state vars that may change during runtime
	mutex  sync.RWMutex
	seen   map[string]struct{}
	chunks []nydus.ChunkInfo
	index  *Index
}

// Change the state/settings of a given loader instance
type Option func(*Loader)

// RequireRepoHint makes the Loader hold off on any bootstrap whose sibling image.repo
// file has not been written yet, leaving it unseen so a later refresh indexes it once
// the hint exists
// => The nydus snapshotter unpacks the bootstrap before its mount step writes
// the hint and indexing inside that window would pin the blobs to the meaningless
// parent directory name
func RequireRepoHint() Option {
	return func(loader *Loader) { loader.requireRepoHint = true }
}

// Creates a new loader that can traverse the root dir looking for files named bootstrapName via refresh func and
// runs extract func to obtain chunk metadata for each one it finds
func NewLoader(dir, bootstrapName string, extract Extractor, opts ...Option) *Loader {
	loader := &Loader{
		root:          dir,
		bootstrapName: bootstrapName,
		extract:       extract,
		seen:          make(map[string]struct{}),
		index:         FromChunks(nil),
	}
	for _, opt := range opts {
		opt(loader)
	}
	return loader
}

// Traverse the root dir again and look for image metadata files named bootstrapName, but only extract the chunk list
// for those files that weren't known from previous scans
// => Still rebuilds the whole index, but only returns the first extraction error to indicate failure without stopping the scan
func (loader *Loader) Refresh(ctx context.Context) error {
	loader.mutex.Lock()
	defer loader.mutex.Unlock()

	var firstErr error

	walkErr := filepath.WalkDir(loader.root, func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if dirEntry.IsDir() || dirEntry.Name() != loader.bootstrapName {
			return nil
		}

		// Skip files we've already seen
		if _, ok := loader.seen[path]; ok {
			return nil
		}

		// The image name is the parent directory of the bootstrap file, unless
		// a sibling image.repo file names the real repository
		// => the nydus snapshotter writes that hint because its directory layout carries no image name
		image := filepath.Base(filepath.Dir(path))
		hinted := false
		if hint, hintErr := os.ReadFile(filepath.Join(filepath.Dir(path), "image.repo")); hintErr == nil {
			if repoName := strings.TrimSpace(string(hint)); repoName != "" {
				image = repoName
				hinted = true
			}
		}

		// The hint arrives a moment after the bootstrap itself, so when hints are
		// required an unhinted bootstrap is not ready yet, so we leave it unseen and let a later refresh do the work
		if loader.requireRepoHint && !hinted {
			return nil
		}

		// TODO: Find cleaner solution here for err handling...
		chunks, extractionErr := loader.extract(ctx, path, image)
		if extractionErr != nil {
			// Record the first extraction error but continue so one image doesn't hide the rest
			// Leave the bootstrap unseen so a temporary/random failure is retried on the next refresh
			if firstErr == nil {
				firstErr = fmt.Errorf("error during extraction at %s: %w", path, extractionErr)
			}
			return nil
		}

		// Mark seen only after a successful extraction so failures can be retried later during another refresh iteration
		loader.seen[path] = struct{}{}
		loader.chunks = append(loader.chunks, chunks...)
		return nil
	})

	if walkErr != nil {
		return fmt.Errorf("error during walking at %s: %w", loader.root, walkErr)
	}

	// Rebuild from every chunk gathered so far, including earlier refreshes, even when
	// some bootstrap failed this round
	loader.index = FromChunks(loader.chunks)
	return firstErr
}

// Return the most recently parsed blob index
func (loader *Loader) Index() *Index {
	loader.mutex.RLock()
	defer loader.mutex.RUnlock()
	return loader.index
}
