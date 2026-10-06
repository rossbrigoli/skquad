package browser

// Effective policy folding for the TG-6 `browser` driver
// (docs/tg6-browser-protocol.md §2/§6, docs/tool-gateway.md §6.4).
// Mirrors the mcp/rest/git layering: effective = config ∧ ceiling ∧
// constraints; a malformed layer never widens reach — it denies.
//
// Wire shapes (CP slice owns registration):
//
//	config:      {base_url|url: str, driver: "browser"}
//	ceiling:     {deny_hosts: [glob], max_pages: int,
//	            max_screenshot_bytes: int, idle_timeout_s: int,
//	            max_session_minutes: int, max_sessions_per_agent: int}
//	constraints: same shape as the ceiling (grant narrowing only)
//
// Numeric bounds fold to the MIN of every layer that sets one
// (tightening only). deny_hosts is a UNION (a grant may add denials but
// never remove them). Defaults follow the protocol doc:
// max_screenshot_bytes 2 MiB, idle_timeout_s 600, max_session_minutes
// 30, max_sessions_per_agent 1, max_pages 50.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Default caps from docs/tg6-browser-protocol.md §2/§6.
const (
	DefaultMaxScreenshotBytes  = 2 * 1024 * 1024 // 2 MiB
	DefaultIdleTimeoutS        = 600
	DefaultMaxSessionMinutes   = 30
	DefaultMaxSessionsPerAgent = 1
	DefaultMaxPages            = 50
)

type browserConfigShape struct {
	BaseURL string `json:"base_url"`
	URL     string `json:"url"`
	Driver  string `json:"driver"`
}

// browserPolicyShape is the shared JSON shape of the ceiling AND the
// grant constraints. Pointer fields distinguish unset (inherit) from an
// explicit value so defaults fold correctly. max_sessions_per_agent is
// a *int because 0 is a meaningful (deny-everything) explicit value.
type browserPolicyShape struct {
	DenyHosts           []string `json:"deny_hosts"`
	MaxPages            int      `json:"max_pages"`
	MaxScreenshotBytes  int      `json:"max_screenshot_bytes"`
	IdleTimeoutS        int      `json:"idle_timeout_s"`
	MaxSessionMinutes   int      `json:"max_session_minutes"`
	MaxSessionsPerAgent *int     `json:"max_sessions_per_agent"`
}

// Policy is the effective browser policy for a grant.
type Policy struct {
	ServiceURL          string
	DenyHosts           []string
	MaxPages            int
	MaxScreenshotBytes  int
	IdleTimeoutS        int
	MaxSessionMinutes   int
	MaxSessionsPerAgent int
}

// EffectivePolicy folds config ∧ ceiling ∧ constraints.
func EffectivePolicy(configRaw, ceilingRaw, constraintsRaw json.RawMessage) (*Policy, error) {
	var cfg browserConfigShape
	if err := parseObject(configRaw, &cfg); err != nil {
		return nil, fmt.Errorf("config: invalid browser config JSON: %w", err)
	}
	var ce browserPolicyShape
	if err := parseObject(ceilingRaw, &ce); err != nil {
		return nil, fmt.Errorf("ceiling: invalid browser ceiling JSON: %w", err)
	}
	var con browserPolicyShape
	if err := parseObject(constraintsRaw, &con); err != nil {
		return nil, fmt.Errorf("constraints: invalid browser constraints JSON: %w", err)
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	}
	if base == "" {
		return nil, fmt.Errorf("config: base_url (or url) is required")
	}

	p := &Policy{
		ServiceURL: base,
		// Deny lists are additive: union of ceiling ∪ constraints.
		DenyHosts: append(trimList(ce.DenyHosts), trimList(con.DenyHosts)...),
		// Numeric bounds: tightest (min) across layers that set one.
		MaxPages:           minPositive(ce.MaxPages, con.MaxPages, DefaultMaxPages),
		MaxScreenshotBytes: minPositive(ce.MaxScreenshotBytes, con.MaxScreenshotBytes, DefaultMaxScreenshotBytes),
		IdleTimeoutS:       minPositive(ce.IdleTimeoutS, con.IdleTimeoutS, DefaultIdleTimeoutS),
		MaxSessionMinutes:  minPositive(ce.MaxSessionMinutes, con.MaxSessionMinutes, DefaultMaxSessionMinutes),
	}
	p.MaxSessionsPerAgent = minPositivePtr(ce.MaxSessionsPerAgent, con.MaxSessionsPerAgent, DefaultMaxSessionsPerAgent)
	return p, nil
}

func parseObject(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func minPositive(a, b, def int) int {
	out := 0
	for _, v := range []int{a, b} {
		if v > 0 && (out == 0 || v < out) {
			out = v
		}
	}
	if out == 0 {
		return def
	}
	return out
}

// minPositivePtr folds an int that may legitimately be 0 (explicit).
// A set layer always wins over the default, even at 0; when both layers
// set a value, the minimum applies.
func minPositivePtr(a, b *int, def int) int {
	vals := make([]int, 0, 2)
	if a != nil {
		vals = append(vals, *a)
	}
	if b != nil {
		vals = append(vals, *b)
	}
	if len(vals) == 0 {
		return def
	}
	out := vals[0]
	for _, v := range vals[1:] {
		if v < out {
			out = v
		}
	}
	return out
}

func trimList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}
