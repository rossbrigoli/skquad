// Package cppolicy is a minimal TTL-cached, fail-closed client for the
// control plane's internal policy read API:
//
//	GET {CP}/internal/v1/policy?agent=<id>
//
// The build service uses it for ONE purpose: verifying that a requested
// git repo falls inside the agent's effective GIT grant scope
// (ceiling ∧ grant constraints, same precedence as the tool-gateway git
// driver: the grant's repos_allow wins when present because the CP has
// already proven it a subset of the ceiling; empty/unset = default-deny).
//
// Posture mirrors tool-gateway/internal/policy: an expired entry plus a
// fetch error is ErrPolicyUnavailable — never a stale allow, never a
// default allow.
package cppolicy

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

// ErrPolicyUnavailable means the agent's policy could not be resolved.
// Callers must DENY.
var ErrPolicyUnavailable = errors.New("policy_unavailable")

// Grant mirrors the CP policy wire shape (subset relevant to builds).
type Grant struct {
	ResourceID   string          `json:"resource_id"`
	ResourceType string          `json:"resource_type"`
	Constraints  json.RawMessage `json:"constraints,omitempty"`
	Ceiling      json.RawMessage `json:"ceiling,omitempty"`
}

type snapshot struct {
	AgentID string  `json:"agent_id"`
	Grants  []Grant `json:"grants"`

	cachedAt time.Time
}

// Lookup resolves the effective git repos_allow globs for an agent.
type Lookup interface {
	GitReposAllow(ctx context.Context, agentID string) ([]string, error)
}

// Client is a TTL cache over the CP policy endpoint.
type Client struct {
	baseURL string
	ttl     time.Duration
	http    *http.Client

	mu      sync.Mutex
	entries map[string]*snapshot
}

// NewClient builds the policy client.
func NewClient(baseURL string, ttl, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		ttl:     ttl,
		http:    &http.Client{Timeout: timeout},
		entries: map[string]*snapshot{},
	}
}

// GitReposAllow returns the effective git repos_allow globs for agentID:
// the union over all git-type grants of (constraints.repos_allow if set,
// else ceiling.repos_allow). Default-deny: no git grants or empty lists
// yield an empty slice (caller denies).
func (c *Client) GitReposAllow(ctx context.Context, agentID string) ([]string, error) {
	snap, err := c.get(ctx, agentID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, g := range snap.Grants {
		if g.ResourceType != "git" {
			continue
		}
		var layer struct {
			ReposAllow *[]string `json:"repos_allow"`
		}
		// Ceiling is the fallback layer.
		if len(g.Ceiling) > 0 {
			layer.ReposAllow = nil
			if err := json.Unmarshal(g.Ceiling, &layer); err != nil {
				return nil, fmt.Errorf("parse git ceiling for grant %s: %w", g.ResourceID, err)
			}
		}
		effective := layer.ReposAllow
		// Constraints (grant narrowing) wins when present, mirroring
		// tool-gateway git policy.EffectivePolicy.
		if len(g.Constraints) > 0 {
			var cons struct {
				ReposAllow *[]string `json:"repos_allow"`
			}
			if err := json.Unmarshal(g.Constraints, &cons); err != nil {
				return nil, fmt.Errorf("parse git constraints for grant %s: %w", g.ResourceID, err)
			}
			if cons.ReposAllow != nil {
				effective = cons.ReposAllow
			}
		}
		if effective != nil {
			out = append(out, *effective...)
		}
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, agentID string) (*snapshot, error) {
	c.mu.Lock()
	if s, ok := c.entries[agentID]; ok && time.Since(s.cachedAt) < c.ttl {
		c.mu.Unlock()
		return s, nil
	}
	c.mu.Unlock()

	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, ErrPolicyUnavailable
	}
	u.Path = u.Path + "/internal/v1/policy"
	q := u.Query()
	q.Set("agent", agentID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrPolicyUnavailable
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrPolicyUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain so the connection can be reused; content is not logged.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, ErrPolicyUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, ErrPolicyUnavailable
	}
	var snap snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, ErrPolicyUnavailable
	}
	if snap.AgentID != agentID {
		// CP answered for a different agent — fail closed.
		return nil, ErrPolicyUnavailable
	}
	snap.cachedAt = time.Now()
	c.mu.Lock()
	c.entries[agentID] = &snap
	c.mu.Unlock()
	return &snap, nil
}
