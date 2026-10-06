package egresspolicy

import (
	"encoding/json"
	"strings"
)

// ValidateGrant enforces the no-escalation invariant (§7/§8):
// grant.constraints ⊆ resource.policy_ceiling.
//
// Semantics per field:
//   - Absent in constraints → inherits the ceiling (no widening possible,
//     because the effective policy is ceiling ∧ constraints).
//   - Allowlists (methods, path_allow, tools_allow, repos_allow): every
//     constraint entry must be covered by some ceiling entry.
//   - Denylist (path_deny, tools_deny, deny_domains, deny_cidrs): may
//     only GROW — every ceiling entry must be covered by some constraint
//     entry when the constraint sets the list at all.
//   - Numeric bounds (rates, sizes): constraint ≤ ceiling.
//   - egress_class: must equal the ceiling (internal reach is admin-only).
//   - allow_private_network / allow_push: cannot be true unless the
//     ceiling allows it.
//   - per_tool.requires_confirmation: a grant may tighten (true) but never
//     loosen a ceiling-mandated confirmation; unknown tools are rejected.
//
// Both sides are shape-validated first, so unknown keys are violations on
// either side. Returns nil when the grant is safe.
func ValidateGrant(resourceType string, constraints, ceiling json.RawMessage) Violations {
	if v := ValidateCeiling(resourceType, ceiling); len(v) > 0 {
		return prefixViolations(v, "ceiling.")
	}
	cObj, v := object("constraints", constraints)
	if len(v) > 0 {
		return v
	}
	ceObj, _ := object("policy_ceiling", ceiling)

	switch resourceType {
	case "web":
		return validateWebGrant(cObj, ceObj)
	case "rest":
		return validateRestGrant(cObj, ceObj)
	case "mcp":
		return validateMCPGrant(cObj, ceObj)
	case "git":
		return validateGitGrant(cObj, ceObj)
	default:
		// Untyped resources: constraints must simply be an object; nothing
		// to escalate against.
		return nil
	}
}

func prefixViolations(v Violations, prefix string) Violations {
	out := make(Violations, len(v))
	for i, x := range v {
		x.Field = prefix + x.Field
		out[i] = x
	}
	return out
}

// globCovers reports whether ceiling pattern `pattern` covers candidate
// `candidate`. Supports:
//   - exact match
//   - trailing "**" → prefix match over one-or-more trailing segments
//     ("org/**" covers "org/repo" and "org/a/b"; "org/**" does NOT
//     cover bare "org")
//   - "*" within a segment → matches any characters except "/"
//
// A candidate wildcard is only covered by an identical pattern, never by
// a narrower one — "issues/**" is not covered by "issues/123".
func globCovers(pattern, candidate string) bool {
	if pattern == candidate {
		return true
	}
	if strings.HasSuffix(pattern, "**") {
		prefix := strings.TrimSuffix(pattern, "**")
		if !strings.HasPrefix(candidate, prefix) {
			return false
		}
		rest := strings.TrimPrefix(candidate, prefix)
		return rest != "" // ** requires at least one character
	}
	if strings.Contains(pattern, "*") {
		// segment-wise single-star matching
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
	// pattern contains at most one '*' (double-star handled above)
	if !strings.Contains(pattern, "*") {
		return pattern == seg
	}
	parts := strings.SplitN(pattern, "*", 2)
	if !strings.HasPrefix(seg, parts[0]) || !strings.HasSuffix(seg, parts[1]) {
		return false
	}
	return len(seg) >= len(parts[0])+len(parts[1])
}

func allowSubset(field string, cList, ceList []string) Violations {
	var v Violations
	for _, c := range cList {
		covered := false
		for _, ce := range ceList {
			if globCovers(ce, c) {
				covered = true
				break
			}
		}
		if !covered {
			v = append(v, Violation{
				Field:   field,
				Code:    "not_subset",
				Message: "grant allows " + c + " which is outside the resource ceiling",
			})
		}
	}
	return v
}

func denyGrows(field string, cList, ceList []string) Violations {
	var v Violations
	for _, ce := range ceList {
		covered := false
		for _, c := range cList {
			if globCovers(c, ce) {
				covered = true
				break
			}
		}
		if !covered {
			v = append(v, Violation{
				Field:   field,
				Code:    "denylist_shrunk",
				Message: "grant does not deny " + ce + " required by the resource ceiling (denylists may only grow)",
			})
		}
	}
	return v
}

func numLE(field string, cVal, ceVal int) Violations {
	if cVal > ceVal {
		return Violations{{Field: field, Code: "exceeds_ceiling", Message: "grant value exceeds ceiling"}}
	}
	return nil
}

func equalField(field string, cVal, ceVal string) Violations {
	if cVal != ceVal {
		return Violations{{Field: field, Code: "mismatch", Message: field + " must equal the resource ceiling"}}
	}
	return nil
}

func listIn(obj map[string]json.RawMessage, field string) []string {
	raw, ok := obj[field]
	if !ok {
		return nil
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

func intIn(obj map[string]json.RawMessage, field string) int {
	raw, ok := obj[field]
	if !ok {
		return 0
	}
	var out int
	_ = json.Unmarshal(raw, &out)
	return out
}

func boolIn(obj map[string]json.RawMessage, field string) (bool, bool) {
	raw, ok := obj[field]
	if !ok {
		return false, false
	}
	var out bool
	_ = json.Unmarshal(raw, &out)
	return out, true
}

func validateWebGrant(c, ce map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("constraints", c, WebShapeKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	// Denylists may only grow when the grant sets them at all.
	if _, has := c["deny_domains"]; has {
		v = append(v, denyGrows("constraints.deny_domains", listIn(c, "deny_domains"), listIn(ce, "deny_domains"))...)
	}
	if _, has := c["deny_cidrs"]; has {
		v = append(v, denyGrows("constraints.deny_cidrs", listIn(c, "deny_cidrs"), listIn(ce, "deny_cidrs"))...)
	}
	if _, has := c["rate_per_min"]; has {
		v = append(v, numLE("constraints.rate_per_min", intIn(c, "rate_per_min"), intIn(ce, "rate_per_min"))...)
	}
	if _, has := c["max_bytes"]; has {
		v = append(v, numLE("constraints.max_bytes", intIn(c, "max_bytes"), intIn(ce, "max_bytes"))...)
	}
	if _, has := c["timeout_seconds"]; has {
		v = append(v, numLE("constraints.timeout_seconds", intIn(c, "timeout_seconds"), intIn(ce, "timeout_seconds"))...)
	}
	if priv, has := boolIn(c, "allow_private_network"); has {
		cePriv, _ := boolIn(ce, "allow_private_network")
		if priv && !cePriv {
			v = append(v, Violation{Field: "constraints.allow_private_network", Code: "exceeds_ceiling", Message: "private network access requires an internal-class ceiling"})
		}
	}
	return sortByField(v)
}

func validateRestGrant(c, ce map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("constraints", c, RestCeilingKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	if _, has := c["methods"]; has {
		v = append(v, allowSubset("constraints.methods", upperAll(listIn(c, "methods")), upperAll(listIn(ce, "methods")))...)
	}
	if _, has := c["path_allow"]; has {
		v = append(v, allowSubset("constraints.path_allow", listIn(c, "path_allow"), listIn(ce, "path_allow"))...)
	}
	if _, has := c["path_deny"]; has {
		v = append(v, denyGrows("constraints.path_deny", listIn(c, "path_deny"), listIn(ce, "path_deny"))...)
	}
	if _, has := c["max_request_bytes"]; has {
		v = append(v, numLE("constraints.max_request_bytes", intIn(c, "max_request_bytes"), intIn(ce, "max_request_bytes"))...)
	}
	if _, has := c["max_response_bytes"]; has {
		v = append(v, numLE("constraints.max_response_bytes", intIn(c, "max_response_bytes"), intIn(ce, "max_response_bytes"))...)
	}
	if _, has := c["rate_per_min"]; has {
		v = append(v, numLE("constraints.rate_per_min", intIn(c, "rate_per_min"), intIn(ce, "rate_per_min"))...)
	}
	if _, has := c["egress_class"]; has {
		var ec string
		_ = json.Unmarshal(c["egress_class"], &ec)
		var cec string
		if raw, ok := ce["egress_class"]; ok {
			_ = json.Unmarshal(raw, &cec)
		}
		v = append(v, equalField("constraints.egress_class", ec, cec)...)
	}
	return sortByField(v)
}

func upperAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func validateMCPGrant(c, ce map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("constraints", c, MCPCeilingKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	if _, has := c["tools_allow"]; has {
		v = append(v, allowSubset("constraints.tools_allow", listIn(c, "tools_allow"), listIn(ce, "tools_allow"))...)
	}
	if _, has := c["tools_deny"]; has {
		v = append(v, denyGrows("constraints.tools_deny", listIn(c, "tools_deny"), listIn(ce, "tools_deny"))...)
	}
	if _, has := c["rate_per_min"]; has {
		v = append(v, numLE("constraints.rate_per_min", intIn(c, "rate_per_min"), intIn(ce, "rate_per_min"))...)
	}
	if _, has := c["max_args_bytes"]; has {
		v = append(v, numLE("constraints.max_args_bytes", intIn(c, "max_args_bytes"), intIn(ce, "max_args_bytes"))...)
	}
	if raw, has := c["per_tool"]; has {
		var cPer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &cPer); err != nil {
			v = append(v, Violation{Field: "constraints.per_tool", Code: "invalid_type", Message: "per_tool must be an object keyed by tool name"})
			return sortByField(v)
		}
		cePer := map[string]map[string]bool{}
		if ceRaw, ok := ce["per_tool"]; ok {
			var cePerRaw map[string]json.RawMessage
			if err := json.Unmarshal(ceRaw, &cePerRaw); err == nil {
				for name, traw := range cePerRaw {
					var tObj map[string]json.RawMessage
					if err := json.Unmarshal(traw, &tObj); err == nil {
						rc, _ := boolIn(tObj, "requires_confirmation")
						cePer[name] = map[string]bool{"requires_confirmation": rc}
					}
				}
			}
		}
		ceAllow := listIn(ce, "tools_allow")
		for name, traw := range cPer {
			var tObj map[string]json.RawMessage
			if err := json.Unmarshal(traw, &tObj); err != nil {
				v = append(v, Violation{Field: "constraints.per_tool." + name, Code: "invalid_type", Message: "per_tool entry must be an object"})
				continue
			}
			v = append(v, checkUnknownKeys("constraints.per_tool."+name, tObj, "requires_confirmation")...)
			if rc, hasRC := boolIn(tObj, "requires_confirmation"); hasRC {
				if !rc {
					if ceT, ok := cePer[name]; ok && ceT["requires_confirmation"] {
						v = append(v, Violation{Field: "constraints.per_tool." + name + ".requires_confirmation", Code: "exceeds_ceiling", Message: "ceiling requires confirmation for this tool"})
					}
				}
			}
			// per_tool entries must reference a tool inside the ceiling allowlist
			allowed := false
			for _, a := range ceAllow {
				if globCovers(a, name) || a == name {
					allowed = true
					break
				}
			}
			if !allowed {
				v = append(v, Violation{Field: "constraints.per_tool." + name, Code: "not_subset", Message: "tool is outside the ceiling tools_allow list"})
			}
		}
	}
	return sortByField(v)
}

func validateGitGrant(c, ce map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("constraints", c, GitCeilingKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	if _, has := c["repos_allow"]; has {
		v = append(v, allowSubset("constraints.repos_allow", listIn(c, "repos_allow"), listIn(ce, "repos_allow"))...)
	}
	if _, has := c["allow_push"]; has {
		push, _ := boolIn(c, "allow_push")
		cePush, _ := boolIn(ce, "allow_push")
		if push && !cePush {
			v = append(v, Violation{Field: "constraints.allow_push", Code: "exceeds_ceiling", Message: "push is not permitted by the resource ceiling (git:push)"})
		}
	}
	if _, has := c["rate_per_min"]; has {
		v = append(v, numLE("constraints.rate_per_min", intIn(c, "rate_per_min"), intIn(ce, "rate_per_min"))...)
	}
	return sortByField(v)
}
