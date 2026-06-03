// Package cacheclient provides an HTTP client for storing and retrieving chunk data from a remote cache service.
package cacheclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client sends GET and PUT requests to a remote chunk cache service.
type Client struct {
	base string
	http *http.Client
}

// New creates a Client that targets the given base URL.
// A trailing slash in base is trimmed automatically.
func New(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{}}
}

func (c *Client) url(digest string) string {
	return c.base + "/chunks/" + digest
}

// Get fetches the chunk identified by digest from the cache.
// A 200 response returns the body (caller must close it) with hit=true.
// A 404 response is a clean miss: all return values are nil/false/nil.
// Any transport error or unexpected status code is returned as a non-nil error.
func (c *Client) Get(ctx context.Context, digest string) (io.ReadCloser, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(digest), nil)
	if err != nil {
		return nil, false, fmt.Errorf("build get request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("cache get %s: %w", digest, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, true, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, false, nil
	default:
		resp.Body.Close()
		return nil, false, fmt.Errorf("cache get %s: unexpected status %d", digest, resp.StatusCode)
	}
}

// Put uploads the content of body to the cache under the given digest.
// The cache service must respond with 204 No Content; any other status is an error.
func (c *Client) Put(ctx context.Context, digest string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(digest), body)
	if err != nil {
		return fmt.Errorf("build put request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cache put %s: %w", digest, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cache put %s: unexpected status %d", digest, resp.StatusCode)
	}

	return nil
}
