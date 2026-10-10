// TG-4 (S-250): BYO credential custody for governed REST resources.
//
// Admin registration accepts a write-only `auth` payload alongside the
// resource's endpoint_config; the secret material is stored as a
// control-plane-managed Kubernetes Secret (the same custody pattern as
// S-155 provider API keys) and the resource's auth_ref points at it.
// Secret values are never serialized by any admin GET — only the
// internal, network-gated credentials API serves them to the tool
// gateway, one resolution per governed call.
//
// Trust class: GET /internal/v1/credentials is identical to
// /internal/v1/policy (design §5.1.5 / TG-1) — no app-layer auth,
// reachability enforced by cluster NetworkPolicy (internal-only).
// Every access is audited WITHOUT secret values (kind + outcome only).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/egresspolicy"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
)

// errSecretStoreUnconfigured guards BYO writes when no K8s Secret
// store is wired (dev without a cluster): callers map it to 503.
var errSecretStoreUnconfigured = errors.New("kubernetes secret storage is not configured")

// ResourceSecretStore is the Secret backend for BYO REST resource
// credentials. The production implementation is kube.SecretStore;
// tests inject fakes.
type ResourceSecretStore interface {
	EnsureResourceSecret(ctx context.Context, name string, fields map[string]string) error
	GetResourceSecret(ctx context.Context, name string) (map[string]string, error)
	DeleteResourceSecret(ctx context.Context, name string) error
	// DeleteSecretsByPrefix sweeps every managed Secret whose name
	// equals prefix or starts with prefix+"-" (S-260: deleting a
	// resource must remove its per-agent custody Secrets,
	// skquad-rest-<id>-agent-<agent>, which no by-name API can find
	// after the resource row is gone). Returns the deleted count.
	DeleteSecretsByPrefix(ctx context.Context, prefix string) (int, error)
	RefFor(secretName string) string
}

// custodySecretName returns the resource-level custody Secret name for
// a typed resource: rest -> "skquad-rest-", git -> "skquad-git-",
// mcp -> "skquad-mcp-". The same prefix anchors the per-agent names
// (<prefix><id>-agent-<agent>), so it doubles as the S-260 sweep
// prefix that catches the resource Secret and all its per-agent
// derivatives in one pass.
func custodySecretName(resource *domain.RegistryResource) string {
	switch resource.Type {
	case domain.ResGit:
		return kube.GitSecretName(resource.ID)
	case domain.ResMCP:
		// TG-5 slice B2a: MCP custody under its own prefix, disjoint
		// from rest/git even if a resource id were reused across types.
		return kube.MCPSecretName(resource.ID)
	default:
		return kube.ResourceSecretName(resource.ID)
	}
}

// sweepResourceCustodySecrets removes the resource-level Secret and
// every per-agent Secret for a resource being deleted (S-260). Runs
// best-effort after the store delete succeeded: a sweep failure then
// orphans Secret material (logged, recoverable) rather than stripping
// credentials from a resource that is still live. Values are never
// read — the sweep works on names only, and the audit carries counts.
func (s *Server) sweepResourceCustodySecrets(r *http.Request, resource *domain.RegistryResource) {
	if s.resourceSecrets == nil || resource == nil {
		return
	}
	prefix := custodySecretName(resource)
	if prefix == "" {
		return
	}
	deleted, err := s.resourceSecrets.DeleteSecretsByPrefix(r.Context(), prefix)
	if err != nil {
		// Log without values; keep going so a partial sweep still
		// removes what it can. The error is surfaced in the audit.
		log.Printf("registry resource %s (%s): custody secret sweep error after %d deleted: %v", resource.ID, resource.Type, deleted, err)
	}
	if deleted == 0 && err == nil {
		return
	}
	metadata, _ := json.Marshal(map[string]string{
		"prefix": prefix,
		"count":  fmt.Sprintf("%d", deleted),
		"error":  fmt.Sprintf("%v", err != nil),
	})
	s.recordUserAudit(r, "registry.resource.secret_sweep", string(resource.Type), resource.ID, "", json.RawMessage(metadata))
}

// restAuthFields lists the allowed secret fields per auth kind. This is
// the registration mirror of the gateway credential contract
// (tool-gateway/internal/credentials.Secret):
//
//	bearer                    -> token
//	api_key_header            -> token (header name lives in endpoint_config)
//	basic                     -> username + password
//	oauth2_client_credentials -> client_id + client_secret + token_url
var restAuthFields = map[string][]string{
	"bearer":                    {"token"},
	"api_key_header":            {"token"},
	"basic":                     {"username", "password"},
	"oauth2_client_credentials": {"client_id", "client_secret", "token_url"},
	// TG-10: ssh BYO static key custody (auth_mode=static_key). CA mode
	// needs no per-resource secret — ephemeral certs are minted by the
	// terminal-service from the cluster-held SSH CA.
	"ssh_key": {"private_key_pem"},
}

// restAuthKindFromConfig extracts endpoint_config.auth_kind ("none" when
// unset/absent — an unauthenticated resource has no custody duty).
func restAuthKindFromConfig(endpointConfig json.RawMessage) string {
	if len(endpointConfig) == 0 {
		return "none"
	}
	var s struct {
		AuthKind string `json:"auth_kind"`
	}
	if err := json.Unmarshal(endpointConfig, &s); err != nil || s.AuthKind == "" {
		return "none"
	}
	return s.AuthKind
}

// gitAuthKind is the fixed credential kind for git resources
// (TG-4b): a bearer token — GitHub fine-grained PAT or App
// installation token. The git endpoint_config shape has no auth_kind
// key; custody is always bearer.
const gitAuthKind = "bearer"

// authKindForType resolves the custody credential kind for a typed
// resource: rest derives it from endpoint_config.auth_kind; git is
// always bearer. "none" for anything else.
func authKindForType(resource *domain.RegistryResource) string {
	if resource.Type == domain.ResGit {
		return gitAuthKind
	}
	if resource.Type == domain.ResSSH {
		return sshKeyAuthKind
	}
	return restAuthKindFromConfig(resource.EndpointConfig)
}

// sshKeyAuthKind is the fixed credential kind for TG-10 ssh resources
// (BYO static key; CA-mode resources carry no secret).
const sshKeyAuthKind = "ssh_key"

// parseRestAuth validates the write-only auth payload against the
// resource's auth_kind. Unknown fields, missing required fields and a
// non-URL token_url are violations. Violation messages never echo
// values.
func parseRestAuth(authKind string, raw json.RawMessage) (map[string]string, egresspolicy.Violations) {
	var v egresspolicy.Violations
	if authKind == "" || authKind == "none" {
		if !isEmptyJSONObject(raw) {
			v = append(v, egresspolicy.Violation{Field: "auth", Code: "invalid_value", Message: "auth payload is not allowed with auth_kind=none"})
		}
		return nil, v
	}
	allowed, ok := restAuthFields[authKind]
	if !ok {
		v = append(v, egresspolicy.Violation{Field: "auth", Code: "invalid_value", Message: "unknown auth_kind"})
		return nil, v
	}
	if isEmptyJSONObject(raw) {
		// No BYO payload: the resource may reference an operator-managed
		// Secret via auth_ref (pre-TG-4 flow). Nothing to validate here;
		// the gateway fails closed if nothing resolves at call time.
		return nil, nil
	}
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil {
		v = append(v, egresspolicy.Violation{Field: "auth", Code: "invalid_type", Message: "auth must be a JSON object of string fields"})
		return nil, v
	}
	for k := range fields {
		if !slices.Contains(allowed, k) {
			v = append(v, egresspolicy.Violation{Field: "auth." + k, Code: "unknown_field", Message: "field is not valid for auth_kind=" + authKind})
		}
	}
	for _, required := range allowed {
		if strings.TrimSpace(fields[required]) == "" {
			v = append(v, egresspolicy.Violation{Field: "auth." + required, Code: "required", Message: required + " is required for auth_kind=" + authKind})
		}
	}
	if authKind == "oauth2_client_credentials" && len(v) == 0 {
		u := fields["token_url"]
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			v = append(v, egresspolicy.Violation{Field: "auth.token_url", Code: "invalid_value", Message: "token_url must be an absolute http(s) URL"})
		}
	}
	if len(v) > 0 {
		return nil, v
	}
	return fields, nil
}

// setResourceSecret stores BYO secret fields in the managed Secret
// store and points the resource's auth_ref at it (the caller
// persists the resource). Git resources (TG-4b) use the
// "skquad-git-" custody prefix; everything else the REST prefix.
func (s *Server) setResourceSecret(ctx context.Context, resource *domain.RegistryResource, fields map[string]string) error {
	if s.resourceSecrets == nil {
		return errSecretStoreUnconfigured
	}
	secretName := custodySecretName(resource)
	if err := s.resourceSecrets.EnsureResourceSecret(ctx, secretName, fields); err != nil {
		return err
	}
	resource.AuthRef = s.resourceSecrets.RefFor(secretName)
	return nil
}

// clearResourceSecret deletes the managed Secret behind a managed
// auth_ref and blanks the ref. Operator-provided (non-managed) refs
// are only unlinked, never deleted.
func (s *Server) clearResourceSecret(ctx context.Context, resource *domain.RegistryResource) {
	if resource.AuthRef == "" {
		return
	}
	if s.resourceSecrets != nil && kube.IsManagedRef(resource.AuthRef) {
		name := resource.AuthRef[strings.LastIndex(resource.AuthRef, "/")+1:]
		if err := s.resourceSecrets.DeleteResourceSecret(ctx, name); err != nil {
			log.Printf("rest resource %s: delete managed secret: %v", resource.ID, err)
		}
	}
	resource.AuthRef = ""
}

// internalCredentials — GET /internal/v1/credentials?resource=<id>[&agent=<id>]
//
// The tool gateway's per-call secret resolution (TG-4, per-agent
// extension TG-4c / S-259). Serves exactly the shape the gateway
// credentials client expects:
//
//	{"resource_id": "...", "kind": "...", "fields": {...}, "scope": "agent|resource"}
//
// Resolution order (S-259): when an agent is supplied, the agent's OWN
// per-(resource,agent) Secret wins if present; otherwise the
// resource-level default Secret (auth_ref) is served. A read error on
// the per-agent Secret falls back to the resource default (the default
// is a legitimate credential for this resource; a hard store failure
// still 503s below). When no agent is supplied only the resource
// default resolves — the backwards-compatible pre-TG-4c path.
//
// Unknown / wrong-type / deprecated resources collapse to a uniform
// 404 (no existence/type leakage beyond id knowledge).
// Responses are Cache-Control: no-store — the gateway resolves per
// call so revocation is immediate.
func (s *Server) internalCredentials(w http.ResponseWriter, r *http.Request) {
	resourceID := strings.TrimSpace(r.URL.Query().Get("resource"))
	if resourceID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "resource query parameter is required")
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
	resource, err := s.store.GetResourceByID(r.Context(), resourceID)
	// TG-4b: git resources join rest in BYO custody (bearer PAT).
	// TG-5 slice B2a: mcp resources join with bearer custody too — the
	// gateway resolves the per-call bearer for mcp_call through this
	// same endpoint (agent-scoped when supplied).
	if err != nil || (resource.Type != domain.ResRest && resource.Type != domain.ResGit && resource.Type != domain.ResMCP && resource.Type != domain.ResSSH) || resource.Status != domain.ResourceActive {
		s.auditCredentialAccess(r, "", resourceID, agentID, "", "denied")
		writeError(w, http.StatusNotFound, "not_found", "no credential for resource")
		return
	}
	resType := string(resource.Type)
	kind := authKindForType(resource)
	if kind == "none" {
		s.auditCredentialAccess(r, resType, resourceID, agentID, kind, "denied")
		writeError(w, http.StatusNotFound, "not_found", "no credential for resource")
		return
	}
	if s.resourceSecrets == nil {
		s.auditCredentialAccess(r, resType, resourceID, agentID, kind, "unavailable")
		writeError(w, http.StatusServiceUnavailable, "credentials_unavailable", "secret store not configured")
		return
	}
	// Per-agent credential first (TG-4c isolation guarantee): the
	// fields served for (resource, agent) are that agent's own secret
	// whenever one exists — never another agent's. Git per-agent
	// custody (TG-4b) uses the same resolution with the git prefix.
	if agentID != "" {
		agentSecretName := kube.ResourceAgentSecretName(resourceID, agentID)
		switch resource.Type {
		case domain.ResGit:
			agentSecretName = kube.GitAgentSecretName(resourceID, agentID)
		case domain.ResMCP:
			agentSecretName = kube.MCPAgentSecretName(resourceID, agentID)
		}
		if name := agentSecretName; name != "" {
			fields, err := s.resourceSecrets.GetResourceSecret(r.Context(), name)
			if err == nil && len(fields) > 0 && restFieldsMatchKind(kind, fields) {
				s.serveCredential(w, r, resType, resourceID, agentID, kind, "agent", fields)
				return
			}
			if err == nil && len(fields) > 0 {
				// Stale payload (e.g. auth_kind rotated on the resource
				// after the per-agent secret was written): refuse rather
				// than inject the wrong shape; fall back to the default.
				log.Printf("credentials: per-agent secret %s inconsistent with auth_kind, falling back to resource default", name)
			}
			if err != nil {
				// Per-agent read failed: fall back to the resource
				// default. Log without values; a store-wide failure
				// surfaces as 503 on the default read below.
				log.Printf("credentials: per-agent secret %s unreadable, falling back to resource default: %v", name, err)
			}
		}
	}
	if resource.AuthRef == "" {
		s.auditCredentialAccess(r, resType, resourceID, agentID, kind, "denied")
		writeError(w, http.StatusNotFound, "not_found", "no credential for resource")
		return
	}
	name := resource.AuthRef[strings.LastIndex(resource.AuthRef, "/")+1:]
	fields, err := s.resourceSecrets.GetResourceSecret(r.Context(), name)
	if err != nil || len(fields) == 0 {
		s.auditCredentialAccess(r, resType, resourceID, agentID, kind, "unavailable")
		writeError(w, http.StatusServiceUnavailable, "credentials_unavailable", "credential could not be resolved")
		return
	}
	s.serveCredential(w, r, resType, resourceID, agentID, kind, "resource", fields)
}

// serveCredential writes a resolved credential response and audits
// the serve. Values go to the response body only — never the audit.
func (s *Server) serveCredential(w http.ResponseWriter, r *http.Request, resType, resourceID, agentID, kind, scope string, fields map[string]string) {
	s.auditCredentialAccess(r, resType, resourceID, agentID, kind, "served")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"resource_id": resourceID,
		"kind":        kind,
		"fields":      fields,
		"scope":       scope,
	})
}

// auditCredentialAccess records one credentials-API access with NO
// secret values — resource id, agent id, auth kind, resolution scope
// and outcome only.
func (s *Server) auditCredentialAccess(r *http.Request, resType, resourceID, agentID, kind, outcome string) {
	metadata, _ := json.Marshal(map[string]string{
		"kind": kind, "outcome": outcome, "agent": agentID,
	})
	entry := &domain.AuditEntry{
		ActorType:    "system",
		ActorID:      "tool-gateway",
		Action:       "credentials.access",
		ResourceType: resType,
		ResourceID:   resourceID,
		Metadata:     metadata,
		Timestamp:    time.Now().UTC(),
	}
	if err := s.store.RecordAudit(r.Context(), entry); err != nil {
		log.Printf("credentials audit failed resource=%s: %v", resourceID, err)
	}
}
