package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/credentials"
)

// oauthSkew trims the cached token lifetime so a token is refreshed
// before it actually expires (clock-drift safety margin).
const oauthSkew = 60 * time.Second

const oauthDefaultExpiry = 300 * time.Second

type cachedToken struct {
	access    string
	expiresAt time.Time
}

// oauthCache caches OAuth2 client-credentials access tokens per
// resource. This is the ONLY place the gateway may retain secret-derived
// material between calls (design §6.2: "cached token + refresh-on-401")
// and it is strictly process-memory: nothing is persisted, nothing is
// logged. The access token itself is secret and never leaves this
// package except in the Authorization header of the governed call.
type oauthCache struct {
	mu     sync.Mutex
	tokens map[string]*cachedToken
}

func newOAuthCache() *oauthCache {
	return &oauthCache{tokens: make(map[string]*cachedToken)}
}

// tokenFetchFunc mints an access token from the IdP. Injected so the
// driver can route it through the same egress guard as the API call,
// and tests can stub the IdP without a network.
type tokenFetchFunc func(ctx context.Context, tokenURL, clientID, clientSecret string) (string, time.Duration, error)

// token returns a valid access token for the resource, minting one when
// absent or near expiry. force bypasses the cache (refresh-on-401).
func (c *oauthCache) token(ctx context.Context, resourceID string, secret *credentials.Secret, force bool, fetch tokenFetchFunc) (string, error) {
	if secret == nil || secret.Kind != AuthOAuth2ClientCreds {
		return "", fmt.Errorf("not an oauth2 client-credentials resource")
	}
	clientID := secret.Fields["client_id"]
	clientSecret := secret.Fields["client_secret"]
	tokenURL := secret.Fields["token_url"]
	if clientID == "" || clientSecret == "" || tokenURL == "" {
		return "", fmt.Errorf("oauth2 secret missing client_id/client_secret/token_url")
	}

	now := time.Now()
	if !force {
		c.mu.Lock()
		t := c.tokens[resourceID]
		c.mu.Unlock()
		if t != nil && now.Before(t.expiresAt) {
			return t.access, nil
		}
	}

	access, ttl, err := fetch(ctx, tokenURL, clientID, clientSecret)
	if err != nil || access == "" {
		return "", fmt.Errorf("oauth2 token fetch failed")
	}
	if ttl <= 0 {
		ttl = oauthDefaultExpiry
	}
	skew := oauthSkew
	if ttl <= skew*2 {
		// Very short-lived tokens: shrink the margin so the cached
		// lifetime stays positive (refresh still happens before expiry).
		skew = ttl / 3
	}
	c.mu.Lock()
	c.tokens[resourceID] = &cachedToken{access: access, expiresAt: now.Add(ttl - skew)}
	c.mu.Unlock()
	return access, nil
}

// invalidate drops the cached token for a resource (refresh-on-401).
func (c *oauthCache) invalidate(resourceID string) {
	c.mu.Lock()
	delete(c.tokens, resourceID)
	c.mu.Unlock()
}

// newDefaultTokenFetcher builds the RFC 6749 client_credentials fetcher
// over the given (egress-guarded) HTTP client. Error strings never
// carry the secret or the response body.
func newDefaultTokenFetcher(client *http.Client) tokenFetchFunc {
	return func(ctx context.Context, tokenURL, clientID, clientSecret string) (string, time.Duration, error) {
		form := url.Values{}
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", 0, fmt.Errorf("oauth2: build token request")
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return "", 0, fmt.Errorf("oauth2: token endpoint unreachable")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", 0, fmt.Errorf("oauth2: token endpoint status %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return "", 0, fmt.Errorf("oauth2: reading token response")
		}
		var out struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int64  `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
			return "", 0, fmt.Errorf("oauth2: malformed token response")
		}
		return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
	}
}
