package mcp

// Effective policy folding for the TG-5 slice A mcp driver
// (docs/tool-gateway.md §6.3). Mirrors the rest/git drivers' layering
// (tool-gateway/internal/drivers/rest/policy.go, .../git/policy.go):
// the enforced policy is resource config ∧ ceiling ∧ grant
// constraints, and a malformed layer must never widen reach — it
// denies.
//
// Wire shapes (Slice B / control-plane owns registration; keys follow
// the §6.3 example):
//
//	config:      {base_url|url: str, auth_kind: "bearer"}
//	ceiling:     {tools_allow: [name|glob], tools_deny: [name|glob],
//	            per_tool: {name: {requires_confirmation: bool}},
//	            rate_per_min: int, max_args_bytes: int, egress_class: str}
//	constraints: same shape as the ceiling (grant narrowing only)

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultMaxArgsBytes is applied when no policy layer sets a cap
// (§6.3 example value).
const DefaultMaxArgsBytes = 32768

// AuthBearer is the only auth kind the v1 mcp driver supports
// (streamable-HTTP bearer, per §6.3 transport resolution).
const AuthBearer = "bearer"

// mcpConfigShape is the resource endpoint_config for an mcp resource.
// Both base_url and url are accepted (the §6.3 example uses `url`;
// rest/git use `base_url` — Slice B may emit either).
type mcpConfigShape struct {
	BaseURL  string `json:"base_url"`
	URL      string `json:"url"`
	AuthKind string `json:"auth_kind"`
}

// perToolRule is the per_tool ceiling entry. Only requires_confirmation
// exists in v1; unknown fields are ignored (forward-compatible).
type perToolRule struct {
	RequiresConfirmation bool `json:"requires_confirmation"`
}

// mcpPolicyShape is the shared JSON shape of the ceiling AND the grant
// constraints. Pointer fields distinguish "unset" (inherit) from an
// explicit empty list, so default-deny folds correctly.
type mcpPolicyShape struct {
	ToolsAllow   *[]string              `json:"tools_allow"`
	ToolsDeny    []string               `json:"tools_deny"`
	PerTool      map[string]perToolRule `json:"per_tool"`
	RatePerMin   int                    `json:"rate_per_min"`
	MaxArgsBytes int                    `json:"max_args_bytes"`
	EgressClass  string                 `json:"egress_class"`
}

// Policy is the effective mcp policy: config ∧ ceiling ∧ constraints.
type Policy struct {
	BaseURL      string
	AuthKind     string
	ToolsAllow   []string // effective allow list/globs (default-deny when empty)
	ToolsDeny    []string // deny globs, UNION of layers (deny wins)
	PerTool      map[string]perToolRule
	RatePerMin   int // 0 = unlimited
	MaxArgsBytes int
	AllowPrivate bool
}

func parseObject(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// EffectivePolicy folds the three policy layers.
//
// Layering rules (§6.3, mirroring rest/git):
//   - tools_allow: constraints.tools_allow when set (the CP
//     no-escalation validator proves it a subset of the ceiling),
//     else the ceiling's. Empty/unset = default-deny.
//   - tools_deny: UNION of ceiling ∪ constraints (additive only);
//     deny always wins over allow.
//   - per_tool: merged; requires_confirmation is sticky-true (either
//     layer setting it true forces confirmation — tightening only).
//   - max_args_bytes / rate_per_min: MIN of every layer that sets one.
//   - egress: private networks only when BOTH ceiling and grant
//     constraints are egress_class=internal (rest semantics).
func EffectivePolicy(configRaw, ceilingRaw, constraintsRaw json.RawMessage) (*Policy, error) {
	var cfg mcpConfigShape
	if err := parseObject(configRaw, &cfg); err != nil {
		return nil, fmt.Errorf("config: invalid mcp config JSON: %w", err)
	}
	var ce mcpPolicyShape
	if err := parseObject(ceilingRaw, &ce); err != nil {
		return nil, fmt.Errorf("ceiling: invalid mcp ceiling JSON: %w", err)
	}
	var con mcpPolicyShape
	if err := parseObject(constraintsRaw, &con); err != nil {
		return nil, fmt.Errorf("constraints: invalid mcp constraints JSON: %w", err)
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	}
	if base == "" {
		return nil, fmt.Errorf("config: base_url (or url) is required")
	}

	p := &Policy{
		BaseURL:  base,
		AuthKind: strings.TrimSpace(cfg.AuthKind),
	}
	if p.AuthKind == "" {
		p.AuthKind = AuthBearer
	}

	// Tool allowlist: the grant's list wins when present (already
	// proven a subset of the ceiling by the CP); otherwise the
	// ceiling's. Empty = default-deny (no tool is reachable).
	if con.ToolsAllow != nil {
		p.ToolsAllow = trimList(*con.ToolsAllow)
	} else if ce.ToolsAllow != nil {
		p.ToolsAllow = trimList(*ce.ToolsAllow)
	}

	// Deny list: union of both layers — a grant can add denials but
	// never remove them.
	p.ToolsDeny = append(trimList(ce.ToolsDeny), trimList(con.ToolsDeny)...)

	// Per-tool rules: merge with sticky-true confirmation.
	p.PerTool = map[string]perToolRule{}
	for name, rule := range ce.PerTool {
		p.PerTool[name] = rule
	}
	for name, rule := range con.PerTool {
		merged := p.PerTool[name]
		merged.RequiresConfirmation = merged.RequiresConfirmation || rule.RequiresConfirmation
		p.PerTool[name] = merged
	}

	// Numeric bounds: tightest (min) across layers that set one.
	p.MaxArgsBytes = minPositive(ce.MaxArgsBytes, con.MaxArgsBytes, DefaultMaxArgsBytes)
	p.RatePerMin = minPositive(ce.RatePerMin, con.RatePerMin, 0)

	// Egress: private ranges require BOTH layers to be internal.
	p.AllowPrivate = strings.EqualFold(strings.TrimSpace(ce.EgressClass), "internal") &&
		strings.EqualFold(strings.TrimSpace(con.EgressClass), "internal")

	return p, nil
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

// toolAllowed applies the default-deny list rules: the tool name must
// match at least one tools_allow entry and no tools_deny entry.
// Entries may be exact names or globs with '*' (the §6.3 example uses
// "*_delete"), so matching is wildcard-capable but otherwise exact.
func (p *Policy) toolAllowed(tool string) bool {
	if !anyMatch(p.ToolsAllow, tool) {
		return false
	}
	if anyMatch(p.ToolsDeny, tool) {
		return false
	}
	return true
}

func anyMatch(patterns []string, s string) bool {
	for _, pat := range patterns {
		if wildcardMatch(pat, s) {
			return true
		}
	}
	return false
}

// wildcardMatch reports whether s matches pattern, where '*' matches
// any run of characters (including none) and all other characters must
// match exactly. No '?' or character classes — tool names are simple
// identifiers and over-generous globs only widen reach.
func wildcardMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	// Prefix must match the first literal.
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	// Suffix must match the last literal.
	last := parts[len(parts)-1]
	if last != "" {
		if !strings.HasSuffix(s, last) || len(s) < len(last) {
			return false
		}
		s = s[:len(s)-len(last)]
	}
	// Middle literals must appear in order.
	for _, mid := range parts[1 : len(parts)-1] {
		if mid == "" {
			continue
		}
		idx := strings.Index(s, mid)
		if idx < 0 {
			return false
		}
		s = s[idx+len(mid):]
	}
	return true
}

// requiresConfirmation reports the effective per-tool confirmation
// rule for a tool (exact-name lookup; globs are allow/deny-only).
func (p *Policy) requiresConfirmation(tool string) bool {
	rule, ok := p.PerTool[tool]
	return ok && rule.RequiresConfirmation
}
