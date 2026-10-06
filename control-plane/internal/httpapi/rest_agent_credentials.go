// TG-4c (S-259): per-agent BYO REST credentials.
//
// A credential for a governed REST resource can be scoped to ONE agent:
// the agent's own per-(resource, agent) Secret wins at gateway
// resolution; agents without one fall back to the resource-level
// default (TG-4 custody). These owner/admin write APIs set, probe and
// clear the per-agent Secret. Like the resource-level BYO flow, the
// payload is WRITE-ONLY: no GET anywhere serializes the values — the
// status probe reports existence and kind only.
//
// Authorization mirrors the grant endpoints' ownership posture:
// platform admins act on any agent; everyone else must own the agent's
// squad. The per-agent credential's field set must match the
// resource's auth_kind (restAuthFields), so a kind rotation on the
// resource invalidates stale per-agent payloads at resolution time
// (fall back to default rather than inject the wrong shape).
package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
)

// restFieldsMatchKind reports whether a stored field set is consistent
// with the resource's auth kind: every required field present and no
// foreign fields. Used at resolution to refuse stale per-agent
// payloads after an auth_kind rotation on the resource.
func restFieldsMatchKind(kind string, fields map[string]string) bool {
	allowed, ok := restAuthFields[kind]
	if !ok {
		return false
	}
	for _, req := range allowed {
		if strings.TrimSpace(fields[req]) == "" {
			return false
		}
	}
	for k := range fields {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

// restAgentCredentialSetup validates the request context shared by the
// set / status / delete handlers: rest resource (active), existing
// agent, credentialed auth_kind, and caller authorization (platform
// admin or the agent's squad owner — the grant-endpoint posture).
// On failure it has already written the HTTP error.
func (s *Server) restAgentCredentialSetup(w http.ResponseWriter, r *http.Request) (*domain.RegistryResource, *domain.Agent, string, bool) {
	typ, ok := registryTypeFromRequest(w, r)
	if !ok {
		return nil, nil, "", false
	}
	if typ != domain.ResRest {
		writeError(w, http.StatusBadRequest, "bad_request", "per-agent credentials are only valid for rest resources")
		return nil, nil, "", false
	}
	resource, err := s.store.GetResource(r.Context(), typ, chi.URLParam(r, "resourceID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, nil, "", false
	}
	if resource.Status != domain.ResourceActive {
		writeError(w, http.StatusNotFound, "not_found", "resource is not active")
		return nil, nil, "", false
	}
	agentID := strings.TrimSpace(chi.URLParam(r, "agentID"))
	agent, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil {
		writeStorageError(w, err)
		return nil, nil, "", false
	}
	if _, ok := s.ensureOwnedOrAdminSquad(w, r, agent.SquadID); !ok {
		return nil, nil, "", false
	}
	kind := restAuthKindFromConfig(resource.EndpointConfig)
	if kind == "none" {
		writeError(w, http.StatusBadRequest, "bad_request", "resource has no credentialed auth_kind; per-agent credentials require bearer, api_key_header, basic or oauth2_client_credentials")
		return nil, nil, "", false
	}
	name := kube.ResourceAgentSecretName(resource.ID, agent.ID)
	if name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent id is not usable for secret naming")
		return nil, nil, "", false
	}
	return resource, agent, name, true
}

// putRestAgentCredential — PUT /api/v1/registry/rest/{resourceID}/agent-credentials/{agentID}
//
// Body: {"auth": {<kind-specific fields>}}. Write-only: values go to
// the Secret store and nowhere else; the response confirms the set
// without echoing anything.
func (s *Server) putRestAgentCredential(w http.ResponseWriter, r *http.Request) {
	resource, agent, secretName, ok := s.restAgentCredentialSetup(w, r)
	if !ok {
		return
	}
	if s.resourceSecrets == nil {
		writeError(w, http.StatusServiceUnavailable, "secret_store_unavailable", "BYO credentials require Kubernetes secret storage")
		return
	}
	var req struct {
		Auth json.RawMessage `json:"auth"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	kind := restAuthKindFromConfig(resource.EndpointConfig)
	fields, v := parseRestAuth(kind, req.Auth)
	if len(v) > 0 {
		writeViolations(w, "invalid_auth_payload", "auth payload failed validation", v)
		return
	}
	if len(fields) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "auth fields are required to set a per-agent credential")
		return
	}
	if err := s.resourceSecrets.EnsureResourceSecret(r.Context(), secretName, fields); err != nil {
		writeError(w, http.StatusBadGateway, "resource_secret_store_failed", "could not store the per-agent credential as a Kubernetes Secret")
		return
	}
	s.recordUserAudit(r, "rest.credential.agent_set", string(domain.ResRest), resource.ID, agent.SquadID,
		json.RawMessage(mustJSON(map[string]string{"agent_id": agent.ID, "kind": kind})))
	writeJSON(w, http.StatusOK, map[string]any{
		"resource_id": resource.ID,
		"agent_id":    agent.ID,
		"auth_kind":   kind,
		"source":      "agent",
	})
}

// getRestAgentCredentialStatus — GET /api/v1/registry/rest/{resourceID}/agent-credentials/{agentID}
//
// Existence probe for the UI's "own credential vs resource default"
// indicator. NEVER returns values — only whether the agent has its own
// credential and the resource's auth kind.
func (s *Server) getRestAgentCredentialStatus(w http.ResponseWriter, r *http.Request) {
	resource, agent, secretName, ok := s.restAgentCredentialSetup(w, r)
	if !ok {
		return
	}
	hasOwn := false
	if s.resourceSecrets != nil {
		if fields, err := s.resourceSecrets.GetResourceSecret(r.Context(), secretName); err == nil && restFieldsMatchKind(restAuthKindFromConfig(resource.EndpointConfig), fields) {
			hasOwn = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource_id":        resource.ID,
		"agent_id":           agent.ID,
		"auth_kind":          restAuthKindFromConfig(resource.EndpointConfig),
		"has_own_credential": hasOwn,
		"source":             map[bool]string{true: "agent", false: "resource_default"}[hasOwn],
	})
}

// deleteRestAgentCredential — DELETE /api/v1/registry/rest/{resourceID}/agent-credentials/{agentID}
//
// Removes the agent's own credential; subsequent calls fall back to
// the resource default (or 404 at the gateway if there is none).
// Deleting an absent per-agent credential is a no-op success.
func (s *Server) deleteRestAgentCredential(w http.ResponseWriter, r *http.Request) {
	resource, agent, secretName, ok := s.restAgentCredentialSetup(w, r)
	if !ok {
		return
	}
	if s.resourceSecrets != nil {
		if err := s.resourceSecrets.DeleteResourceSecret(r.Context(), secretName); err != nil {
			writeError(w, http.StatusBadGateway, "resource_secret_store_failed", "could not delete the per-agent credential Secret")
			return
		}
	}
	s.recordUserAudit(r, "rest.credential.agent_delete", string(domain.ResRest), resource.ID, agent.SquadID,
		json.RawMessage(mustJSON(map[string]string{"agent_id": agent.ID})))
	w.WriteHeader(http.StatusNoContent)
}

// mustJSON marshals m, returning "{}" on failure (metadata-only
// payloads; a marshal failure here must not fail the request).
func mustJSON(m map[string]string) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}
