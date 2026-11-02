package nydus

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Flatten a image directory by first joining all files into a byte stream before splitting into fixed size chunks as specified
func FlattenImage(exportDir string, chunkSize int, imageName string) ([]ChunkInfo, error) {

	// Walk along the exported image directory and collect all non-empty file paths
	var allPaths []string
	err := filepath.WalkDir(exportDir, func(path string, currDirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip if not a file
		if currDirEntry.Type().IsRegular() {
			// Get file info to check size
			info, err := currDirEntry.Info()
			if err != nil {
				return err
			}

			// Only include non-empty files
			if info.Size() > 0 {
				allPaths = append(allPaths, path)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("error during walking the export dir: %w", err)
	}

	// Sort the file paths by name to ensure same order in every run
	sort.Strings(allPaths)

	// Concatenate all file content into single byte stream
	var stream []byte
	for _, path := range allPaths {
		data, err := os.ReadFile(path)

		if err != nil {
			return nil, fmt.Errorf("error while reading path %s: %w", path, err)
		}

		stream = append(stream, data...)
	}

	// Chunk the file stream at fixed sized chunks
	return FixedChunk(stream, chunkSize, imageName), nil
}
