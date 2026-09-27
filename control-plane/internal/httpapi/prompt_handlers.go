package httpapi

// S-PROMPT WP2 — prompt tier APIs (ADR-0011, plan §3).
//
// Every save path runs the same battery before writing: reserved-delimiter
// sanitize → template-var allowlist → per-tier token budget. Saves append a
// prompt_revisions row in the same transaction as the entity update (via
// the storage revision-intent context, mirroring the S-86 audit pattern).
// Nothing beyond token counts and hashes is logged; prompt content only ever
// flows to principals the tier visibility rules allow.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/promptcompo"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// promptFailure is a structured draft-validation failure mapped to HTTP 400.
type promptFailure struct {
	code    string
	message string
	details map[string]any
}

// tierNameForScope maps a stored prompt scope to its composer tier.
func tierNameForScope(scope string) promptcompo.TierName {
	switch scope {
	case domain.PromptScopeSquad:
		return promptcompo.TierSquad
	case domain.PromptScopeAgent:
		return promptcompo.TierAgent
	default:
		return promptcompo.TierOrganization
	}
}

// checkPromptDraft runs the save-time battery for one tier's draft content:
// sanitize (reserved delimiters), template-var allowlist, token budget.
// Returns the draft token count and soft-limit warnings, or a structured
// failure. Pure: never writes anything.
func checkPromptDraft(scope promptcompo.TierName, content string) (int, []string, *promptFailure) {
	if err := promptcompo.Sanitize(content); err != nil {
		return 0, nil, &promptFailure{
			code:    "prompt_contains_reserved_tokens",
			message: "prompt contains reserved <skquad_ block delimiters",
		}
	}
	if unknown := promptcompo.ValidateTemplateVars(content); len(unknown) > 0 {
		return 0, nil, &promptFailure{
			code:    "prompt_unknown_template_vars",
			message: "unknown template variable(s): " + strings.Join(unknown, ", "),
		}
	}
	caps := promptcompo.CapsFromEnv()[scope]
	tokens := promptcompo.EstimateTokens(content)
	if tokens > caps.Hard {
		return tokens, nil, &promptFailure{
			code:    "prompt_token_cap_exceeded",
			message: fmt.Sprintf("%s prompt has %d tokens, hard cap is %d", scope, tokens, caps.Hard),
			details: map[string]any{
				"scope":     string(scope),
				"tokens":    tokens,
				"soft_warn": caps.Soft,
				"hard_cap":  caps.Hard,
			},
		}
	}
	var warnings []string
	if tokens > caps.Soft {
		warnings = append(warnings, fmt.Sprintf("%s prompt: %d tokens exceeds soft limit %d", scope, tokens, caps.Soft))
	}
	return tokens, warnings, nil
}

// writePromptFailure emits the standard error envelope, extended with the
// token report fields when the failure carries them.
func writePromptFailure(w http.ResponseWriter, f *promptFailure) {
	body := map[string]any{"code": f.code, "message": f.message}
	for k, v := range f.details {
		body[k] = v
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": body})
}

// --- Organization tier (platform-admin only) ------------------------------

type orgPromptResponse struct {
	OrgName   string   `json:"org_name"`
	OrgPrompt string   `json:"org_prompt"`
	Tokens    int      `json:"tokens"`
	SoftWarn  int      `json:"soft_warn"`
	HardCap   int      `json:"hard_cap"`
	Warnings  []string `json:"warnings,omitempty"`
	UpdatedAt string   `json:"updated_at"`
	UpdatedBy string   `json:"updated_by"`
}

func (s *Server) orgPromptView(settings *domain.InstanceSettings) orgPromptResponse {
	caps := promptcompo.CapsFromEnv()[promptcompo.TierOrganization]
	tokens := promptcompo.EstimateTokens(settings.OrgPrompt)
	var warnings []string
	if tokens > caps.Soft {
		warnings = append(warnings, fmt.Sprintf("organization prompt: %d tokens exceeds soft limit %d", tokens, caps.Soft))
	}
	return orgPromptResponse{
		OrgName:   settings.OrgName,
		OrgPrompt: settings.OrgPrompt,
		Tokens:    tokens,
		SoftWarn:  caps.Soft,
		HardCap:   caps.Hard,
		Warnings:  warnings,
		UpdatedAt: settings.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedBy: settings.UpdatedBy,
	}
}

func (s *Server) getOrgPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	settings, err := s.store.GetInstanceSettings(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.orgPromptView(settings))
}

func (s *Server) putOrgPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req struct {
		OrgName   *string `json:"org_name"`
		OrgPrompt *string `json:"org_prompt"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OrgPrompt == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "org_prompt is required")
		return
	}
	content := strings.TrimSpace(*req.OrgPrompt)
	if _, _, failure := checkPromptDraft(promptcompo.TierOrganization, content); failure != nil {
		writePromptFailure(w, failure)
		return
	}
	current, err := s.store.GetInstanceSettings(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	settings := *current
	settings.OrgPrompt = content
	if req.OrgName != nil {
		settings.OrgName = strings.TrimSpace(*req.OrgName)
	}
	settings.UpdatedBy = currentUser(r.Context()).ID

	ctx := s.pendingUserAuditCtx(r, "settings.org_prompt.update", "instance_settings", "", "", nil)
	ctx = storage.WithPromptRevision(ctx, storage.PromptRevisionIntent{
		Scope:   domain.PromptScopeOrganization,
		ScopeID: "",
		SavedBy: settings.UpdatedBy,
	})
	updated, err := s.store.UpdateInstanceSettings(ctx, &settings)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.orgPromptView(updated))
}

// --- Composition ----------------------------------------------------------

type promptTierView struct {
	Name     string `json:"name"`
	Tokens   int    `json:"tokens"`
	SoftWarn int    `json:"soft_warn"`
	HardCap  int    `json:"hard_cap"`
	Content  string `json:"content,omitempty"`
}

type composedPromptResponse struct {
	Prompt    string           `json:"prompt,omitempty"`
	SHA256    string           `json:"sha256"`
	TotalToks int              `json:"total_tokens"`
	Warnings  []string         `json:"warnings,omitempty"`
	Tiers     []promptTierView `json:"tiers"`
}

func compositionView(comp promptcompo.Composition, includeTierContent bool) composedPromptResponse {
	resp := composedPromptResponse{
		SHA256:    comp.SHA256,
		TotalToks: comp.TotalToks,
		Warnings:  comp.Warnings,
		Tiers:     make([]promptTierView, 0, len(comp.Tiers)),
	}
	for _, tier := range comp.Tiers {
		view := promptTierView{Name: string(tier.Name), Tokens: tier.Tokens, SoftWarn: tier.SoftWarn, HardCap: tier.HardCap}
		if includeTierContent {
			view.Content = tier.Content
		}
		resp.Tiers = append(resp.Tiers, view)
	}
	return resp
}

// composePromptForAgent gathers the tier texts and facts for one agent and
// runs the WP1 composer. Reads are cheap (settings + squad + roster +
// grants) and happen per request; the ETag keeps repeat fetches at 304.
func (s *Server) composePromptForAgent(ctx context.Context, agent *domain.Agent) (promptcompo.Composition, error) {
	settings, err := s.store.GetInstanceSettings(ctx)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return promptcompo.Composition{}, err
	}
	var orgPrompt string
	if settings != nil {
		orgPrompt = settings.OrgPrompt
	}
	squad, err := s.store.GetSquad(ctx, agent.SquadID)
	if err != nil {
		return promptcompo.Composition{}, err
	}
	squadAgents, err := s.store.ListAgents(ctx, agent.SquadID)
	if err != nil {
		return promptcompo.Composition{}, err
	}
	roster := make([]string, 0, len(squadAgents))
	for _, member := range squadAgents {
		if strings.TrimSpace(member.Role) != "" {
			roster = append(roster, member.Name+" ("+member.Role+")")
		} else {
			roster = append(roster, member.Name)
		}
	}
	granted, err := s.currentAgentResources(ctx, agent.ID)
	if err != nil {
		return promptcompo.Composition{}, err
	}
	resourceLines := make([]string, 0, len(granted))
	for _, res := range granted {
		line := res.Name + " [" + string(res.ResourceType) + "]"
		if strings.TrimSpace(res.Description) != "" {
			line += ": " + res.Description
		}
		resourceLines = append(resourceLines, line)
	}
	facts := promptcompo.Facts{
		AgentName:   agent.Name,
		AgentRole:   agent.Role,
		SquadName:   squad.Name,
		SquadRoster: roster,
		Resources:   resourceLines,
		Workspace:   squad.Namespace,
		PlatformVer: s.cfg.APIServerVersion,
	}
	// Platform override is a deploy-time operator concern (Helm-rendered
	// file); WP2 serves the embedded platform prompt.
	return promptcompo.Compose("", orgPrompt, squad.Prompt, agent.SystemPrompt, facts)
}

// ifNoneMatchMatches reports whether an If-None-Match header contains the
// given composition sha. Weak validators (W/"...") are accepted; "*" is
// deliberately NOT treated as a match — the response is served fresh.
func ifNoneMatchMatches(headerValue, sha string) bool {
	if headerValue == "" {
		return false
	}
	for _, candidate := range strings.Split(headerValue, ",") {
		c := strings.TrimSpace(candidate)
		c = strings.TrimPrefix(c, "W/")
		c = strings.Trim(c, `"`)
		if c == sha {
			return true
		}
	}
	return false
}

// GET /api/v1/agents/me/prompt — agent-credential auth (reuses the
// /agents/me middleware group). ETag = composition sha256.
func (s *Server) getMyComposedPrompt(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	comp, err := s.composePromptForAgent(r.Context(), principal.Agent)
	if err != nil {
		// Stored tiers are validated at save time; a compose failure means
		// corrupted state. Fail closed — never serve a partial prompt.
		writeError(w, http.StatusInternalServerError, "prompt_composition_failed", "failed to compose the effective prompt")
		return
	}
	w.Header().Set("ETag", `"`+comp.SHA256+`"`)
	if ifNoneMatchMatches(r.Header.Get("If-None-Match"), comp.SHA256) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	resp := compositionView(comp, false)
	resp.Prompt = comp.Prompt
	writeJSON(w, http.StatusOK, resp)
}

// GET /api/v1/prompt/effective?agent_id= — owner/admin preview with
// per-tier blocks, token counts, warnings and the composed sha.
func (s *Server) getEffectivePrompt(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent_id query parameter is required")
		return
	}
	agent, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if _, ok := s.ensureOwnedOrAdminSquad(w, r, agent.SquadID); !ok {
		return
	}
	comp, err := s.composePromptForAgent(r.Context(), agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prompt_composition_failed", "failed to compose the effective prompt")
		return
	}
	resp := compositionView(comp, true)
	resp.Prompt = comp.Prompt
	writeJSON(w, http.StatusOK, resp)
}

// GET /api/v1/prompt/revisions?scope=&scope_id= — tier-appropriate auth:
// admin sees all tiers; squad owners see their squad; agent revisions
// require the agent's squad owner (or admin). Content is included:
// requesters already hold read rights over the tier.
func (s *Server) listPromptRevisions(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	scopeID := strings.TrimSpace(r.URL.Query().Get("scope_id"))
	if !domain.ValidPromptScope(scope) {
		writeError(w, http.StatusBadRequest, "bad_request", "scope must be one of: organization, squad, agent")
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}

	switch scope {
	case domain.PromptScopeOrganization:
		if !s.requirePlatformAdmin(w, r) {
			return
		}
		scopeID = "" // the organization tier is keyed by the empty scope_id
	case domain.PromptScopeSquad:
		if scopeID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "scope_id is required for squad revisions")
			return
		}
		if _, ok := s.ensureOwnedOrAdminSquad(w, r, scopeID); !ok {
			return
		}
	case domain.PromptScopeAgent:
		if scopeID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "scope_id is required for agent revisions")
			return
		}
		agent, err := s.store.GetAgent(r.Context(), scopeID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if _, ok := s.ensureOwnedOrAdminSquad(w, r, agent.SquadID); !ok {
			return
		}
	}

	revisions, err := s.store.ListPromptRevisions(r.Context(), scope, scopeID, limit)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": revisions})
}

// POST /api/v1/prompt/validate — dry-run of the save-time battery for a
// draft {scope, content}. Returns token count, caps, warnings and any
// validation error. Never writes anything.
func (s *Server) validatePrompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope   string `json:"scope"`
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !domain.ValidPromptScope(req.Scope) {
		writeError(w, http.StatusBadRequest, "bad_request", "scope must be one of: organization, squad, agent")
		return
	}
	tier := tierNameForScope(req.Scope)
	tokens, warnings, failure := checkPromptDraft(tier, req.Content)
	caps := promptcompo.CapsFromEnv()[tier]
	resp := map[string]any{
		"valid":     failure == nil,
		"scope":     req.Scope,
		"tokens":    tokens,
		"soft_warn": caps.Soft,
		"hard_cap":  caps.Hard,
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	if failure != nil {
		resp["error"] = map[string]string{"code": failure.code, "message": failure.message}
	}
	writeJSON(w, http.StatusOK, resp)
}
