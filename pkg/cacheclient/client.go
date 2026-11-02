// Provides an HTTP client for storing and retrieving chunk data from a remote cache service (used by the chunk proxy on the cluster nodes)
package cacheclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// This client sends Get and Put requests to the remote cluster cache
type Client struct {
	baseUrl string
	http    *http.Client
}

func New(baseUrl string) *Client {
	return &Client{baseUrl: strings.TrimRight(baseUrl, "/"), http: &http.Client{}}
}

func (client *Client) url(digest string) string {
	return client.baseUrl + "/chunks/" + digest
}

// Fetches the chunk with the given digest from the cache
// Possible responses:
// 200: cache hit => returns the body (we must close it at this client) with hit=true.
// 404: cache miss
func (client *Client) Get(ctx context.Context, digest string) (io.ReadCloser, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.url(digest), nil)
	if err != nil {
		return nil, false, fmt.Errorf("error while building the get request: %w", err)
	}

	resp, err := client.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("error while sending a cache get %s: %w", digest, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, true, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, false, nil
	default:
		resp.Body.Close()
		return nil, false, fmt.Errorf("error received for cache get %s: status %d", digest, resp.StatusCode)
	}
}

// Send a new chunk to the cache (digest, payload)
// Everything except 204 indicates that something went wrong
func (client *Client) Put(ctx context.Context, digest string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, client.url(digest), body)
	if err != nil {
		return fmt.Errorf("error building put request: %w", err)
	}

	resp, err := client.http.Do(req)
	if err != nil {
		return fmt.Errorf("error during cache put %s: %w", digest, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("error received for cache put %s: status %d", digest, resp.StatusCode)
	}

	return nil
}
