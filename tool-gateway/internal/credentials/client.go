// Package credentials implements the gateway-side client for the control
// plane's internal credentials read API (TG-4, design §5.2/§6.2):
//
//	GET {CP}/internal/v1/credentials?resource=<id>
//
// The gateway resolves BYO credential material PER CALL and never
// persists it: the only allowed retention is the driver's process-memory
// OAuth token cache. Secret values must never be logged, audited, or
// echoed. Like the policy client, this is FAIL-CLOSED: any resolution
// failure denies the call (ErrUnavailable) — the driver never proceeds
// with missing or partial credentials.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrUnavailable means the secret for a resource could not be resolved
// (CP unreachable, unknown resource, missing auth_ref, unreadable
// secret). Callers must DENY the call.
var ErrUnavailable = errors.New("credentials_unavailable")

// Secret is the resolved credential material for one resource. Fields
// are kind-specific (see control-plane TG-4 contract):
//
//	bearer                     -> {"token": "..."}
//	api_key_header             -> {"token": "..."}   (header name from config)
//	basic                      -> {"username": "...", "password": "..."}
//	oauth2_client_credentials  -> {"client_id": "...", "client_secret": "...", "token_url": "..."}
//
// Values are sensitive: never log, never audit, never embed in errors.
type Secret struct {
	ResourceID string            `json:"resource_id"`
	Kind       string            `json:"kind"`
	Fields     map[string]string `json:"fields"`
}

// Resolver resolves the credential secret for a resource id.
type Resolver interface {
	Resolve(ctx context.Context, resourceID string) (*Secret, error)
}

// Client is an HTTP resolver against the CP internal credentials API.
// It performs NO caching: every call hits the CP so revocation is
// immediate (the OAuth token cache in the driver is the only exception,
// bounded by token expiry).
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a credentials client. timeout bounds each CP call.
func NewClient(cpBaseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: cpBaseURL,
		http:    &http.Client{Timeout: timeout},
	}
}

// Resolve fetches the secret material for resourceID. Any non-200 or
// malformed response is ErrUnavailable (fail-closed). Error strings
// never contain secret material.
func (c *Client) Resolve(ctx context.Context, resourceID string) (*Secret, error) {
	u, err := url.Parse(c.baseURL + "/internal/v1/credentials")
	if err != nil {
		return nil, fmt.Errorf("%w: bad CP base URL", ErrUnavailable)
	}
	q := u.Query()
	q.Set("resource", resourceID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: control plane unreachable", ErrUnavailable)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
		if err != nil {
			return nil, fmt.Errorf("%w: reading response", ErrUnavailable)
		}
		var s Secret
		if err := json.Unmarshal(body, &s); err != nil {
			return nil, fmt.Errorf("%w: decoding response", ErrUnavailable)
		}
		if s.Kind == "" {
			return nil, fmt.Errorf("%w: missing kind", ErrUnavailable)
		}
		return &s, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: resource has no resolvable credential", ErrUnavailable)
	default:
		return nil, fmt.Errorf("%w: CP returned status %d", ErrUnavailable, resp.StatusCode)
	}
}

// compile-time interface check
var _ Resolver = (*Client)(nil)
