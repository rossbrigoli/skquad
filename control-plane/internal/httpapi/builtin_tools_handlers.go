package httpapi

// BT-2 — built-in platform tools API (ADR-0012 §2).
//
// Admin surface (platform_admin only, same IdP-group role binding as the
// rest of the admin surface): list + merge-patch tool config; every
// successful PATCH carries a same-transaction audit entry with the
// acting admin (S-86 pending-audit pattern).
//
// Agent surface (agent-credential auth): the tools view with
// ETag/If-None-Match mirroring the composed-prompt caching pattern
// (ADR-0011 D4), and the web_search proxy so provider API keys never
// leave the control plane.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/search"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	defaultSearchTimeoutSeconds = 20
	defaultSearchMaxResults     = 8
	searchMaxResultsCap         = 20
	toolsViewTimeLayout         = "2006-01-02T15:04:05Z07:00"
)

// builtinToolAdminView is the admin-facing full config (contract keys
// camelCase per ADR-0012 §2).
type builtinToolAdminView struct {
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Policy    json.RawMessage `json:"policy"`
	UpdatedAt string          `json:"updatedAt"`
	UpdatedBy string          `json:"updatedBy"`
}

// builtinToolAgentView is the runtime-facing config slice — never any
// secrets (keys are not part of the stored row to begin with).
type builtinToolAgentView struct {
	Name    string          `json:"name"`
	Enabled bool            `json:"enabled"`
	Policy  json.RawMessage `json:"policy"`
}

func normalizePolicy(p json.RawMessage) json.RawMessage {
	if len(p) == 0 {
		return json.RawMessage(`{}`)
	}
	return p
}

func builtinToolAdminViewOf(t *domain.BuiltinToolConfig) builtinToolAdminView {
	return builtinToolAdminView{
		Name:      t.Name,
		Enabled:   t.Enabled,
		Policy:    normalizePolicy(t.Policy),
		UpdatedAt: t.UpdatedAt.UTC().Format(toolsViewTimeLayout),
		UpdatedBy: t.UpdatedBy,
	}
}

// --- Admin endpoints ------------------------------------------------------

// GET /api/v1/admin/tools
func (s *Server) listBuiltinToolsAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	tools, err := s.store.ListBuiltinTools(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	views := make([]builtinToolAdminView, 0, len(tools))
	for _, t := range tools {
		views = append(views, builtinToolAdminViewOf(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": views})
}

// PATCH /api/v1/admin/tools/{name} — merge semantics: absent fields
// keep their stored values. Policy is validated server-side before the
// write; unknown keys → 400 with a clear message (ADR-0012 §1/§3).
func (s *Server) patchBuiltinTool(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	name := chi.URLParam(r, "name")
	if !domain.IsBuiltinToolName(name) {
		writeError(w, http.StatusNotFound, "not_found", "unknown built-in tool: "+name)
		return
	}
	var req struct {
		Enabled *bool           `json:"enabled"`
		Policy  json.RawMessage `json:"policy"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil && len(req.Policy) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "at least one of enabled or policy must be provided")
		return
	}
	if len(req.Policy) > 0 {
		if errs := domain.ValidateBuiltinPolicy(name, req.Policy); len(errs) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_policy", strings.Join(errs, "; "))
			return
		}
	}
	actor := currentUser(r.Context())
	metadata, _ := json.Marshal(map[string]any{"enabled": req.Enabled, "policy": req.Policy})
	updated, err := s.store.UpdateBuiltinTool(
		s.pendingUserAuditCtx(r, "builtin_tools.update", "builtin_tools", name, "", metadata),
		name, req.Enabled, req.Policy, actor.ID,
	)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	// Audit log line with the actor on every successful PATCH (ADR-0012 §2).
	log.Printf("audit builtin_tools.update actor=%s tool=%s enabled=%t", actor.ID, name, updated.Enabled)
	writeJSON(w, http.StatusOK, builtinToolAdminViewOf(updated))
}

// --- Agent-facing config --------------------------------------------------

// GET /api/v1/agents/me/tools — agent-credential auth (reuses the
// /agents/me middleware group). ETag = sha256 of the response body,
// mirroring the composed-prompt pattern: the runtime fetches at wake
// and gets cheap 304s when nothing changed.
func (s *Server) getMyBuiltinTools(w http.ResponseWriter, r *http.Request) {
	tools, err := s.store.ListBuiltinTools(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	views := make([]builtinToolAgentView, 0, len(tools))
	for _, t := range tools {
		views = append(views, builtinToolAgentView{Name: t.Name, Enabled: t.Enabled, Policy: normalizePolicy(t.Policy)})
	}
	body, err := json.Marshal(map[string]any{"tools": views})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to encode tools config")
		return
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	w.Header().Set("ETag", `"`+digest+`"`)
	if ifNoneMatchMatches(r.Header.Get("If-None-Match"), digest) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// --- Search proxy ----------------------------------------------------------

// POST /api/v1/tools/web_search — agent-credential auth. The provider
// API key lives only in the control plane; the runtime gets proxied
// results and never sees the key (ADR-0012 §2/§3).
func (s *Server) agentWebSearch(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	cfg, err := s.store.GetBuiltinTool(r.Context(), domain.BuiltinToolWebSearch)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		writeStorageError(w, err)
		return
	}
	if err != nil || cfg == nil || !cfg.Enabled {
		writeError(w, http.StatusForbidden, "tool_disabled", "web_search is disabled by platform policy")
		return
	}

	var req struct {
		Query      string `json:"query"`
		MaxResults int    `json:"maxResults"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "query is required")
		return
	}
	if req.MaxResults < 0 || req.MaxResults > searchMaxResultsCap {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("maxResults must be between 1 and %d", searchMaxResultsCap))
		return
	}

	providerName, timeout := s.webSearchPolicy(cfg)
	maxResults := defaultSearchMaxResults
	if policyMax := policyInt(cfg.Policy, "maxResults"); policyMax > 0 {
		maxResults = policyMax
	}
	if req.MaxResults > 0 {
		maxResults = req.MaxResults
	}

	provider, ok := s.searchProviders[providerName]
	if !ok {
		writeError(w, http.StatusBadGateway, "search_provider_unavailable",
			fmt.Sprintf("search provider %q is not configured on this control plane (missing API key)", providerName))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	results, err := provider.Search(ctx, req.Query, maxResults)
	if err != nil {
		log.Printf("web_search proxy error agent=%s provider=%s: %v", principal.Agent.ID, providerName, err)
		writeError(w, http.StatusBadGateway, "search_provider_error", "search provider request failed")
		return
	}
	if results == nil {
		results = []search.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// webSearchPolicy reads provider + timeout from the validated policy
// row, falling back to the ADR defaults. Validation happened at write
// time, so malformed values here indicate corruption: defaults win.
func (s *Server) webSearchPolicy(cfg *domain.BuiltinToolConfig) (string, time.Duration) {
	providerName := "duckduckgo"
	timeout := defaultSearchTimeoutSeconds * time.Second
	if len(cfg.Policy) > 0 {
		var p struct {
			Provider       string `json:"provider"`
			TimeoutSeconds int    `json:"timeoutSeconds"`
		}
		if err := json.Unmarshal(cfg.Policy, &p); err == nil {
			if domain.IsSearchProvider(p.Provider) {
				providerName = p.Provider
			}
			if p.TimeoutSeconds > 0 {
				timeout = time.Duration(p.TimeoutSeconds) * time.Second
			}
		}
	}
	return providerName, timeout
}

func policyInt(policy json.RawMessage, key string) int {
	if len(policy) == 0 {
		return 0
	}
	var p map[string]any
	if err := json.Unmarshal(policy, &p); err != nil {
		return 0
	}
	if n, ok := p[key].(float64); ok && n > 0 {
		return int(n)
	}
	return 0
}
