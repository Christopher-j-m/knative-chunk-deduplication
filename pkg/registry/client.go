// Package registry provides a minimal HTTP client for fetching OCI blobs from a container registry.
package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Client fetches blob data from an OCI-compliant container registry over HTTP.
type Client struct {
	base       string
	httpClient *http.Client
}

// New creates a Client that targets the given registry base URL.
// A trailing slash is trimmed from base so callers can pass either form.
func New(base string) *Client {
	return &Client{
		base:       strings.TrimRight(base, "/"),
		httpClient: &http.Client{},
	}
}

// blobReference turns a blob identifier into the form the registry API expects.
// Nydus names blobs by their bare hex digest, while the registry wants the
// algorithm prefix, so a 64-character hex string becomes sha256:<id>.
func blobReference(blob string) string {
	if len(blob) != 64 || strings.Contains(blob, ":") {
		return blob
	}
	for _, character := range blob {
		isHexDigit := (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')
		if !isHexDigit {
			return blob
		}
	}
	return "sha256:" + blob
}

// GetBlobRange fetches the inclusive byte range [start, end] of a blob from the registry.
// A registry that honours the Range header replies 206 and the body contains exactly the
// requested bytes, which are returned as-is. A registry that ignores Range replies 200 with
// the whole blob; in that case the body is read in full, closed, and sliced down to the
// requested range before returning, so the caller always receives only the bytes it asked for.
func (c *Client) GetBlobRange(ctx context.Context, repo, blob string, start, end uint64) (io.ReadCloser, error) {
	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", c.base, repo, blobReference(blob))

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, fmt.Errorf("registry get %s: build request: %w", blob, err)
	}

	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("registry get %s: %w", blob, err)
	}

	switch response.StatusCode {
	case http.StatusPartialContent:
		// The registry honoured the Range header — the body contains exactly the requested bytes
		return response.Body, nil

	case http.StatusOK:
		// The registry ignored the Range header and returned the whole blob — slice it down
		// Buffer the whole blob only because this registry ignored Range; the 206 path streams instead
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("registry get %s: read body: %w", blob, err)
		}

		if start >= uint64(len(data)) {
			return nil, fmt.Errorf("registry get %s: range start %d past end of %d-byte blob", blob, start, len(data))
		}

		stop := end + 1
		if stop > uint64(len(data)) {
			stop = uint64(len(data))
		}

		return io.NopCloser(bytes.NewReader(data[start:stop])), nil

	default:
		response.Body.Close()
		return nil, fmt.Errorf("registry get %s: unexpected status %d", blob, response.StatusCode)
	}
}

// HeadBlob returns the total size of a blob in bytes by issuing a HEAD request to the registry.
// The Content-Length header from the response is used to determine the size.
func (c *Client) HeadBlob(ctx context.Context, repo, blob string) (uint64, error) {
	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", c.base, repo, blobReference(blob))

	request, err := http.NewRequestWithContext(ctx, http.MethodHead, blobURL, nil)
	if err != nil {
		return 0, fmt.Errorf("registry head %s: build request: %w", blob, err)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("registry head %s: %w", blob, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("registry head %s: unexpected status %d", blob, response.StatusCode)
	}

	contentLength := response.Header.Get("Content-Length")
	if contentLength == "" {
		return 0, fmt.Errorf("registry head %s: missing Content-Length header", blob)
	}

	size, err := strconv.ParseUint(contentLength, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("registry head %s: parse Content-Length %q: %w", blob, contentLength, err)
	}

	return size, nil
}
