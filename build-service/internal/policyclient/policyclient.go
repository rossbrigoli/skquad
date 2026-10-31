// Package policyclient reads an agent's live git-grant scope from the
// control plane's internal policy API (GET {cp}/internal/v1/policy?agent=).
//
// Governance rule MIG-8-R1: a build is only allowed for a repo that the
// agent's OWN git grant covers. The build service therefore treats the
// CP policy snapshot as authoritative for repo scope:
//
//	allowed(repo) = ∃ git grant g : repo ∈ effective_repos_allow(g)
//
// where effective_repos_allow uses the grant's constraints when
// present (the narrower list) and falls back to the ceiling.
//
// Posture: FAIL-CLOSED. A fetch error, a stale cache past TTL that
// cannot be refreshed, a missing agent, or a response without any git
// grant all deny. The TTL cache exists to bound CP load, not to outlive
// revocations by more than cp_policy_ttl_seconds.
package policyclient

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

	"github.com/rossbrigoli/skquad/build-service/internal/buildspec"
)

// PolicySnapshot mirrors the CP /internal/v1/policy response subset we
// consume. Unknown fields are ignored deliberately (forward-compat: the
// CP adds fields; the contract for consumers is additive).
type PolicySnapshot struct {
	AgentID        string        `json:"agent_id"`
	CredentialHash string        `json:"credential_hash"`
	Generation     int           `json:"generation"`
	Grants         []PolicyGrant `json:"grants"`
}

// PolicyGrant is one grant in the snapshot.
type PolicyGrant struct {
	ResourceID   string          `json:"resource_id"`
	ResourceType string          `json:"resource_type"`
	Constraints  json.RawMessage `json:"constraints,omitempty"`
	Ceiling      json.RawMessage `json:"ceiling,omitempty"`
}

// Client fetches and caches policy snapshots.
type Client struct {
	baseURL string
	ttl     time.Duration
	http    *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	snap      *PolicySnapshot
	expiresAt time.Time
}

// New builds a client. baseURL is the CP root (e.g.
// http://skquad-api.skquad-system.svc.cluster.local). timeout bounds
// each fetch.
func New(baseURL string, ttl, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		ttl:     ttl,
		http:    &http.Client{Timeout: timeout},
		cache:   map[string]cacheEntry{},
	}
}

// ErrDeniedScope is returned when the agent's policy does not cover the
// requested repo (or cannot be consulted).
type ErrDeniedScope struct{ Reason string }

func (e *ErrDeniedScope) Error() string { return "git grant scope does not cover repo: " + e.Reason }

// RepoAllowed consults (cached) policy for agentID and reports whether
// orgRepo is covered. Denials carry a reason for the audit log; the
// agent-facing response never leaks other agents' policy details.
func (c *Client) RepoAllowed(ctx context.Context, agentID, orgRepo string) error {
	snap, err := c.get(ctx, agentID)
	if err != nil {
		return &ErrDeniedScope{Reason: "policy unavailable: " + err.Error()}
	}
	if snap.AgentID != agentID {
		return &ErrDeniedScope{Reason: "policy agent_id mismatch"}
	}
	var matched string
	for _, g := range snap.Grants {
		if g.ResourceType != "git" {
			continue
		}
		list := effectiveReposAllow(g)
		if len(list) == 0 {
			continue
		}
		if buildspec.RepoAllowed(list, orgRepo) {
			matched = g.ResourceID
			break
		}
	}
	if matched == "" {
		return &ErrDeniedScope{Reason: "no git grant covers " + orgRepo}
	}
	return nil
}

// effectiveReposAllow returns the grant's constraints.repos_allow when
// present (narrower), else ceiling.repos_allow.
func effectiveReposAllow(g PolicyGrant) []string {
	if l := reposAllowOf(g.Constraints); len(l) > 0 {
		return l
	}
	return reposAllowOf(g.Ceiling)
}

func reposAllowOf(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	v, ok := m["repos_allow"]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (c *Client) get(ctx context.Context, agentID string) (*PolicySnapshot, error) {
	c.mu.Lock()
	if e, ok := c.cache[agentID]; ok && time.Now().Before(e.expiresAt) {
		c.mu.Unlock()
		return e.snap, nil
	}
	c.mu.Unlock()

	snap, err := c.fetch(ctx, agentID)
	if err != nil {
		// Fail closed: serve nothing stale.
		return nil, err
	}
	c.mu.Lock()
	c.cache[agentID] = cacheEntry{snap: snap, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return snap, nil
}

func (c *Client) fetch(ctx context.Context, agentID string) (*PolicySnapshot, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("bad base url: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/internal/v1/policy"
	q := u.Query()
	q.Set("agent", agentID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("policy fetch: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("unknown agent (404)")
	default:
		return nil, fmt.Errorf("policy fetch status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("policy read: %w", err)
	}
	var snap PolicySnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, fmt.Errorf("policy parse: %w", err)
	}
	return &snap, nil
}
