package git

// Effective policy folding for the TG-4b git driver
// (docs/tool-gateway.md §6.5). Mirrors the rest driver's layering
// (tool-gateway/internal/drivers/rest/policy.go): the enforced policy
// is resource config ∧ ceiling ∧ grant constraints, and a malformed
// layer must never widen reach — it denies.
//
// Wire shapes (control-plane/internal/egresspolicy GitConfigKeys /
// GitCeilingKeys):
//
//	config:    {base_url: str}
//	ceiling:   {repos_allow: [glob], allow_push: bool, rate_per_min: int}
//	constraints: same shape as the ceiling (grant narrowing only)

import (
	"encoding/json"
	"fmt"
	"strings"
)

// gitConfigShape is the resource endpoint_config for a git resource.
type gitConfigShape struct {
	BaseURL string `json:"base_url"`
}

// gitPolicyShape is the shared JSON shape of the ceiling AND the grant
// constraints. Pointer fields distinguish "unset" (inherit) from an
// explicit false/empty, so default-deny and push-default semantics
// fold correctly.
type gitPolicyShape struct {
	ReposAllow *[]string `json:"repos_allow"`
	AllowPush  *bool     `json:"allow_push"`
	RatePerMin int       `json:"rate_per_min"`
}

// Policy is the effective git policy: config ∧ ceiling ∧ constraints.
type Policy struct {
	BaseURL    string
	ReposAllow []string // effective allow globs (default-deny when empty)
	AllowPush  bool     // receive-pack permitted (ceiling AND grant must both allow)
	RatePerMin int      // 0 = unlimited
}

func parseObject(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// EffectivePolicy folds the three policy layers.
//
// Layering rules (§6.5, mirroring rest.EffectivePolicy):
//   - repos_allow: constraints.repos_allow when set (the CP
//     no-escalation validator already proved it a subset of the
//     ceiling), else the ceiling's. Empty/unset = default-deny.
//   - allow_push: true only when BOTH the ceiling and the grant
//     constraints set it true (push is additive-tightening-only).
//   - rate_per_min: MIN of every layer that sets one.
func EffectivePolicy(configRaw, ceilingRaw, constraintsRaw json.RawMessage) (*Policy, error) {
	var cfg gitConfigShape
	if err := parseObject(configRaw, &cfg); err != nil {
		return nil, fmt.Errorf("config: invalid git config JSON: %w", err)
	}
	var ce gitPolicyShape
	if err := parseObject(ceilingRaw, &ce); err != nil {
		return nil, fmt.Errorf("ceiling: invalid git ceiling JSON: %w", err)
	}
	var con gitPolicyShape
	if err := parseObject(constraintsRaw, &con); err != nil {
		return nil, fmt.Errorf("constraints: invalid git constraints JSON: %w", err)
	}

	p := &Policy{BaseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")}
	if p.BaseURL == "" {
		return nil, fmt.Errorf("config: base_url is required")
	}

	// Repo allowlist: the grant's list wins when present (already
	// proven a subset of the ceiling by the CP); otherwise the
	// ceiling's. Empty = default-deny (no repo is reachable).
	if con.ReposAllow != nil {
		p.ReposAllow = trimList(*con.ReposAllow)
	} else if ce.ReposAllow != nil {
		p.ReposAllow = trimList(*ce.ReposAllow)
	}

	// Push: both layers must allow. An unset layer does not grant push
	// (default false), so push requires explicit ceiling AND grant
	// opt-in — mirrors the "private networks need both layers" rule.
	p.AllowPush = ce.AllowPush != nil && *ce.AllowPush && con.AllowPush != nil && *con.AllowPush

	// Rate: tightest (min) across layers that set one.
	rates := []int{}
	for _, v := range []int{ce.RatePerMin, con.RatePerMin} {
		if v > 0 {
			rates = append(rates, v)
		}
	}
	if len(rates) > 0 {
		p.RatePerMin = rates[0]
		for _, r := range rates[1:] {
			if r < p.RatePerMin {
				p.RatePerMin = r
			}
		}
	}
	return p, nil
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

// repoAllowed applies the default-deny glob rules: the parsed repo
// ("org/repo", .git already stripped) must match at least one
// repos_allow pattern. Glob semantics mirror
// control-plane/internal/egresspolicy.globCovers (exact, trailing
// "**" = one-or-more trailing segments, "*" within a segment = any
// chars). The gateway module cannot import the CP's internal package,
// so the semantics are replicated here — keep the two in sync if the
// CP matcher changes (same posture as the rest driver's globMatch).
func (p *Policy) repoAllowed(repo string) bool {
	for _, a := range p.ReposAllow {
		if globMatch(a, repo) {
			return true
		}
	}
	return false
}

// globMatch reports whether concrete `candidate` is covered by
// pattern `pattern` (same semantics as egresspolicy.globCovers and
// the rest driver's globMatch).
func globMatch(pattern, candidate string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == candidate {
		return true
	}
	if strings.HasSuffix(pattern, "**") {
		prefix := strings.TrimSuffix(pattern, "**")
		if !strings.HasPrefix(candidate, prefix) {
			return false
		}
		return strings.TrimPrefix(candidate, prefix) != ""
	}
	if strings.Contains(pattern, "*") {
		psegs := strings.Split(pattern, "/")
		csegs := strings.Split(candidate, "/")
		if len(psegs) != len(csegs) {
			return false
		}
		for i := range psegs {
			if !starSegmentMatch(psegs[i], csegs[i]) {
				return false
			}
		}
		return true
	}
	return false
}

func starSegmentMatch(pattern, seg string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == seg
	}
	parts := strings.SplitN(pattern, "*", 2)
	if !strings.HasPrefix(seg, parts[0]) || !strings.HasSuffix(seg, parts[1]) {
		return false
	}
	// Exact replication of egresspolicy.starSegmentMatch: length
	// guard only.
	return len(seg) >= len(parts[0])+len(parts[1])
}
