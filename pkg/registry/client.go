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

// Client for fetching blobs from a remote (OCI) container registry
type Client struct {
	base       string
	httpClient *http.Client
	tokens     tokenCache
}

func New(base string) *Client {
	return &Client{
		base:       strings.TrimRight(base, "/"), // url (root) of the target registry
		httpClient: &http.Client{},               // http client to make the registry requests
	}
}

// Returns a blob identifier into the format that the registry understands
// Nydus uses the plain digest for each blob, but our registry expects the hash alg. prefix as well
// e.g. returns sha256:<digest>
func blobReference(blob string) string {
	// Already in correct format
	if len(blob) != 64 || strings.Contains(blob, ":") {
		return blob
	}

	// Validate every character of the blob identifier to be either a lowercase char or number
	for _, character := range blob {
		isHexDigit := (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')
		if !isHexDigit {
			return blob
		}
	}
	return "sha256:" + blob
}

// Fetches the bytes from a start and end byte (inclusively) from the (OCI) registry
// and return the corresponding stream that can be read by the caller
func (registryClient *Client) GetBlobRange(ctx context.Context, repo, blob string, start, end uint64) (io.ReadCloser, error) {
	// OCI format for fetching a blob range
	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", registryClient.base, repo, blobReference(blob))

	// Build the range request
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error at registry get %s: build request: %w", blob, err)
	}

	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	// Send the request to registry
	response, err := registryClient.do(request, repo)
	if err != nil {
		return nil, fmt.Errorf("error from registry get %s: %w", blob, err)
	}

	switch response.StatusCode {
	// The registry returned the requested blob range
	case http.StatusPartialContent:
		return response.Body, nil

	// The registry just returned the whole blob and we need to create the range ourself
	case http.StatusOK:
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("error from registry get %s: reading body: %w", blob, err)
		}

		// Our range starts lies beyond the blob end
		if start >= uint64(len(data)) {
			return nil, fmt.Errorf("error from registry get %s: range start %d lies past end of %d-byte blob", blob, start, len(data))
		}

		// Get the correct slice out of the whole blob
		stop := end + 1
		if stop > uint64(len(data)) {
			stop = uint64(len(data))
		}

		return io.NopCloser(bytes.NewReader(data[start:stop])), nil

	default:
		response.Body.Close()
		return nil, fmt.Errorf("error from registry get %s: unexpected status %d", blob, response.StatusCode)
	}
}

// Return the total blob size by sending a head request to the corresponding registry
func (registryClient *Client) HeadBlob(ctx context.Context, repo, blob string) (uint64, error) {
	// Again, OCI registry routes are uniform
	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", registryClient.base, repo, blobReference(blob))

	// Create new request
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, blobURL, nil)
	if err != nil {
		return 0, fmt.Errorf("error from registry head %s: build request: %w", blob, err)
	}

	response, err := registryClient.do(request, repo)
	if err != nil {
		return 0, fmt.Errorf("error from registry head %s: %w", blob, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("error from registry head %s: unexpected status %d", blob, response.StatusCode)
	}

	// Check if response contains the relevant header field
	contentLength := response.Header.Get("Content-Length")
	if contentLength == "" {
		return 0, fmt.Errorf("error from registry head %s: expected content-length header, but got none", blob)
	}

	size, err := strconv.ParseUint(contentLength, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("error from registry head %s: couldn't parse content-length header %q: %w", blob, contentLength, err)
	}

	return size, nil
}
