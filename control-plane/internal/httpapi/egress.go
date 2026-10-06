// TG-2 (S-248): governed egress plane — internal policy API and shared
// validation helpers for the typed registry.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/egresspolicy"
)

// writeViolations renders a structured 400 for validation failures. The
// shape is stable for API clients:
//
//	{"error":{"code":"...","message":"..."},"violations":[{field,code,message}]}
func writeViolations(w http.ResponseWriter, code string, message string, violations egresspolicy.Violations) {
	if violations == nil {
		violations = egresspolicy.Violations{}
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":      map[string]string{"code": code, "message": message},
		"violations": violations,
	})
}

// validateTypedResourceFields validates the TG-2 typed fields for a
// resource create/update. It returns violations for the first failing
// area (ceiling first, then config) so callers get a deterministic order.
func validateTypedResourceFields(resourceType string, endpointConfig, policyCeiling json.RawMessage, riskTier, egressClass string) egresspolicy.Violations {
	if v := validateTierAndClass(riskTier, egressClass); len(v) > 0 {
		return v
	}
	if !egresspolicy.IsTyped(resourceType) {
		// Untyped resources must not smuggle typed config in: an empty
		// object is fine (default), anything else is rejected so a
		// mis-set type can never create half-typed resources.
		if !isEmptyJSONObject(endpointConfig) {
			return egresspolicy.Violations{{Field: "endpoint_config", Code: "not_typed", Message: "endpoint_config is only valid for web/rest/mcp/git resources"}}
		}
		if !isEmptyJSONObject(policyCeiling) {
			return egresspolicy.Violations{{Field: "policy_ceiling", Code: "not_typed", Message: "policy_ceiling is only valid for web/rest/mcp/git resources"}}
		}
		return nil
	}
	if v := egresspolicy.ValidateCeiling(resourceType, policyCeiling); len(v) > 0 {
		return v
	}
	return egresspolicy.ValidateEndpointConfig(resourceType, endpointConfig)
}

func validateTierAndClass(riskTier, egressClass string) egresspolicy.Violations {
	var v egresspolicy.Violations
	if riskTier != "" && riskTier != "low" && riskTier != "medium" && riskTier != "high" {
		v = append(v, egresspolicy.Violation{Field: "risk_tier", Code: "invalid_value", Message: "risk_tier must be low, medium or high"})
	}
	if egressClass != "" && egressClass != "public" && egressClass != "internal" {
		v = append(v, egresspolicy.Violation{Field: "egress_class", Code: "invalid_value", Message: "egress_class must be public or internal"})
	}
	return v
}

func isEmptyJSONObject(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null" || string(raw) == "{}"
}

// ── Internal policy API (design §5.1.5, TG-2) ─────────────────────────────
//
// GET /internal/v1/policy?agent=<id>
//
// The gateway's single source of truth for agent policy. Contract (TG-1):
//   - 200: {agent_id, credential_hash, generation, grants:[...]} + ETag
//   - 304 on If-None-Match match
//   - 404 unknown agent
//   - empty credential_hash authorizes nothing (gateway must deny)
//   - response agent_id must match the requested agent
//
// SECURITY: this endpoint must never leak secret material. credential_hash
// is base64 RawStd sha256 of the agent token (same encoding as
// matchesAgentCredential). Endpoint configs are emitted with a
// denylist-stripped copy (stripSecretKeys) as defense-in-depth — configs
// are schema-validated to never contain secrets, but a future schema slip
// must not become an exfil path.
//
// Network posture: /internal/* is reachable only from the cluster
// internal network (NetworkPolicy); no app-layer auth, matching the
// TG-1 gateway client which sends no credential on this path. Do not
// expose this route outside the cluster.

// policyGrant is one effective grant in the policy snapshot.
type policyGrant struct {
	ResourceID   string          `json:"resource_id"`
	ResourceType string          `json:"resource_type"`
	Config       json.RawMessage `json:"config,omitempty"`
	Constraints  json.RawMessage `json:"constraints,omitempty"`
	Ceiling      json.RawMessage `json:"ceiling,omitempty"`
	RiskTier     string          `json:"risk_tier,omitempty"`
	EgressClass  string          `json:"egress_class,omitempty"`
	// TG-5 slice B2a: MCP tool snapshot surfaced for the gateway's mcp
	// driver (tools + gateway-canonical hash, docs §6.3). The ceiling is
	// emitted verbatim (tools_allow/tools_deny/per_tool/rate_per_min/
	// max_args_bytes) exactly as the slice A driver folds it. Snapshot
	// input schemas pass through the same secret-stripping defense as
	// configs: a schema property that looks like secret material is
	// removed fail-closed (call-time arg validation then rejects it),
	// because this response must never become an exfil path.
	ToolsSnapshot json.RawMessage `json:"tools_snapshot,omitempty"`
	ToolsHash     string          `json:"tools_hash,omitempty"`
}

// policySnapshot is the full policy document for one agent.
type policySnapshot struct {
	AgentID        string        `json:"agent_id"`
	CredentialHash string        `json:"credential_hash"`
	Generation     int           `json:"generation"`
	Grants         []policyGrant `json:"grants"`
}

// secretKeyDenylist: any JSON key (case-insensitive, any nesting) matching
// one of these substrings is stripped from policy responses. The schema
// already forbids secrets in endpoint_config; this is the belt to the
// suspenders.
var secretKeyDenylist = []string{
	"secret", "password", "passwd", "token", "api_key", "apikey",
	"credential", "authorization", "auth_ref", "private_key", "access_key",
}

func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, bad := range secretKeyDenylist {
		if strings.Contains(k, bad) {
			return true
		}
	}
	return false
}

// stripSecretKeys recursively removes secret-looking keys from a parsed
// JSON value. Non-object values pass through.
func stripSecretKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSecretKey(k) {
				continue
			}
			out[k] = stripSecretKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripSecretKeys(val)
		}
		return out
	default:
		return v
	}
}

// safeConfigJSON parses raw config, strips secret keys, and re-marshals.
// Invalid JSON degrades to an empty object (never echo unparsed bytes).
func safeConfigJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return json.RawMessage(`{}`)
	}
	stripped, err := json.Marshal(stripSecretKeys(parsed))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return stripped
}

func (s *Server) internalPolicy(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent query parameter is required")
		return
	}
	snap, err := s.buildPolicySnapshot(r.Context(), agentID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	body, err := json.Marshal(snap)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to encode policy")
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	if match := r.Header.Get("If-None-Match"); match != "" && etagsMatch(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// etagsMatch compares an If-None-Match header (possibly a comma list or
// W/ weak prefix) against the strong ETag.
func etagsMatch(ifNoneMatch, etag string) bool {
	weak := strings.TrimPrefix(etag, "W/")
	for _, cand := range strings.Split(ifNoneMatch, ",") {
		c := strings.TrimSpace(cand)
		if c == "*" {
			return true
		}
		if strings.TrimPrefix(c, "W/") == weak {
			return true
		}
	}
	return false
}

// buildPolicySnapshot assembles the effective policy document for one
// agent. Deprecated resources and grants pointing at missing resources
// are skipped (fail-closed: nothing surfaced that isn't live).
func (s *Server) buildPolicySnapshot(ctx context.Context, agentID string) (*policySnapshot, error) {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	snap := &policySnapshot{
		AgentID: agent.ID,
		Grants:  []policyGrant{},
	}
	identity, err := s.store.GetAgentIdentity(ctx, agent.ID)
	if err == nil && identity != nil {
		snap.CredentialHash = identity.CredentialHash
		snap.Generation = identity.Generation
	} else if err != nil && !isNotFound(err) {
		return nil, err
	}
	perms, err := s.store.ListAgentPermissions(ctx, agent.ID)
	if err != nil {
		return nil, err
	}
	for _, perm := range perms {
		resource, err := s.store.GetResource(ctx, perm.ResourceType, perm.ResourceID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		if resource.Status != domain.ResourceActive {
			continue
		}
		snap.Grants = append(snap.Grants, policyGrant{
			ResourceID:    resource.ID,
			ResourceType:  string(resource.Type),
			Config:        safeConfigJSON(resource.EndpointConfig),
			Constraints:   safeConfigJSON(perm.Constraints),
			Ceiling:       safeConfigJSON(resource.PolicyCeiling),
			RiskTier:      resource.RiskTier,
			EgressClass:   resource.EgressClass,
			ToolsSnapshot: safeConfigJSON(resource.ToolsSnapshot),
			ToolsHash:     resource.ToolsHash,
		})
	}
	return snap, nil
}
