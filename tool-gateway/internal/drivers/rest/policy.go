package rest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Default caps applied when no policy layer sets one (mirrors the
// registration example in docs/tool-gateway.md §6.2).
const (
	DefaultMaxRequestBytes  = 65536  // 64 KiB
	DefaultMaxResponseBytes = 262144 // 256 KiB
)

// Auth kinds (contract: control-plane/internal/egresspolicy rest shapes).
const (
	AuthNone              = "none"
	AuthBearer            = "bearer"
	AuthAPIKeyHeader      = "api_key_header"
	AuthBasic             = "basic"
	AuthOAuth2ClientCreds = "oauth2_client_credentials"
)

// restConfigShape is the resource config JSON (admin floor).
type restConfigShape struct {
	BaseURL    string `json:"base_url"`
	AuthKind   string `json:"auth_kind"`
	HeaderName string `json:"header_name"`
}

// restPolicyShape is the shared JSON shape of ceiling AND grant
// constraints (control-plane/internal/egresspolicy RestCeilingKeys).
// Pointer slices distinguish "unset" from "empty" so folding can apply
// default-deny semantics correctly.
type restPolicyShape struct {
	Methods          *[]string `json:"methods"`
	PathAllow        *[]string `json:"path_allow"`
	PathDeny         []string  `json:"path_deny"`
	MaxRequestBytes  int       `json:"max_request_bytes"`
	MaxResponseBytes int       `json:"max_response_bytes"`
	RatePerMin       int       `json:"rate_per_min"`
	EgressClass      string    `json:"egress_class"`
}

// Policy is the effective rest policy: config ∧ ceiling ∧ constraints.
type Policy struct {
	BaseURL          string
	AuthKind         string
	HeaderName       string
	Methods          []string // effective allow-set (already folded)
	PathAllow        []string // effective allow globs (default-deny when empty)
	PathDeny         []string // deny globs (deny wins)
	MaxRequestBytes  int
	MaxResponseBytes int
	RatePerMin       int // 0 = unlimited
	AllowPrivate     bool
}

func parseConfig(raw json.RawMessage) (restConfigShape, error) {
	var s restConfigShape
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("invalid rest config JSON: %w", err)
	}
	return s, nil
}

func parseCeiling(raw json.RawMessage) (restPolicyShape, error) {
	var s restPolicyShape
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("invalid rest ceiling JSON: %w", err)
	}
	return s, nil
}

func parseConstraints(raw json.RawMessage) (restPolicyShape, error) {
	var s restPolicyShape
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("invalid rest constraints JSON: %w", err)
	}
	return s, nil
}

// EffectivePolicy folds resource config (admin floor), the resource
// ceiling and the grant constraints into one enforced policy.
//
// Layering rules (§6.2 + §8, mirroring the web driver):
//   - methods: intersection across layers that set one; a layer that
//     sets none inherits the tighter of the others. Empty result = deny all.
//   - path_allow: constraints.path_allow when set (CP no-escalation
//     already proved it a subset of the ceiling), else ceiling.path_allow.
//     Empty = default-deny.
//   - path_deny: UNION of ceiling ∪ constraints (additive only), and
//     deny always wins over allow.
//   - numeric bounds: MIN of every layer that sets one.
//   - egress: private networks allowed only when BOTH ceiling and grant
//     constraints are egress_class=internal (mirrors web allow_private
//     semantics).
func EffectivePolicy(configRaw, ceilingRaw, constraintsRaw json.RawMessage) (*Policy, error) {
	cfg, err := parseConfig(configRaw)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	ce, err := parseCeiling(ceilingRaw)
	if err != nil {
		return nil, fmt.Errorf("ceiling: %w", err)
	}
	con, err := parseConstraints(constraintsRaw)
	if err != nil {
		return nil, fmt.Errorf("constraints: %w", err)
	}

	p := &Policy{
		BaseURL:          strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		AuthKind:         strings.TrimSpace(cfg.AuthKind),
		HeaderName:       strings.TrimSpace(cfg.HeaderName),
		MaxRequestBytes:  DefaultMaxRequestBytes,
		MaxResponseBytes: DefaultMaxResponseBytes,
	}
	if p.AuthKind == "" {
		p.AuthKind = AuthNone
	}
	if p.BaseURL == "" {
		return nil, fmt.Errorf("config: base_url is required")
	}

	// Methods: intersect every layer that sets one.
	methods := nilSafeUpper(ce.Methods)
	if con.Methods != nil {
		methods = intersectUpper(methods, nilSafeUpper(con.Methods))
	}
	p.Methods = methods

	// Path allow: the grant's list wins when present (already proven a
	// subset of the ceiling by the CP); otherwise the ceiling's.
	if con.PathAllow != nil {
		p.PathAllow = *con.PathAllow
	} else if ce.PathAllow != nil {
		p.PathAllow = *ce.PathAllow
	}

	// Path deny: union (additive tightening only).
	p.PathDeny = append(append([]string{}, ce.PathDeny...), con.PathDeny...)

	// Numeric bounds: tighten (min) across layers that set them.
	for _, v := range []int{ce.MaxRequestBytes, con.MaxRequestBytes} {
		if v > 0 && v < p.MaxRequestBytes {
			p.MaxRequestBytes = v
		}
	}
	for _, v := range []int{ce.MaxResponseBytes, con.MaxResponseBytes} {
		if v > 0 && v < p.MaxResponseBytes {
			p.MaxResponseBytes = v
		}
	}
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

	p.AllowPrivate = ce.EgressClass == "internal" && con.EgressClass == "internal"
	return p, nil
}

func nilSafeUpper(p *[]string) []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(*p))
	for _, v := range *p {
		out = append(out, strings.ToUpper(strings.TrimSpace(v)))
	}
	return out
}

// intersectUpper returns the case-insensitive set intersection,
// preserving the order of `a`.
func intersectUpper(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	out := []string{}
	for _, v := range a {
		if set[v] {
			out = append(out, v)
		}
	}
	return out
}

// methodAllowed reports whether method is in the effective allow-set.
// An unset/empty allow-set denies everything (default-deny).
func (p *Policy) methodAllowed(method string) bool {
	m := strings.ToUpper(strings.TrimSpace(method))
	for _, allowed := range p.Methods {
		if allowed == m {
			return true
		}
	}
	return false
}

// pathAllowed applies the default-deny + deny-wins glob rules:
// a path must match at least one path_allow pattern and no path_deny
// pattern. Glob semantics mirror control-plane/internal/egresspolicy
// (exact, trailing "**" = one-or-more trailing segments, "*" within a
// segment = any chars except "/"). The gateway module cannot import the
// CP's internal package, so the semantics are replicated here — keep
// the two in sync if the CP matcher changes.
func (p *Policy) pathAllowed(path string) bool {
	for _, d := range p.PathDeny {
		if globMatch(d, path) {
			return false
		}
	}
	for _, a := range p.PathAllow {
		if globMatch(a, path) {
			return true
		}
	}
	return false
}

// globMatch reports whether concrete path `candidate` is covered by
// pattern `pattern` (same semantics as egresspolicy.globCovers).
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
	// Exact replication of egresspolicy.starSegmentMatch: length guard
	// only. (The CP matcher does not exclude "/" from a single star;
	// enforcement must stay identical to validation semantics.)
	return len(seg) >= len(parts[0])+len(parts[1])
}
