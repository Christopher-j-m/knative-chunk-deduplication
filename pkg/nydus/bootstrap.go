package nydus

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// The text nydus-image emits at the end of each command, used as a marker when reading output of the command to see when it terminated
	replPrompt = "Inspecting RAFS :> "

	// Limit on how much output the prompt reader may buffer before giving up and assuming an error happened
	maxPromptReadBytes = 1 << 20 // 1 MiB
)

// Holds the metadata fields needed from each blob in a Nydus bootstrap.
// Used to resolve numeric blob indices in chunk records to their content digests.
type blobMeta struct {
	Index              int
	BlobID             string
	CompressedDataSize uint64
	ChunkCount         int
}

// Reads all unique chunks from a Nydus bootstrap by first loading the blob table,
// then streaming the full chunk dump from nydus-image. Chunks are deduplicated by content hash,
// keeping first-seen order.
func ExtractChunks(ctx context.Context, bootstrapPath string, imageName string) (*ImageChunks, error) {
	blobs, err := loadBlobTable(ctx, bootstrapPath)
	if err != nil {
		return nil, fmt.Errorf("error loading the blob table: %w", err)
	}

	// Stream the full chunk dump and parse each record against the blob table
	checkCmd := exec.CommandContext(ctx, "nydus-image", "check", "-B", bootstrapPath, "-v")
	stdoutPipe, err := checkCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("error opening check stdout pipe: %w", err)
	}
	checkCmd.Stderr = io.Discard

	if err := checkCmd.Start(); err != nil {
		return nil, fmt.Errorf("Error during starting the nydus-image check: %w", err)
	}

	chunks, err := parseCheckDump(stdoutPipe, blobs, imageName)
	if err != nil {
		_ = checkCmd.Wait()
		return nil, fmt.Errorf("parse check dump: %w", err)
	}

	if err := checkCmd.Wait(); err != nil {
		return nil, fmt.Errorf("nydus-image check exited: %w", err)
	}

	ic := &ImageChunks{
		Image:  imageName,
		Chunks: chunks,
	}
	ic.TotalBytes = ic.TotalSize()
	return ic, nil
}

// Queries nydus-image for the bootstrap's blob table and returns one entry per blob.
// The tool is driven interactively via stdin because it does not expose a non-interactive listing mode.
func loadBlobTable(ctx context.Context, bootstrapPath string) ([]blobMeta, error) {
	procCtx, killProc := context.WithCancel(ctx)
	defer killProc()

	inspectCmd := exec.CommandContext(procCtx, "nydus-image", "inspect", bootstrapPath)

	stdinPipe, err := inspectCmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open inspect stdin pipe: %w", err)
	}

	defer stdinPipe.Close() //nolint:errcheck
	stdoutPipe, err := inspectCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open inspect stdout pipe: %w", err)
	}

	inspectCmd.Stderr = io.Discard

	if err := inspectCmd.Start(); err != nil {
		return nil, fmt.Errorf("start nydus-image inspect: %w", err)
	}

	r := bufio.NewReader(stdoutPipe)

	if _, err := readUntilPrompt(r); err != nil {
		return nil, fmt.Errorf("wait for initial inspect prompt: %w", err)
	}

	if _, err := io.WriteString(stdinPipe, "blobs\n"); err != nil {
		return nil, fmt.Errorf("write blobs command: %w", err)
	}
	blobsText, err := readUntilPrompt(r)
	if err != nil {
		return nil, fmt.Errorf("wait for blobs response: %w", err)
	}

	blobs, err := parseBlobsTable(blobsText)
	if err != nil {
		return nil, fmt.Errorf("parse blobs response: %w", err)
	}

	_, _ = io.WriteString(stdinPipe, "exit\n")
	if err := inspectCmd.Wait(); err != nil {
		return nil, fmt.Errorf("nydus-image inspect exited: %w", err)
	}

	return blobs, nil
}

// Reads bytes until the REPL output appears, returning everything before it.
func readUntilPrompt(r *bufio.Reader) (string, error) {
	var buf strings.Builder
	sentinel := []byte(replPrompt)
	n := len(sentinel)

	window := make([]byte, 0, n)

	for {
		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				out := buf.String()
				preview := out
				if len(preview) > 200 {
					preview = preview[:200]
				}
				return "", fmt.Errorf("nydus-image inspect exited before emitting prompt (output so far: %q): %w", preview, io.ErrUnexpectedEOF)
			}
			return "", fmt.Errorf("error reading inspect-cmd stdout: %w", err)
		}
		buf.WriteByte(b)
		if buf.Len() > maxPromptReadBytes {
			return "", fmt.Errorf("error in readUntilPrompt: output exceeded 1 MiB without prompt")
		}
		window = append(window, b)
		if len(window) > n {
			window = window[1:]
		}
		if len(window) == n && string(window) == replPrompt {
			// Strip the sentinel from the accumulated buffer
			s := buf.String()
			return s[:len(s)-n], nil
		}
	}
}

// Parses the text output of the blobs listing into one entry per blob.
// Each entry must contain Blob Index, Blob ID, Compressed Data Size, and Chunk Count
func parseBlobsTable(text string) ([]blobMeta, error) {
	var blobs []blobMeta
	var cur *blobMeta
	var haveIndex, haveBlobID, haveCDS, haveCC bool

	flush := func(entryNum int) error {
		if cur == nil {
			return nil
		}
		if !haveIndex {
			return fmt.Errorf("blob entry #%d: missing %q", entryNum, "Blob Index")
		}
		if !haveBlobID {
			return fmt.Errorf("blob entry #%d (index %d): missing %q", entryNum, cur.Index, "Blob ID")
		}
		if !haveCDS {
			return fmt.Errorf("blob entry #%d (index %d): missing %q", entryNum, cur.Index, "Compressed Data Size")
		}
		if !haveCC {
			return fmt.Errorf("blob entry #%d (index %d): missing %q", entryNum, cur.Index, "Chunk Count")
		}
		blobs = append(blobs, *cur)
		return nil
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "Blob Index:") {
			// New entry: flush the previous one
			if cur != nil {
				if err := flush(len(blobs)); err != nil {
					return nil, err
				}
			}
			cur = &blobMeta{}
			haveIndex, haveBlobID, haveCDS, haveCC = false, false, false, false

			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "Blob Index:"))
			val = strings.ReplaceAll(val, ",", "")
			idx, parseErr := strconv.Atoi(val)
			if parseErr != nil {
				return nil, fmt.Errorf("error parsing Blob Index %q: %w", val, parseErr)
			}
			cur.Index = idx
			haveIndex = true
			continue
		}

		if cur == nil {
			continue
		}

		// "Raw Blob ID:" must NOT match the "Blob ID:" branch below, as the former is a different field that appears in the same section of the outpu,
		// but is not needed for chunk resolution
		if strings.HasPrefix(trimmed, "Raw Blob ID:") {
			continue
		}

		if !haveBlobID && strings.HasPrefix(trimmed, "Blob ID:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "Blob ID:"))
			if val == "" {
				return nil, fmt.Errorf("blob entry (index %d): empty %q value", cur.Index, "Blob ID")
			}
			cur.BlobID = val
			haveBlobID = true
			continue
		}

		if !haveCDS && strings.HasPrefix(trimmed, "Compressed Data Size:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "Compressed Data Size:"))
			val = strings.ReplaceAll(val, ",", "")
			v, parseErr := strconv.ParseUint(val, 10, 64)
			if parseErr != nil {
				return nil, fmt.Errorf("parse Compressed Data Size %q (blob index %d): %w", val, cur.Index, parseErr)
			}
			cur.CompressedDataSize = v
			haveCDS = true
			continue
		}

		if !haveCC && strings.HasPrefix(trimmed, "Chunk Count:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "Chunk Count:"))
			val = strings.ReplaceAll(val, ",", "")
			v, parseErr := strconv.Atoi(val)
			if parseErr != nil {
				return nil, fmt.Errorf("error parsing Chunk Count %q (blob index %d): %w", val, cur.Index, parseErr)
			}
			cur.ChunkCount = v
			haveCC = true
			continue
		}
	}

	// Flush the final entry
	if cur != nil {
		if err := flush(len(blobs)); err != nil {
			return nil, err
		}
	}

	if len(blobs) == 0 {
		return nil, fmt.Errorf("no blob entries found in blobs response")
	}
	return blobs, nil
}

// Reads the chunk dump output line by line and returns one unique chunk per content hash.
func parseCheckDump(r io.Reader, blobs []blobMeta, imageName string) ([]ChunkInfo, error) {
	scanner := bufio.NewScanner(r)
	const maxLine = 1 << 20 // Increase buffer cap since some chunk lines will be longer
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	// Build blob_index - BlobID map for faster resolution
	blobByIndex := make(map[int]string, len(blobs))
	for _, b := range blobs {
		blobByIndex[b.Index] = b.BlobID
	}

	var (
		currentFilePath string
		chunks          []ChunkInfo
		seen            = make(map[string]struct{})
		lineNum         int
	)

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimLeft(line, " \t")

		switch {
		case strings.HasPrefix(trimmed, "inode: file "):
			path, err := parseInodePath(trimmed)
			if err != nil {
				return nil, fmt.Errorf("error at line %d: %w", lineNum, err)
			}
			currentFilePath = path

		case strings.HasPrefix(trimmed, "inode: dir "),
			strings.HasPrefix(trimmed, "inode: symlink "):
			currentFilePath = ""

		case strings.HasPrefix(trimmed, "chunk:"):
			ci, err := parseCheckChunkLine(trimmed, currentFilePath, blobByIndex, imageName, len(chunks))
			if err != nil {
				return nil, fmt.Errorf("error at line %d: %w", lineNum, err)
			}

			if _, ok := seen[ci.Hash]; ok {
				continue
			}
			seen[ci.Hash] = struct{}{}
			chunks = append(chunks, ci)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error occured while scan check stdout: %w", err)
	}

	return chunks, nil
}

// Extracts the file path from the quoted section of an inode line.
// Returns an error if the line does not contain a properly quoted path.
func parseInodePath(line string) (string, error) {
	// Find the first '"' and the matching '"' that closes the path
	start := strings.IndexByte(line, '"')
	if start < 0 {
		return "", fmt.Errorf("error at inode - line missing opening quote: %q", line)
	}
	end := strings.IndexByte(line[start+1:], '"')
	if end < 0 {
		return "", fmt.Errorf("error at inode - line missing closing quote: %q", line)
	}
	return line[start+1 : start+1+end], nil
}

// Parses one chunk record line into a ChunkInfo, resolving the blob index to a blob digest.
func parseCheckChunkLine(line, filePath string, blobByIndex map[int]string, imageName string, chunkIndex int) (ChunkInfo, error) {
	// Strip the "chunk:" prefix
	body := strings.TrimSpace(strings.TrimPrefix(line, "chunk:"))

	var (
		id               string
		blobIndex        int
		haveID           bool
		haveBlobIndex    bool
		compressedOffset uint64
		compressedSize   uint64
		haveCompressed   bool
		uncompSize       uint64
		haveUncompSize   bool
	)

	for _, raw := range strings.Split(body, ",") {
		field := strings.TrimSpace(raw)
		if field == "" {
			continue
		}

		// Each field is "<key> <value>" or "<key> <a>/<b>". Split on the first space
		sp := strings.IndexByte(field, ' ')
		if sp < 0 {
			continue
		}

		key := field[:sp]
		val := strings.TrimSpace(field[sp+1:])

		switch key {
		case "id":
			id = val
			haveID = true

		case "blob_index":
			n, err := strconv.Atoi(val)
			if err != nil {
				return ChunkInfo{}, fmt.Errorf("error while parsing 'blob_index' %q: %w", val, err)
			}
			blobIndex = n
			haveBlobIndex = true

		case "compressed":
			offset, size, err := parseSlashOffsetSize(val)
			if err != nil {
				return ChunkInfo{}, fmt.Errorf("error while parsing 'compressed' %q: %w", val, err)
			}
			compressedOffset = offset
			compressedSize = size
			haveCompressed = true
		case "uncompressed":
			size, err := parseSlashSize(val)
			if err != nil {
				return ChunkInfo{}, fmt.Errorf("error while parsing 'uncompressed' %q: %w", val, err)
			}
			uncompSize = size
			haveUncompSize = true
		}
	}

	if !haveID || id == "" {
		return ChunkInfo{}, fmt.Errorf("chunk line missing 'id': %q", line)
	}
	if !haveBlobIndex {
		return ChunkInfo{}, fmt.Errorf("chunk line missing 'blob_index': %q", line)
	}
	if !haveCompressed {
		return ChunkInfo{}, fmt.Errorf("chunk line missing 'compressed' size: %q", line)
	}
	if !haveUncompSize {
		return ChunkInfo{}, fmt.Errorf("chunk line missing 'uncompressed' size: %q", line)
	}

	blobID, ok := blobByIndex[blobIndex]
	if !ok {
		return ChunkInfo{}, fmt.Errorf("chunk references blob_index %d which is not in the bootstrap blob table", blobIndex)
	}

	return ChunkInfo{
		Hash:                      id,
		Size:                      uncompSize,
		CompressedOffset:          compressedOffset,
		CompressedSize:            compressedSize,
		CompressedSizeGranularity: "chunk",
		BlobDigest:                blobID,
		Image:                     imageName,
		ChunkIndex:                chunkIndex,
		FilePath:                  filePath,
	}, nil
}

// Parses an offset/size pair and returns the size component.
func parseSlashSize(s string) (uint64, error) {
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return 0, fmt.Errorf("expected '<offset>/<size>', got %q", s)
	}
	val := strings.TrimSpace(s[slash+1:])
	n, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("non-integer size %q: %w", val, err)
	}
	return n, nil
}

// Parses an offset/size pair and returns both components.
func parseSlashOffsetSize(s string) (offset uint64, size uint64, err error) {
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return 0, 0, fmt.Errorf("expected '<offset>/<size>', got %q", s)
	}
	offset, err = strconv.ParseUint(strings.TrimSpace(s[:slash]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("non-integer offset in %q: %w", s, err)
	}
	size, err = strconv.ParseUint(strings.TrimSpace(s[slash+1:]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("non-integer size in %q: %w", s, err)
	}
	return offset, size, nil
}
