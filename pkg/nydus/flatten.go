package nydus

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// FlattenImage concatenates all regular file contents in sorted path order into a single byte stream,
// then chunks the entire stream at fixed boundaries without resetting between files.
// This means a single chunk can span multiple files, which lets identical content be shared even when file boundaries shift.
func FlattenImage(exportDir string, chunkSize int, imageName string) ([]ChunkInfo, error) {
	var paths []string
	err := filepath.WalkDir(exportDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Size() > 0 {
				paths = append(paths, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk export dir: %w", err)
	}

	sort.Strings(paths)

	// Concatenate all file contents into single continuous stream
	var stream []byte
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		stream = append(stream, data...)
	}

	// Chunk the entire stream at fixed boundaries
	return FixedChunk(stream, chunkSize, imageName), nil
}
