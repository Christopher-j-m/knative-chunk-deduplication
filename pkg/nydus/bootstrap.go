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
	// cli text that nydus shows when waiting for the next command
	nydusReadyMarker = "Inspecting RAFS :> "

	// Stop after a certain amount of output from nydus before assuming failure...
	maxPromptReadBytes = 1 << 20 // 1 MiB
)

// Blob metadata returned from nydus-image inspect cmd
type blobMeta struct {
	Index  int    // Index
	BlobID string // Hash (named Blob Id in output, so we just try being consistent)
}

// Reads all unique chunks from a Nydus bootstrap by first loading the blob table,
// then streaming the full chunk dump from nydus-image. Chunks are deduplicated by content hash,
// keeping first-seen order.
func ExtractChunks(ctx context.Context, bootstrapPath string, imageName string) (*ImageChunks, error) {

	// Load the blob table from the bootstrap file
	blobs, err := loadBlobTable(ctx, bootstrapPath)
	if err != nil {
		return nil, fmt.Errorf("error while loading the blob table via bootstrap manifest file: %w", err)
	}

	// RUn the nydus-image check cmd to get all the chunks
	checkCmd := exec.CommandContext(ctx, "nydus-image", "check", "-B", bootstrapPath, "-v")
	stdoutPipe, err := checkCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("error occurred while opening the check stdout pipe: %w", err)
	}
	checkCmd.Stderr = io.Discard // We dont need stderr output for parsind the chunk metadata

	if err := checkCmd.Start(); err != nil {
		return nil, fmt.Errorf("error running the nydus-image check cmd: %w", err)
	}

	// Parse the output of the check command to extract the chunk list
	chunks, err := parseCheckDump(stdoutPipe, blobs, imageName)
	if err != nil {
		_ = checkCmd.Wait()
		return nil, fmt.Errorf("error during parsing of the check cmd output: %w", err)
	}

	if err := checkCmd.Wait(); err != nil {
		return nil, fmt.Errorf("error during exiting the nydus-image check: %w", err)
	}

	allImageChunks := &ImageChunks{
		Image:  imageName,
		Chunks: chunks,
	}
	allImageChunks.TotalBytes = allImageChunks.TotalSize()
	return allImageChunks, nil
}

// For a given bootstrap metadata file, this function runs nydus-image inspect to retrieve the blob metadata
// => There is no non-interactive way to get the blobs from the bootstrap file apparently rn...
func loadBlobTable(ctx context.Context, bootstrapPath string) ([]blobMeta, error) {
	processCtx, killProcess := context.WithCancel(ctx)
	defer killProcess()

	// Start nydus-image inspect in interactive mode and start reading its output
	inspectCmd := exec.CommandContext(processCtx, "nydus-image", "inspect", bootstrapPath)
	stdinPipe, err := inspectCmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("error occurred while trying to open inspect stdin pipe: %w", err)
	}

	defer stdinPipe.Close() //nolint:errcheck
	stdoutPipe, err := inspectCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("error while trying to open inspect stdout pipe: %w", err)
	}

	// We dont need stderr output for parsing the blob metadata... hence we discard it
	inspectCmd.Stderr = io.Discard

	// Run the inspect command
	if err := inspectCmd.Start(); err != nil {
		return nil, fmt.Errorf("Running cmd: nydus-image inspect: %w", err)
	}

	// Read the output of the last inspect command until we get the prompt
	nydusOutputReader := bufio.NewReader(stdoutPipe)
	if _, err := readUntilPrompt(nydusOutputReader); err != nil {
		return nil, fmt.Errorf("error occurred while waiting for the inspect prompt: %w", err)
	}

	// Send the "blobs" command as followup and wait for the subsequent prompt again...
	if _, err := io.WriteString(stdinPipe, "blobs\n"); err != nil {
		return nil, fmt.Errorf("error occurred while responding to prompt with blobs command: %w", err)
	}
	blobsText, err := readUntilPrompt(nydusOutputReader)
	if err != nil {
		return nil, fmt.Errorf("error occurred while waiting for the blobs command response: %w", err)
	}

	// Parse the blobs output into the blobMeta form
	blobs, err := parseBlobsTable(blobsText)
	if err != nil {
		return nil, fmt.Errorf("error occurred while parsing the blobs response: %w", err)
	}

	// Run exit cmd to end the inspect process that we started earlier
	_, _ = io.WriteString(stdinPipe, "exit\n")
	if err := inspectCmd.Wait(); err != nil {
		return nil, fmt.Errorf("error while trying to exit the nydus-image inspect process: %w", err)
	}

	return blobs, nil
}

// Reads from the specific reader until the nydus prompt is active
// => Returns the output before the prompt came
func readUntilPrompt(reader *bufio.Reader) (string, error) {
	var buffer strings.Builder
	nydusExpectedPromptLen := len(nydusReadyMarker)
	window := make([]byte, 0, nydusExpectedPromptLen)

	// Read byte by byte until we find the prompt or reach EOF
	for {
		currByte, err := reader.ReadByte()
		if err != nil {
			if err == io.EOF {
				out := buffer.String()
				preview := out
				if len(preview) > 200 {
					preview = preview[:200]
				}
				return "", fmt.Errorf("nydus-image inspect exited before a prompt appeared - output so far: %q, error: %w", preview, io.ErrUnexpectedEOF)
			}
			return "", fmt.Errorf("error while reading the inspect-cmd output: %w", err)
		}

		// Append the current byte to the buffer and throw if we are exceeding our max ´size to read until assuming an error or stuck
		buffer.WriteByte(currByte)
		if buffer.Len() > maxPromptReadBytes {
			return "", fmt.Errorf("Output exceeded %d bytes without prompt in readUntilPrompt fkt", maxPromptReadBytes)
		}

		// Sliding window to check for the expected prompt
		window = append(window, currByte)
		if len(window) > nydusExpectedPromptLen {
			window = window[1:]
		}

		// Check if the current window matches the expected prompt
		if len(window) == nydusExpectedPromptLen && string(window) == nydusReadyMarker {

			// Return the output before the prompt
			lastOutput := buffer.String()
			return lastOutput[:len(lastOutput)-nydusExpectedPromptLen], nil
		}
	}
}

// Parses the output of the blobs command to extract the blob metadata into blobMeta format
func parseBlobsTable(text string) ([]blobMeta, error) {
	var blobs []blobMeta
	var currBlobMeta *blobMeta
	var haveIndex, haveBlobID bool

	// Flush the current blob entry to the blobs arr and reset the state for the next blob entry
	flush := func(entryNum int) error {
		if currBlobMeta == nil {
			return nil
		}

		if !haveIndex {
			return fmt.Errorf("Current blob entry #%d - missing Blob Index", entryNum)
		}

		if !haveBlobID {
			return fmt.Errorf("Current blob entry #%d - (index %d): missing Blob ID", entryNum, currBlobMeta.Index)
		}

		blobs = append(blobs, *currBlobMeta)
		return nil
	}

	// Go over each line of the blobs output...
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "Blob Index:") {

			// Flush the previous blob entry before starting this new one
			if currBlobMeta != nil {
				if err := flush(len(blobs)); err != nil {
					return nil, err
				}
			}

			// Start a new blob entry
			currBlobMeta = &blobMeta{}
			haveIndex, haveBlobID = false, false

			// Blob index parsing
			metadataValue := strings.TrimSpace(strings.TrimPrefix(trimmed, "Blob Index:"))
			metadataValue = strings.ReplaceAll(metadataValue, ",", "")
			idx, parseErr := strconv.Atoi(metadataValue)
			if parseErr != nil {
				return nil, fmt.Errorf("error occurred while trying parsing Blob Index %q: %w", metadataValue, parseErr)
			}
			currBlobMeta.Index = idx
			haveIndex = true
			continue
		}

		if currBlobMeta == nil {
			continue
		}
		// Similar but not the data we want to parse, so we skip it
		// => Need to skip it explicitly, since it appears at the expected position but is not what we want
		if strings.HasPrefix(trimmed, "Raw Blob ID:") {
			continue
		}

		// Blob ID parsing
		if !haveBlobID && strings.HasPrefix(trimmed, "Blob ID:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "Blob ID:"))
			if val == "" {
				return nil, fmt.Errorf("error occurred for blob entry (index %d): empty Blob ID value", currBlobMeta.Index)
			}
			currBlobMeta.BlobID = val
			haveBlobID = true
		}
	}

	// Flush the final entry
	if currBlobMeta != nil {
		if err := flush(len(blobs)); err != nil {
			return nil, err
		}
	}

	if len(blobs) == 0 {
		return nil, fmt.Errorf("No blob entries found, which is not expected!")
	}
	return blobs, nil
}

// Reads the chunk dump output line by line and returns one unique chunk per content hash.
func parseCheckDump(reader io.Reader, allBlobs []blobMeta, imageName string) ([]ChunkInfo, error) {
	scanner := bufio.NewScanner(reader)
	const maxLine = 1 << 20 // Increase buffer cap since some chunk lines will be longer
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	// Build blob_index - BlobID map for faster resolution of blob_index to blob digest
	blobByIndex := make(map[int]string, len(allBlobs))
	for _, b := range allBlobs {
		blobByIndex[b.Index] = b.BlobID
	}

	// State that contains the current file path and the list of unique chunks seen so far
	var (
		currentFilePath string
		chunks          []ChunkInfo
		seen            = make(map[string]struct{})
		lineNum         int
	)

	// Read the check cmd output and parse the chunk lines while keeping track of the current file path and deduplicating by content hash
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimLeft(line, " \t")

		switch {

		// File lines contain the file path => Set it
		case strings.HasPrefix(trimmed, "inode: file "):
			path, err := parseInodePath(trimmed)
			if err != nil {
				return nil, fmt.Errorf("error at line %d: %w", lineNum, err)
			}
			currentFilePath = path

		// Directory and symlink lines do not contain chunks => Clear the current file path
		case strings.HasPrefix(trimmed, "inode: dir "),
			strings.HasPrefix(trimmed, "inode: symlink "):
			currentFilePath = ""

		// Chunk lines contain the chunk metadata => Parse it and add it to the list of unique chunks if not seen before
		case strings.HasPrefix(trimmed, "chunk:"):
			chunkInfo, err := parseCheckChunkLine(trimmed, currentFilePath, blobByIndex, imageName)
			if err != nil {
				return nil, fmt.Errorf("error at line %d: %w", lineNum, err)
			}

			// Only add the chunk if it has not been seen before => by content hash
			if _, ok := seen[chunkInfo.Hash]; ok {
				continue
			}
			seen[chunkInfo.Hash] = struct{}{}
			chunks = append(chunks, chunkInfo)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error occurred while scanning the check cmd stdout: %w", err)
	}

	return chunks, nil
}

// Returns the text between the first pair of two double quotes in a given line
// Example: `inode: file "path/to/file"` => returns `path/to/file`
func parseInodePath(line string) (string, error) {
	// Find the first '"'
	start := strings.IndexByte(line, '"')
	if start < 0 {
		return "", fmt.Errorf("error at parsing an inode path: line missing opening quote: %q", line)
	}

	// Find the next '"' after the first one
	end := strings.IndexByte(line[start+1:], '"')
	if end < 0 {
		return "", fmt.Errorf("error at parsing an inode path:  line missing closing quote: %q", line)
	}
	return line[start+1 : start+1+end], nil
}

// Parses a chunk line from the nydus-image check output and returns its chunk metadata
func parseCheckChunkLine(line, filePath string, blobByIndex map[int]string, imageName string) (ChunkInfo, error) {
	// Remove the "chunk:" prefix
	body := strings.TrimSpace(strings.TrimPrefix(line, "chunk:"))

	// State for the chunk metadata fields to keep track what data we're looking for
	var (
		id                   string
		blobIndex            int
		haveID               bool
		haveBlobIndex        bool
		compressedOffset     uint64
		compressedSize       uint64
		haveCompressed       bool
		uncompressedSize     uint64
		haveUncompressedSize bool
	)

	// Split the chunk line by commas and parse each field
	for _, raw := range strings.Split(body, ",") {

		field := strings.TrimSpace(raw)
		// No fields to parse => skip
		if field == "" {
			continue
		}

		// Each field is "<key> <value>" or "<key> <a>/<b>", so we split on the first space
		firstSpace := strings.IndexByte(field, ' ')
		if firstSpace < 0 {
			continue
		}

		key := field[:firstSpace]
		value := strings.TrimSpace(field[firstSpace+1:])

		// Parse the field based on its key
		switch key {
		case "id":
			id = value
			haveID = true

		case "blob_index":
			n, err := strconv.Atoi(value)
			if err != nil {
				return ChunkInfo{}, fmt.Errorf("error while parsing blob_index %q: %w", value, err)
			}

			blobIndex = n
			haveBlobIndex = true

		case "compressed":
			offset, size, err := parseSlashOffsetSize(value)
			if err != nil {
				return ChunkInfo{}, fmt.Errorf("error while parsing compressed %q: %w", value, err)
			}

			compressedOffset = offset
			compressedSize = size
			haveCompressed = true

		case "uncompressed":
			size, err := parseSlashSize(value)
			if err != nil {
				return ChunkInfo{}, fmt.Errorf("error while parsing uncompressed %q: %w", value, err)
			}

			uncompressedSize = size
			haveUncompressedSize = true
		}
	}

	if !haveID || id == "" {
		return ChunkInfo{}, fmt.Errorf("Chunk line missing id: %q", line)
	}
	if !haveBlobIndex {
		return ChunkInfo{}, fmt.Errorf("Chunk line missing blob_index: %q", line)
	}
	if !haveCompressed {
		return ChunkInfo{}, fmt.Errorf("Chunk line missing compressed size: %q", line)
	}
	if !haveUncompressedSize {
		return ChunkInfo{}, fmt.Errorf("Chunk line missing uncompressed size: %q", line)
	}

	// Resolve the blob index to the corresponding blob digest using the blobByIndex map
	// => Needed since the chunk line only contains the blob_index, but we want to store the blob digest in the ChunkInfo
	blobID, ok := blobByIndex[blobIndex]
	if !ok {
		return ChunkInfo{}, fmt.Errorf("Chunk references blob_index %d which is not in the bootstrap blob table", blobIndex)
	}

	return ChunkInfo{
		Hash:             id,
		Size:             uncompressedSize,
		CompressedOffset: compressedOffset,
		CompressedSize:   compressedSize,
		BlobDigest:       blobID,
		Image:            imageName,
		FilePath:         filePath,
	}, nil
}

// HELPER
// Parses a string of the format "0/123" and returns 123
func parseSlashSize(s string) (uint64, error) {

	// Find the '/' character in the string
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return 0, fmt.Errorf("Expected '<offset>/<size>' format, but got %q", s)
	}

	rawSizeValue := strings.TrimSpace(s[slash+1:])
	parsedSize, err := strconv.ParseUint(rawSizeValue, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("Parsed size is a non-integer - %q: %w", rawSizeValue, err)
	}

	return parsedSize, nil
}

// Parses an offset/size pair and returns both components.
func parseSlashOffsetSize(s string) (offset uint64, size uint64, err error) {
	// Find the '/' character in the string
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return 0, 0, fmt.Errorf("Expected '<offset>/<size>' format, but got %q", s)
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
