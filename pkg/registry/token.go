package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Cache every token for each registry repo for which we already requested a token
// => Only the first request for a registry needs the extra api call round trip
type tokenCache struct {
	mutex  sync.Mutex
	tokens map[string]string
}

// Returns the cached token for an registry repo, or empty string if not cached
func (cache *tokenCache) get(repo string) string {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	return cache.tokens[repo]
}

// Store a new token (or updated one) to the token cache
func (cache *tokenCache) set(repo, token string) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if cache.tokens == nil {
		cache.tokens = map[string]string{}
	}
	cache.tokens[repo] = token
}

func (registryClient *Client) do(request *http.Request, repo string) (*http.Response, error) {
	// Use an already cached token for this registry repo if we already have one
	if token := registryClient.tokens.get(repo); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	// Return response from registry if the used token was accepted or we dont need one at all
	response, err := registryClient.httpClient.Do(request)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}

	// Registry rejected our token or we had none, so we need get a (new) one
	challenge := response.Header.Get("Www-Authenticate")
	response.Body.Close()

	token, err := registryClient.fetchAnonymousToken(request.Context(), challenge)
	if err != nil {
		return nil, fmt.Errorf("error while trying to get a registry repo token for %s: %w", repo, err)
	}

	// Cache the newly retrieved token
	registryClient.tokens.set(repo, token)

	// Create a clean new request with the new token, since the old one was already sent to the registry
	retry := request.Clone(request.Context())
	retry.Header.Set("Authorization", "Bearer "+token)
	return registryClient.httpClient.Do(retry)
}

// Fetches & returns a new token for a specific registry repo
// => Called when receiving a 401 before
func (registryClient *Client) fetchAnonymousToken(ctx context.Context, challenge string) (string, error) {
	// Get the endpoint for fetching a new token
	realm, parameters, err := parseBearerChallenge(challenge)
	if err != nil {
		return "", err
	}

	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("error at parsing challenge realm %q: %w", realm, err)
	}

	// Create the query string of the token url with the parsed token challenge
	query := tokenURL.Query()
	for _, name := range []string{"service", "scope"} {
		if value := parameters[name]; value != "" {
			query.Set(name, value)
		}
	}
	tokenURL.RawQuery = query.Encode()

	// Build & Send the request
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("error while building the token request: %w", err)
	}

	response, err := registryClient.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status received after token request: %d", response.StatusCode)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("error while decoding the token response: %w", err)
	}

	// We received the token, but they may be named differently by different registries
	if payload.AccessToken != "" {
		return payload.AccessToken, nil
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	return "", fmt.Errorf("token endpoint returned no token")
}

// Parse the token endpoint from the www-authenticate header in a 401 response
func parseBearerChallenge(challenge string) (string, map[string]string, error) {
	// We only support bearer auth challenge, since this is the only one we needed so far
	const prefix = "Bearer "
	if !strings.HasPrefix(challenge, prefix) {
		return "", nil, fmt.Errorf("unsupported auth challenge %q, only bearer supported", challenge)
	}

	// Split everything after the prefix ("Bearer "), e.g. realm="https://host/token",service="host"
	// => Result would be
	// map[string]string{
	// "realm":   "https://host/token",
	// "service": "host",
	// }
	parameters := map[string]string{}
	for _, part := range strings.Split(challenge[len(prefix):], ",") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		parameters[name] = strings.Trim(value, `"`)
	}

	realm := parameters["realm"]
	if realm == "" {
		return "", nil, fmt.Errorf("auth challenge %q carries no realm", challenge)
	}
	return realm, parameters, nil
}
