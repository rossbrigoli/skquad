// TG-6 slice D: browser-driver resources.
//
// A browser resource is an `mcp`-typed registration whose
// endpoint_config declares `driver: "browser"` and whose url points at
// the in-cluster browser service (default
// http://skquad-browser-service.skquad-browser.svc.cluster.local:8090/mcp).
// The gateway selects the browser driver from that config field
// (tool-gateway/internal/httpapi/server.go grantRequestsBrowserDriver)
// and folds the grant with
// tool-gateway/internal/drivers/browser/policy.go. THIS FILE IS THE CP
// MIRROR of that ceiling shape: the flat ceiling/constraints shape below
// must stay byte-compatible with the gateway's browserPolicyShape —
//
//	{deny_hosts: [glob], max_pages: int, max_screenshot_bytes: int,
//	 idle_timeout_s: int, max_session_minutes: int,
//	 max_sessions_per_agent: int}
//
// NOTE on the task-doc wording "navigation_ceiling.deny_hosts": the
// gateway folds a FLAT ceiling (no navigation_ceiling nesting exists
// anywhere on the wire); policy.go is authoritative, so CP emits flat.
//
// Registration defaults (docs/tg6-browser-protocol.md §2/§6) are
// MATERIALIZED into the stored policy_ceiling at registration so the
// grant payload the gateway reads is fully explicit; the gateway would
// fold the same defaults anyway (tightening-only min-fold / deny union).
package egresspolicy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Browser ceiling defaults — MUST match
// tool-gateway/internal/drivers/browser/policy.go Default* constants.
const (
	BrowserDefaultMaxPages            = 50
	BrowserDefaultMaxScreenshotBytes  = 2 * 1024 * 1024 // 2 MiB
	BrowserDefaultIdleTimeoutS        = 600
	BrowserDefaultMaxSessionMinutes   = 30
	BrowserDefaultMaxSessionsPerAgent = 1
)

// Browser ceiling upper bounds (CP-side admin sanity rails; the gateway
// folds mins, so these only constrain what an admin may REGISTER).
const (
	BrowserMaxPagesUpperBound             = 500
	BrowserMaxSessionMinutesUpperBound    = 240
	BrowserMaxSessionsPerAgentUpperBound  = 64
	BrowserMaxScreenshotBytesUpperBound   = 32 * 1024 * 1024 // 32 MiB
	BrowserMaxIdleTimeoutSecondsUpperBound = 86400            // 24 h
)

// BrowserCeilingKeys is the exact flat shape the gateway's
// browserPolicyShape accepts. Unknown keys are rejected everywhere
// (ceiling, constraints) so a typo can never silently widen a policy.
var BrowserCeilingKeys = []string{
	"deny_hosts", "max_pages", "max_screenshot_bytes",
	"idle_timeout_s", "max_session_minutes", "max_sessions_per_agent",
}

// IsBrowserConfig reports whether an mcp endpoint_config declares the
// TG-6 browser driver (`driver: "browser"`, case-insensitive to mirror
// the gateway's EqualFold selection).
func IsBrowserConfig(endpointConfig json.RawMessage) bool {
	if len(endpointConfig) == 0 {
		return false
	}
	var s struct {
		Driver string `json:"driver"`
	}
	if err := json.Unmarshal(endpointConfig, &s); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(s.Driver), "browser")
}

// boundedIntField validates an optional int within [minV, maxV].
func boundedIntField(obj map[string]json.RawMessage, key, path string, minV, maxV int, out *int) Violations {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return Violations{{Field: path, Code: "invalid_type", Message: path + " must be an integer"}}
	}
	if *out < minV || *out > maxV {
		return Violations{{Field: path, Code: "invalid_value",
			Message: fmt.Sprintf("%s must be between %d and %d", path, minV, maxV)}}
	}
	return nil
}

// validateBrowserShape validates one layer (ceiling or constraints)
// against the browser policy shape: unknown keys rejected, types and
// bounds enforced.
func validateBrowserShape(field string, obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys(field, obj, BrowserCeilingKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var denyHosts []string
	v = append(v, stringListField(obj, "deny_hosts", field+".deny_hosts", &denyHosts)...)
	var maxPages, maxSessBytes, idleS, sessMin, sessions int
	v = append(v, boundedIntField(obj, "max_pages", field+".max_pages", 1, BrowserMaxPagesUpperBound, &maxPages)...)
	v = append(v, boundedIntField(obj, "max_screenshot_bytes", field+".max_screenshot_bytes", 1, BrowserMaxScreenshotBytesUpperBound, &maxSessBytes)...)
	v = append(v, boundedIntField(obj, "idle_timeout_s", field+".idle_timeout_s", 1, BrowserMaxIdleTimeoutSecondsUpperBound, &idleS)...)
	v = append(v, boundedIntField(obj, "max_session_minutes", field+".max_session_minutes", 1, BrowserMaxSessionMinutesUpperBound, &sessMin)...)
	v = append(v, boundedIntField(obj, "max_sessions_per_agent", field+".max_sessions_per_agent", 1, BrowserMaxSessionsPerAgentUpperBound, &sessions)...)
	return sortByField(v)
}

// ValidateBrowserCeiling validates a browser resource's policy_ceiling.
func ValidateBrowserCeiling(raw json.RawMessage) Violations {
	obj, v := object("policy_ceiling", raw)
	if len(v) > 0 {
		return v
	}
	return validateBrowserShape("policy_ceiling", obj)
}

// NormalizeBrowserCeiling fills every unset ceiling field with the
// protocol default so the stored ceiling (and therefore every grant
// payload the gateway reads) is fully explicit. Validated input is
// preserved verbatim; only absent keys gain defaults. deny_hosts
// defaults to the empty array [].
func NormalizeBrowserCeiling(raw json.RawMessage) (json.RawMessage, Violations) {
	obj, v := object("policy_ceiling", raw)
	if len(v) > 0 {
		return nil, v
	}
	// Defense in depth: never normalize a malformed ceiling into a
	// defaulted-but-still-invalid one.
	if v := validateBrowserShape("policy_ceiling", obj); len(v) > 0 {
		return nil, v
	}
	if _, ok := obj["deny_hosts"]; !ok {
		obj["deny_hosts"] = json.RawMessage(`[]`)
	}
	setDefaultInt(obj, "max_pages", BrowserDefaultMaxPages)
	setDefaultInt(obj, "max_screenshot_bytes", BrowserDefaultMaxScreenshotBytes)
	setDefaultInt(obj, "idle_timeout_s", BrowserDefaultIdleTimeoutS)
	setDefaultInt(obj, "max_session_minutes", BrowserDefaultMaxSessionMinutes)
	setDefaultInt(obj, "max_sessions_per_agent", BrowserDefaultMaxSessionsPerAgent)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, Violations{{Field: "policy_ceiling", Code: "invalid_json", Message: "policy_ceiling could not be normalized"}}
	}
	return out, nil
}

func setDefaultInt(obj map[string]json.RawMessage, key string, def int) {
	if raw, ok := obj[key]; ok && len(raw) > 0 && string(raw) != "null" {
		return
	}
	obj[key] = json.RawMessage(fmt.Sprintf("%d", def))
}

// ValidateBrowserGrant enforces the no-escalation invariant for
// browser-driver resources: grant constraints ⊆ policy_ceiling under
// the same folding semantics the gateway applies —
//   - numeric bounds may only TIGHTEN (grant ≤ ceiling);
//   - deny_hosts may only GROW (every ceiling denial must still be
//     denied by the grant when the grant sets the list at all);
//   - unknown keys on either side are rejected.
//
// Both layers are shape-validated first, mirroring ValidateGrant.
func ValidateBrowserGrant(constraints, ceiling json.RawMessage) Violations {
	if v := ValidateBrowserCeiling(ceiling); len(v) > 0 {
		return prefixViolations(v, "ceiling.")
	}
	cObj, v := object("constraints", constraints)
	if len(v) > 0 {
		return v
	}
	if v := validateBrowserShape("constraints", cObj); len(v) > 0 {
		return v
	}
	ceObj, _ := object("policy_ceiling", ceiling)

	var out Violations
	if _, has := cObj["deny_hosts"]; has {
		out = append(out, denyGrows("constraints.deny_hosts", listIn(cObj, "deny_hosts"), listIn(ceObj, "deny_hosts"))...)
	}
	// Numeric tightening: grant must not exceed the ceiling. A ceiling
	// that somehow lacks a field falls back to the protocol default
	// (registration normalizes ceilings, so this is belt-and-braces).
	for _, f := range []struct {
		key string
		def int
	}{
		{"max_pages", BrowserDefaultMaxPages},
		{"max_screenshot_bytes", BrowserDefaultMaxScreenshotBytes},
		{"idle_timeout_s", BrowserDefaultIdleTimeoutS},
		{"max_session_minutes", BrowserDefaultMaxSessionMinutes},
		{"max_sessions_per_agent", BrowserDefaultMaxSessionsPerAgent},
	} {
		if _, has := cObj[f.key]; !has {
			continue
		}
		ceVal := intIn(ceObj, f.key)
		if ceVal <= 0 {
			ceVal = f.def
		}
		out = append(out, numLE("constraints."+f.key, intIn(cObj, f.key), ceVal)...)
	}
	return sortByField(out)
}
