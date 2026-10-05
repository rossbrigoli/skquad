// Package policy implements the gateway-side cache client for the control
// plane's internal policy read API (design §5.1.5 / §5.2):
//
//	GET {CP}/internal/v1/policy?agent=<id>   (ETag / If-None-Match)
//
// The cache is FAIL-CLOSED: an entry is served only while it is within TTL;
// once expired, a fetch failure denies (ErrPolicyUnavailable) — the gateway
// never serves a stale grant set and never defaults to allow.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ErrPolicyUnavailable means the policy for an agent could not be resolved
// (CP unreachable/error and no fresh cache entry). Callers must DENY.
var ErrPolicyUnavailable = errors.New("policy_unavailable")

// Grant is a single effective grant surfaced by the CP policy API
// (TG-2 wire shape: control-plane/internal/httpapi policyGrant). Typed
// drivers (web/rest/mcp/git) enforce on Config (resource floor),
// Ceiling and Constraints.
type Grant struct {
	ResourceID   string          `json:"resource_id"`
	ResourceType string          `json:"resource_type"`
	Config       json.RawMessage `json:"config,omitempty"`
	Constraints  json.RawMessage `json:"constraints,omitempty"`
	Ceiling      json.RawMessage `json:"ceiling,omitempty"`
	RiskTier     string          `json:"risk_tier,omitempty"`
	EgressClass  string          `json:"egress_class,omitempty"`
}

// Snapshot is the CP's answer for one agent: identity material needed to
// verify the bearer credential plus the effective grant set.
// CredentialHash is base64(std sha256(token)) — the same encoding the
// control plane uses (httpapi.hashCredential). The raw credential never
// leaves the CP.
type Snapshot struct {
	AgentID        string    `json:"agent_id"`
	CredentialHash string    `json:"credential_hash"`
	Grants         []Grant   `json:"grants"`
	FetchedAt      time.Time `json:"-"`
}

// Lookup returns the fresh-or-fetched snapshot for an agent, or
// ErrPolicyUnavailable when it must be denied.
type Lookup interface {
	Get(ctx context.Context, agentID string) (*Snapshot, error)
}

type entry struct {
	snapshot *Snapshot
	etag     string
	fetched  time.Time
}

// Client is a TTL+ETag cache over the CP policy endpoint. It is safe for
// concurrent use.
type Client struct {
	baseURL string
	ttl     time.Duration
	http    *http.Client

	mu        sync.Mutex
	entries   map[string]*entry
	reachable bool // any non-transport-error response since last failure
}

// NewClient builds a policy cache client. timeout bounds each CP call.
func NewClient(cpBaseURL string, ttl, timeout time.Duration) *Client {
	return &Client{
		baseURL: cpBaseURL,
		ttl:     ttl,
		http:    &http.Client{Timeout: timeout},
		entries: make(map[string]*entry),
	}
}

// Get resolves a snapshot for agentID. Fresh cache entries are served
// without a network call; stale/missing entries trigger a fetch. On fetch
// failure the result is always ErrPolicyUnavailable (fail-closed).
func (c *Client) Get(ctx context.Context, agentID string) (*Snapshot, error) {
	c.mu.Lock()
	e := c.entries[agentID]
	c.mu.Unlock()
	if e != nil && time.Since(e.fetched) < c.ttl {
		return e.snapshot, nil
	}
	return c.fetch(ctx, agentID, e)
}

func (c *Client) fetch(ctx context.Context, agentID string, cached *entry) (*Snapshot, error) {
	u, err := url.Parse(c.baseURL + "/internal/v1/policy")
	if err != nil {
		return nil, fmt.Errorf("%w: bad CP base URL: %v", ErrPolicyUnavailable, err)
	}
	q := u.Query()
	q.Set("agent", agentID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPolicyUnavailable, err)
	}
	if cached != nil && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.setReachable(false)
		// Fail-closed: expired/missing entry + CP error => deny. Never serve
		// stale grants, never allow.
		return nil, fmt.Errorf("%w: control plane unreachable: %v", ErrPolicyUnavailable, err)
	}
	defer resp.Body.Close()
	c.setReachable(true)

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
		if err != nil {
			return nil, fmt.Errorf("%w: reading policy: %v", ErrPolicyUnavailable, err)
		}
		var snap Snapshot
		if err := json.Unmarshal(body, &snap); err != nil {
			return nil, fmt.Errorf("%w: decoding policy: %v", ErrPolicyUnavailable, err)
		}
		if snap.AgentID == "" {
			return nil, fmt.Errorf("%w: policy missing agent_id", ErrPolicyUnavailable)
		}
		snap.FetchedAt = time.Now()
		c.mu.Lock()
		c.entries[agentID] = &entry{snapshot: &snap, etag: resp.Header.Get("ETag"), fetched: snap.FetchedAt}
		c.mu.Unlock()
		return &snap, nil
	case http.StatusNotModified:
		if cached == nil || cached.snapshot == nil {
			return nil, fmt.Errorf("%w: 304 without cached entry", ErrPolicyUnavailable)
		}
		// Refresh the TTL on the cached entry; content is unchanged.
		c.mu.Lock()
		c.entries[agentID] = &entry{snapshot: cached.snapshot, etag: cached.etag, fetched: time.Now()}
		c.mu.Unlock()
		return cached.snapshot, nil
	default:
		return nil, fmt.Errorf("%w: CP returned status %d", ErrPolicyUnavailable, resp.StatusCode)
	}
}

// Reachable reports whether the CP answered the last attempted call at all
// (any HTTP status). Used only for the readiness gate; it never widens a
// policy decision.
func (c *Client) Reachable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reachable
}

func (c *Client) setReachable(v bool) {
	c.mu.Lock()
	c.reachable = v
	c.mu.Unlock()
}

// Probe performs a best-effort policy fetch whose result is discarded; it
// exists to keep the reachability signal fresh between agent requests.
func (c *Client) Probe(ctx context.Context, agentID string) {
	_, _ = c.Get(ctx, agentID)
}
