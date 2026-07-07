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

// Extractor is a function that reads chunk metadata from a bootstrap file at the given
// path and associates it with the provided image name.
type Extractor func(ctx context.Context, bootstrapPath, imageName string) ([]nydus.ChunkInfo, error)

// Loader scans a directory tree for bootstrap files and builds a blob Index from them.
// It tracks which bootstraps have already been read so repeated refreshes only pay
// for newly appeared files.
type Loader struct {
	root            string
	bootstrapName   string
	extract         Extractor
	requireRepoHint bool

	mu     sync.RWMutex
	seen   map[string]struct{}
	chunks []nydus.ChunkInfo
	index  *Index
}

// Option customises how a Loader treats the bootstrap files it discovers.
type Option func(*Loader)

// RequireRepoHint makes the Loader hold off on any bootstrap whose sibling image.repo
// file has not been written yet, leaving it unseen so a later refresh indexes it once
// the hint exists. The snapshotter unpacks the bootstrap before its mount step writes
// the hint, and indexing inside that window would pin the blobs to the meaningless
// parent directory name for good.
func RequireRepoHint() Option {
	return func(l *Loader) { l.requireRepoHint = true }
}

// NewLoader creates a Loader that walks dir looking for files named bootstrapName and
// calls extract to obtain chunk metadata for each one it finds.
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

// Refresh walks the root directory, extracts chunks from any bootstrap files not yet
// seen, and rebuilds the current index. It is safe to call concurrently with Index.
func (l *Loader) Refresh(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	var firstErr error

	walkErr := filepath.WalkDir(l.root, func(path string, d fs.DirEntry, err error) error {
		// Swallow entry errors so one unreadable subtree doesn't abort the scan
		if err != nil {
			return nil
		}

		if d.IsDir() || d.Name() != l.bootstrapName {
			return nil
		}

		// Skip bootstraps we have already processed
		if _, ok := l.seen[path]; ok {
			return nil
		}

		// The image name is the parent directory of the bootstrap file, unless
		// a sibling image.repo file names the real repository; the snapshotter
		// writes that hint because its directory layout carries no image name
		image := filepath.Base(filepath.Dir(path))
		hinted := false
		if hint, hintErr := os.ReadFile(filepath.Join(filepath.Dir(path), "image.repo")); hintErr == nil {
			if repoName := strings.TrimSpace(string(hint)); repoName != "" {
				image = repoName
				hinted = true
			}
		}

		// The hint arrives a moment after the bootstrap itself, so when hints are
		// required an unhinted bootstrap is simply not ready yet: leave it unseen
		// and let a later refresh index it under its real repository
		if l.requireRepoHint && !hinted {
			return nil
		}

		chunks, exErr := l.extract(ctx, path, image)
		if exErr != nil {
			// Record the first extraction error but continue so one bad image doesn't hide the rest
			// Leave the bootstrap unseen so a transient failure is retried on the next refresh
			if firstErr == nil {
				firstErr = fmt.Errorf("extract %s: %w", path, exErr)
			}
			return nil
		}

		// Mark seen only after a successful extraction so failures can be retried later
		l.seen[path] = struct{}{}
		l.chunks = append(l.chunks, chunks...)
		return nil
	})

	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", l.root, walkErr)
	}

	// Rebuild from every chunk gathered so far, including earlier refreshes, even when
	// some bootstrap failed this round
	l.index = FromChunks(l.chunks)
	return firstErr
}

// Index returns the most recently built blob index. It is safe to call concurrently
// with Refresh and always returns a non-nil value.
func (l *Loader) Index() *Index {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.index
}
