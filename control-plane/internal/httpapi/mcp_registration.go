// TG-5 slice B2a (S-244-series): MCP resource registration — snapshot +
// allowlist validation (docs/tool-gateway.md §6.3).
//
// At registration the CP delegates `initialize` + `tools/list` to the
// tool gateway (see mcp_enumerate_client.go) and stores the resulting
// snapshot (tools + gateway-computed canonical hash + enumerated_at) on
// the resource row. Registration FAILS when the gateway reports the
// upstream unreachable/broken — a half-registered MCP resource must
// never exist, so the caller compensates (deletes the row) on any
// failure returned here.
//
// Allowlist rule: every tools_allow entry must correspond to a REAL tool
// in the enumerated snapshot. Exact names must exist; wildcard entries
// ('*' globs) must match at least one snapshot tool — a wildcard that
// matches nothing is a typo, not a policy, and is rejected. Deny-wins
// semantics are preserved untouched: tools_deny is additive and is
// enforced at call time by the gateway driver (deny evaluated after
// allow, union across layers); this validation never weakens it, so
// deny entries are NOT required to match the snapshot.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/egresspolicy"
)

// mcpRegFailure is a registration/update failure with a pre-mapped HTTP
// response (status + code, optionally structured violations).
type mcpRegFailure struct {
	status     int
	code       string
	message    string
	violations egresspolicy.Violations
}

func (f *mcpRegFailure) respond(w http.ResponseWriter) {
	if len(f.violations) > 0 {
		writeViolations(w, f.code, f.message, f.violations)
		return
	}
	writeError(w, f.status, f.code, f.message)
}

// mcpURLFromConfig extracts the upstream MCP URL from endpoint_config
// (accepts `url` per the §6.3 shape and `base_url` for symmetry with
// rest/git — the gateway's mcp config accepts both as well).
func mcpURLFromConfig(endpointConfig json.RawMessage) string {
	if len(endpointConfig) == 0 {
		return ""
	}
	var s struct {
		URL     string `json:"url"`
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(endpointConfig, &s); err != nil {
		return ""
	}
	if u := strings.TrimSpace(s.URL); u != "" {
		return u
	}
	return strings.TrimSpace(s.BaseURL)
}

// mcpAllowPrivateFor mirrors the resource's egress class into the
// enumerate request's allow_private flag: internal-class egress (the
// resource-level field or a ceiling-pinned egress_class) may reach
// private/loopback MCP upstreams; anything else stays public-only.
// Without this, internal MCP upstreams would enumerate as egress_denied.
func mcpAllowPrivateFor(egressClass string, ceiling json.RawMessage) bool {
	if strings.EqualFold(strings.TrimSpace(egressClass), "internal") {
		return true
	}
	var ce struct {
		EgressClass string `json:"egress_class"`
	}
	if err := json.Unmarshal(ceiling, &ce); err == nil {
		return strings.EqualFold(strings.TrimSpace(ce.EgressClass), "internal")
	}
	return false
}

// mcpWildcardMatch reports whether tool name s matches pattern, where
// '*' matches any run of characters (including none) and every other
// character must match exactly. This MIRRORS the gateway driver's
// wildcardMatch (tool-gateway/internal/drivers/mcp/policy.go) so the
// CP validates allowlist patterns with exactly the semantics that will
// later be enforced at call time — no drift between "what the admin
// allowlisted" and "what the gateway allows".
func mcpWildcardMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	if last != "" {
		if !strings.HasSuffix(s, last) || len(s) < len(last) {
			return false
		}
		s = s[:len(s)-len(last)]
	}
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

// mcpAllowlistViolations validates ceiling.tools_allow against the
// enumerated snapshot tools (see package comment). Absent/empty
// tools_allow is legal (default-deny at call time) and yields no
// violations.
func mcpAllowlistViolations(ceiling json.RawMessage, tools []MCPToolInfo) egresspolicy.Violations {
	if isEmptyJSONObject(ceiling) {
		return nil
	}
	var ce struct {
		ToolsAllow []string `json:"tools_allow"`
	}
	if err := json.Unmarshal(ceiling, &ce); err != nil || len(ce.ToolsAllow) == 0 {
		return nil
	}
	names := make(map[string]bool, len(tools))
	for _, t := range tools {
		names[t.Name] = true
	}
	var v egresspolicy.Violations
	for i, entry := range ce.ToolsAllow {
		field := "policy_ceiling.tools_allow[" + strconv.Itoa(i) + "]"
		if strings.Contains(entry, "*") {
			matched := false
			for name := range names {
				if mcpWildcardMatch(entry, name) {
					matched = true
					break
				}
			}
			if !matched {
				v = append(v, egresspolicy.Violation{
					Field:   field,
					Code:    "unmatched_wildcard",
					Message: "wildcard " + entry + " matches no tool in the enumerated MCP tool set",
				})
			}
			continue
		}
		if !names[entry] {
			v = append(v, egresspolicy.Violation{
				Field:   field,
				Code:    "unknown_tool",
				Message: "tool " + entry + " does not exist in the enumerated MCP tool set",
			})
		}
	}
	return v
}

// snapshotMCPTools runs the gateway enumeration for an MCP resource and
// stamps the snapshot fields onto the (unsaved) resource. It does NOT
// persist — callers persist via CreateResource/UpdateResource so the
// snapshot lands atomically with the rest of the row.
func (s *Server) snapshotMCPTools(ctx context.Context, resource *domain.RegistryResource, bearerToken string) *mcpRegFailure {
	if s.mcpEnumerate == nil {
		return &mcpRegFailure{
			status:  http.StatusServiceUnavailable,
			code:    "gateway_unavailable",
			message: "MCP registration requires the tool gateway (set SKQUAD_TOOL_GATEWAY_URL and SKQUAD_GATEWAY_INTERNAL_TOKEN)",
		}
	}
	upstream := mcpURLFromConfig(resource.EndpointConfig)
	if upstream == "" {
		return &mcpRegFailure{status: http.StatusBadRequest, code: "bad_request", message: "mcp endpoint_config.url is required"}
	}
	res, err := s.mcpEnumerate.Enumerate(ctx, resource.ID, upstream, bearerToken, mcpAllowPrivateFor(resource.EgressClass, resource.PolicyCeiling))
	if err != nil {
		status := http.StatusBadRequest
		code := "mcp_enumerate_failed"
		var ee *mcpEnumerateError
		if errors.As(err, &ee) && ee.Code != "" {
			code = ee.Code
			// Gateway-level unavailability (kill switch / unconfigured
			// internal endpoint) is a platform condition, not a bad
			// registration payload.
			if ee.Status == http.StatusServiceUnavailable {
				status = http.StatusServiceUnavailable
			}
		}
		return &mcpRegFailure{status: status, code: code, message: "could not enumerate MCP upstream (" + code + ")"}
	}
	if v := mcpAllowlistViolations(resource.PolicyCeiling, res.Tools); len(v) > 0 {
		return &mcpRegFailure{
			status:     http.StatusBadRequest,
			code:       "invalid_tool_allowlist",
			message:    "tools_allow entries must exist in the enumerated MCP tool set",
			violations: v,
		}
	}
	snapshot, err := json.Marshal(res.Tools)
	if err != nil {
		return &mcpRegFailure{status: http.StatusInternalServerError, code: "snapshot_encode_failed", message: "could not encode the MCP tool snapshot"}
	}
	resource.ToolsSnapshot = snapshot
	resource.ToolsHash = res.Hash
	now := time.Now().UTC()
	resource.ToolsEnumeratedAt = &now
	return nil
}

// mcpStoredToken resolves the resource-level bearer from the managed
// Secret behind auth_ref (used when an MCP upstream URL moves but no
// fresh credential is supplied — the stored token re-enumerates the
// new URL). Never logs the token. TG-5 B2b: the shared core lives in
// mcpStoredTokenFor so the drift scanner can use it without a *Server.
func (s *Server) mcpStoredToken(ctx context.Context, resource *domain.RegistryResource) (string, error) {
	return mcpStoredTokenFor(ctx, s.resourceSecrets, resource)
}

// mcpSnapshotTools decodes a stored snapshot into the enumerated tool
// list. ok=false when the snapshot is absent/unparseable.
func mcpSnapshotTools(snapshot json.RawMessage) ([]MCPToolInfo, bool) {
	if isEmptyJSONObject(snapshot) {
		return nil, false
	}
	var tools []MCPToolInfo
	if err := json.Unmarshal(snapshot, &tools); err != nil {
		return nil, false
	}
	return tools, true
}
