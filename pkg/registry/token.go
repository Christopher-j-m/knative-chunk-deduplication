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

// tokenCache remembers the bearer token a registry handed out for each repo,
// so only the first request (and the first after an expiry) pays the extra
// round trip to the token endpoint.
type tokenCache struct {
	mu     sync.Mutex
	tokens map[string]string
}

func (cache *tokenCache) get(repo string) string {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.tokens[repo]
}

func (cache *tokenCache) set(repo, token string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.tokens == nil {
		cache.tokens = map[string]string{}
	}
	cache.tokens[repo] = token
}

// do sends the request, transparently satisfying a bearer challenge the way
// registries with anonymous pull (such as ACR) require: on a 401 it asks the
// challenge's token endpoint for a token without credentials, retries the
// request once with that token, and caches it for later requests to the repo.
func (c *Client) do(request *http.Request, repo string) (*http.Response, error) {
	if token := c.tokens.get(repo); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}

	challenge := response.Header.Get("Www-Authenticate")
	response.Body.Close()

	token, err := c.fetchAnonymousToken(request.Context(), challenge)
	if err != nil {
		return nil, fmt.Errorf("anonymous token for %s: %w", repo, err)
	}
	c.tokens.set(repo, token)

	retry := request.Clone(request.Context())
	retry.Header.Set("Authorization", "Bearer "+token)
	return c.httpClient.Do(retry)
}

// fetchAnonymousToken asks the token endpoint named by a bearer challenge for
// a pull token, sending no credentials. Both spellings of the token field are
// accepted because ACR answers with access_token and the reference registry
// implementation with token.
func (c *Client) fetchAnonymousToken(ctx context.Context, challenge string) (string, error) {
	realm, parameters, err := parseBearerChallenge(challenge)
	if err != nil {
		return "", err
	}

	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse challenge realm %q: %w", realm, err)
	}
	query := tokenURL.Query()
	for _, name := range []string{"service", "scope"} {
		if value := parameters[name]; value != "" {
			query.Set(name, value)
		}
	}
	tokenURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint: unexpected status %d", response.StatusCode)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}

	if payload.AccessToken != "" {
		return payload.AccessToken, nil
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	return "", fmt.Errorf("token endpoint returned no token")
}

// parseBearerChallenge extracts the realm and the remaining parameters from a
// WWW-Authenticate bearer challenge such as
// Bearer realm="https://host/oauth2/token",service="host",scope="repository:x:pull"
func parseBearerChallenge(challenge string) (string, map[string]string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(challenge, prefix) {
		return "", nil, fmt.Errorf("unsupported auth challenge %q", challenge)
	}

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
