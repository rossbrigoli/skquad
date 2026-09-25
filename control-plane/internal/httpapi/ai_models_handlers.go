// AI Model CRUD + user-level model grant handlers (ADR-0010, S-107).
//
// AI Models are the grantable unit (D1); grants follow users (D3). All
// admin surfaces here mirror the existing LLM-provider registry handler
// patterns (auth helpers, audit-ctx, error envelopes). Revoke/deprecate/
// delete cascades converge virtual keys via model_cascade.go (WP4, D9).

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// aiModelPricingRates are the four mandatory per-1M-token rates inside the
// pricing object (ADR-0010 D8). All must be present, numeric, non-negative.
var aiModelPricingRates = []string{
	"input_per_1m",
	"cached_input_per_1m",
	"cache_write_per_1m",
	"output_per_1m",
}

// aiModelUsageEntry describes one holder of a live reference to an AI
// Model being deleted: a user holding a grant (agent fields empty) or an
// agent bound to it (primary or fallback, with the owner identified).
// Mirrors the S-103 delete-with-in-use-warning shape so the UI reuses it.
type aiModelUsageEntry struct {
	UserID    string `json:"user_id"`
	UserEmail string `json:"user_email"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	SquadID   string `json:"squad_id"`
	// Slot names which binding references the model ("primary" or
	// "fallback"); empty for grant-only entries. Additive to the S-103
	// shape so the WP6 dialog renders it unchanged.
	Slot string `json:"slot,omitempty"`
}

// validateAIModelPricing enforces the four-rate contract. Returns a
// field-named message on the first violation.
func validateAIModelPricing(pricing json.RawMessage) (string, bool) {
	if len(pricing) == 0 {
		return "pricing is required", false
	}
	var rates map[string]json.RawMessage
	if err := json.Unmarshal(pricing, &rates); err != nil || rates == nil {
		return "pricing must be a JSON object", false
	}
	for _, rate := range aiModelPricingRates {
		raw, ok := rates[rate]
		if !ok || string(raw) == "null" {
			return rate + " is required in pricing", false
		}
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return rate + " in pricing must be numeric", false
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return rate + " in pricing must be finite", false
		}
		if value < 0 {
			return rate + " in pricing must be non-negative", false
		}
	}
	return "", true
}

func validateLongContextThreshold(value *int) (string, bool) {
	if value != nil && *value < 0 {
		return "long_context_threshold_tokens must be non-negative", false
	}
	return "", true
}

// ensureAIModelProviderExists validates the internal provider credential
// reference (D2). Providers have no new public CRUD; the reference must
// point at an already-registered provider.
func (s *Server) ensureAIModelProviderExists(w http.ResponseWriter, r *http.Request, providerID string) bool {
	if _, err := s.store.GetLLMProvider(r.Context(), providerID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "bad_request", "provider_id must reference an existing provider")
			return false
		}
		writeStorageError(w, err)
		return false
	}
	return true
}

// isDuplicateAIModelName reports an existing model sharing the
// (provider_id, model_name) pair, excluding excludeID (for PATCH).
func (s *Server) isDuplicateAIModelName(ctx context.Context, providerID, modelName, excludeID string) (bool, error) {
	models, err := s.store.ListAIModels(ctx, "")
	if err != nil {
		return false, err
	}
	for _, m := range models {
		if m.ID != excludeID && m.ProviderID == providerID && m.ModelName == modelName {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) createAIModel(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req struct {
		ProviderID                 string          `json:"provider_id"`
		DisplayName                string          `json:"display_name"`
		ModelName                  string          `json:"model_name"`
		ContextWindow              int             `json:"context_window"`
		SupportsTools              bool            `json:"supports_tools"`
		Pricing                    json.RawMessage `json:"pricing"`
		LongContextThresholdTokens *int            `json:"long_context_threshold_tokens"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateRequired(w, "provider_id", req.ProviderID) || !validateRequired(w, "model_name", req.ModelName) {
		return
	}
	if msg, ok := validateAIModelPricing(req.Pricing); !ok {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	if msg, ok := validateLongContextThreshold(req.LongContextThresholdTokens); !ok {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	providerID := strings.TrimSpace(req.ProviderID)
	modelName := strings.TrimSpace(req.ModelName)
	if !s.ensureAIModelProviderExists(w, r, providerID) {
		return
	}
	if dup, err := s.isDuplicateAIModelName(r.Context(), providerID, modelName, ""); err != nil {
		writeStorageError(w, err)
		return
	} else if dup {
		writeError(w, http.StatusConflict, "duplicate_model", "an AI model with this model_name already exists for the provider")
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = modelName
	}
	threshold := 0
	if req.LongContextThresholdTokens != nil {
		threshold = *req.LongContextThresholdTokens
	}
	u := currentUser(r.Context())
	model := &domain.AIModel{
		ProviderID:                 providerID,
		DisplayName:                displayName,
		ModelName:                  modelName,
		ContextWindow:              req.ContextWindow,
		SupportsTools:              req.SupportsTools,
		Pricing:                    req.Pricing,
		LongContextThresholdTokens: threshold,
		Status:                     domain.ResourceActive,
		RegisteredBy:               u.ID,
	}
	created, err := s.store.CreateAIModel(s.pendingUserAuditCtx(r, "aimodel.create", "ai_model", "", "", nil), model)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listAIModels(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	status := domain.ResourceStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	switch status {
	case "", domain.ResourceActive, domain.ResourceDeprecated:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "status must be active or deprecated")
		return
	}
	models, err := s.store.ListAIModels(r.Context(), status)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *Server) getAIModel(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	model, err := s.store.GetAIModel(r.Context(), chi.URLParam(r, "modelID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// updateAIModelRequest is the PATCH body for an AI Model.
type updateAIModelRequest struct {
	ProviderID                 *string          `json:"provider_id"`
	DisplayName                *string          `json:"display_name"`
	ModelName                  *string          `json:"model_name"`
	ContextWindow              *int             `json:"context_window"`
	SupportsTools              *bool            `json:"supports_tools"`
	Pricing                    *json.RawMessage `json:"pricing"`
	LongContextThresholdTokens *int             `json:"long_context_threshold_tokens"`
}

// applyAIModelScalarFields validates and applies the non-pricing fields of
// an updateAIModel request. On invalid input it writes the HTTP error and
// returns false. Extracted for cognitive complexity (S-126 / S3776).
func (s *Server) applyAIModelScalarFields(w http.ResponseWriter, r *http.Request, model *domain.AIModel, req updateAIModelRequest) bool {
	if req.ProviderID != nil {
		providerID := strings.TrimSpace(*req.ProviderID)
		if !validateRequired(w, "provider_id", providerID) {
			return false
		}
		if !s.ensureAIModelProviderExists(w, r, providerID) {
			return false
		}
		model.ProviderID = providerID
	}
	if req.ModelName != nil {
		modelName := strings.TrimSpace(*req.ModelName)
		if !validateRequired(w, "model_name", modelName) {
			return false
		}
		model.ModelName = modelName
	}
	if req.DisplayName != nil {
		displayName := strings.TrimSpace(*req.DisplayName)
		if !validateRequired(w, "display_name", displayName) {
			return false
		}
		model.DisplayName = displayName
	}
	if req.ContextWindow != nil {
		if *req.ContextWindow < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "context_window must be non-negative")
			return false
		}
		model.ContextWindow = *req.ContextWindow
	}
	if req.SupportsTools != nil {
		model.SupportsTools = *req.SupportsTools
	}
	return true
}

// applyAIModelPricingFields validates and applies the pricing-related
// fields of an updateAIModel request. On invalid input it writes the
// HTTP error and returns false. Extracted for cognitive complexity
// (S-126 / S3776).
func applyAIModelPricingFields(w http.ResponseWriter, model *domain.AIModel, req updateAIModelRequest) bool {
	if req.Pricing != nil {
		if msg, ok := validateAIModelPricing(*req.Pricing); !ok {
			writeError(w, http.StatusBadRequest, "bad_request", msg)
			return false
		}
		model.Pricing = *req.Pricing
	}
	if req.LongContextThresholdTokens != nil {
		if msg, ok := validateLongContextThreshold(req.LongContextThresholdTokens); !ok {
			writeError(w, http.StatusBadRequest, "bad_request", msg)
			return false
		}
		model.LongContextThresholdTokens = *req.LongContextThresholdTokens
	}
	return true
}

func (s *Server) updateAIModel(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	model, err := s.store.GetAIModel(r.Context(), chi.URLParam(r, "modelID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	var req updateAIModelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.applyAIModelScalarFields(w, r, model, req) {
		return
	}
	if !applyAIModelPricingFields(w, model, req) {
		return
	}
	if dup, err := s.isDuplicateAIModelName(r.Context(), model.ProviderID, model.ModelName, model.ID); err != nil {
		writeStorageError(w, err)
		return
	} else if dup {
		writeError(w, http.StatusConflict, "duplicate_model", "an AI model with this model_name already exists for the provider")
		return
	}
	updated, err := s.store.UpdateAIModel(s.pendingUserAuditCtx(r, "aimodel.update", "ai_model", model.ID, "", nil), model)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// deprecateAIModel deprecates the model and then runs the WP4 cascade:
// every agent bound to it (primary or fallback, any owner) has the slot
// cleared and its virtual key converged. Deprecation does NOT hard-block
// on in-use references — it is the intended "stop using this" action —
// but it reports how many agents/users were affected and any key
// convergence failures. Response is 200 with the cascade report.
func (s *Server) deprecateAIModel(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	modelID := chi.URLParam(r, "modelID")
	model, err := s.store.GetAIModel(r.Context(), modelID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := s.store.DeprecateAIModel(s.pendingUserAuditCtx(r, "aimodel.deprecate", "ai_model", modelID, "", nil), modelID); err != nil {
		writeStorageError(w, err)
		return
	}
	report := s.deprecateAIModelCascade(r, model)
	s.finishCascade(w, r, report, false)
}

// aiModelUsage enumerates every live reference to an AI Model: user grants
// (agent fields empty) and agent bindings in the primary or fallback slot
// (with the squad owner identified where resolvable).
func (s *Server) aiModelUsage(ctx context.Context, modelID string) ([]aiModelUsageEntry, error) {
	usage := make([]aiModelUsageEntry, 0)
	grants, err := s.store.ListUsersGrantedModel(ctx, modelID)
	if err != nil {
		return nil, err
	}
	for _, grant := range grants {
		user, err := s.store.GetUser(ctx, grant.GranteeUserID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return nil, err
		}
		usage = append(usage, aiModelUsageEntry{UserID: user.ID, UserEmail: user.Email})
	}
	agents, err := s.store.ListAllAgents(ctx)
	if err != nil {
		return nil, err
	}
	for _, agent := range agents {
		if agent.AIModelID != modelID && agent.FallbackAIModelID != modelID {
			continue
		}
		entry := aiModelUsageEntry{AgentID: agent.ID, AgentName: agent.Name, SquadID: agent.SquadID, Slot: agentBoundSlot(agent, modelID)}
		// Owner identification is best-effort: the agent binding is the
		// load-bearing signal even if the owner lookup fails.
		if ownerID, ownerEmail, ok := s.squadOwnerOf(ctx, agent.SquadID); ok {
			entry.UserID = ownerID
			entry.UserEmail = ownerEmail
		}
		usage = append(usage, entry)
	}
	return usage, nil
}

// squadOwnerOf resolves the owner user of a squad for usage reporting.
// Extracted from aiModelUsage for cognitive complexity (S-126 / S3776).
func (s *Server) squadOwnerOf(ctx context.Context, squadID string) (userID, email string, ok bool) {
	squad, err := s.store.GetSquad(ctx, squadID)
	if err != nil {
		return "", "", false
	}
	owner, err := s.store.GetUser(ctx, squad.OwnerID)
	if err != nil {
		return "", "", false
	}
	return owner.ID, owner.Email, true
}

// deleteAIModel hard-deletes a model with the S-103 in-use warning shape.
// force=true unbinds every agent referencing the model (the store
// RESTRICTs deletion while bindings exist) and cascades the user grants.
func (s *Server) deleteAIModel(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	modelID := chi.URLParam(r, "modelID")
	model, err := s.store.GetAIModel(r.Context(), modelID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	force := r.URL.Query().Get("force") == "true"
	usage, err := s.aiModelUsage(r.Context(), modelID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if !force && len(usage) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "in_use",
			"message": fmt.Sprintf("AI model is granted to users and/or bound by %d referencing(s); retry with force to delete it, revoke the grants and unbind those agents", len(usage)),
			"usage":   usage,
		})
		return
	}
	if force && !s.convergeForAIModelDelete(w, r, modelID, model) {
		return
	}
	if err := s.store.DeleteAIModel(s.pendingUserAuditCtx(r, "aimodel.delete", "ai_model", modelID, "", nil), modelID); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// convergeForAIModelDelete runs the WP4 pre-delete convergence: every
// affected virtual key must converge before the model disappears, or a
// live key could keep calling a deleted model. If any convergence fails
// the delete is NOT performed (502, retryable) so a half-applied state
// is never reported as success. Returns true when the delete may proceed;
// on failure it writes the HTTP response and returns false. Extracted
// from deleteAIModel for cognitive complexity (S-126 / S3776).
func (s *Server) convergeForAIModelDelete(w http.ResponseWriter, r *http.Request, modelID string, model *domain.AIModel) bool {
	affected, err := s.modelBoundAgents(r.Context(), modelID, "")
	if err != nil {
		writeStorageError(w, err)
		return false
	}
	userSet := map[string]bool{}
	for _, agent := range affected {
		if squad, err := s.store.GetSquad(r.Context(), agent.SquadID); err == nil && squad.OwnerID != "" {
			userSet[squad.OwnerID] = true
		}
	}
	report := &cascadeReport{
		ModelID:        modelID,
		Operation:      "delete",
		AffectedAgents: len(affected),
		AffectedUsers:  make([]string, 0, len(userSet)),
	}
	for uid := range userSet {
		report.AffectedUsers = append(report.AffectedUsers, uid)
	}
	if len(affected) > 0 {
		actions, failures := s.convergeAfterModelRemoval(r, modelID, model.ModelName, "deleted", affected)
		report.KeyActions = actions
		report.Failures = failures
		s.notifyCascadeOwners(r.Context(), model.ModelName, "deleted", affected)
	}
	if report.hasFailures() {
		s.finishCascade(w, r, report, true)
		return false
	}
	return true
}

// grantedAIModels resolves a user's grants to model records.
func (s *Server) grantedAIModels(ctx context.Context, userID string) ([]*domain.AIModel, error) {
	grants, err := s.store.ListUserModelGrants(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.AIModel, 0, len(grants))
	for _, grant := range grants {
		model, err := s.store.GetAIModel(ctx, grant.AIModelID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, model)
	}
	return out, nil
}

// listUsers handles GET /api/v1/users — the platform_admin directory
// read for the Settings → Access tab (WP6). Returns a lean projection
// (id, email, name, role, created_at) rather than the full domain.User:
// the grant editor never needs OIDC issuer/subject, so they stay behind
// the store boundary.
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id":         u.ID,
			"email":      u.Email,
			"name":       u.Name,
			"role":       u.Role,
			"created_at": u.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listUserModels(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	userID := chi.URLParam(r, "userID")
	if _, err := s.store.GetUser(r.Context(), userID); err != nil {
		writeStorageError(w, err)
		return
	}
	models, err := s.grantedAIModels(r.Context(), userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, models)
}

// setUserModels sets the user's grant set idempotently: the request is
// the desired state; adds and removes are diffed and applied; re-granting
// an existing grant is a no-op. Returns the resulting granted set.
func (s *Server) setUserModels(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	userID := chi.URLParam(r, "userID")
	user, err := s.store.GetUser(r.Context(), userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	var req struct {
		ModelIDs []string `json:"model_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	desired, desiredIDs, ok := s.validateDesiredModelIDs(w, r, req.ModelIDs)
	if !ok {
		return
	}
	current, err := s.store.ListUserModelGrants(r.Context(), userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	currentSet := make(map[string]bool, len(current))
	for _, grant := range current {
		currentSet[grant.AIModelID] = true
	}

	// WP4 (D9): removals from the desired set are revokes and must not
	// strand live keys. If any user-owned agent still binds a model that
	// is about to be removed, require ?force=true; with force the full
	// cascade (clear slots → converge keys) runs for each removal.
	force := r.URL.Query().Get("force") == "true"
	removals, boundTotal, err := s.pendingGrantRemovals(r, userID, current, desired)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if s.writeGrantRemovalConflict(w, user, removals, boundTotal, force) {
		return
	}

	u := currentUser(r.Context())
	metadata, _ := json.Marshal(map[string]any{"user_id": userID, "model_ids": desiredIDs})
	if err := s.recordUserAuditRequired(r, "aimodel.grants.set", "user_model_grant", userID, "", metadata); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to audit model grant update")
		return
	}
	if !s.grantMissingModels(w, r, userID, desiredIDs, currentSet, u.ID) {
		return
	}
	cascadeFailures, ok := s.revokeGrantsAndConverge(w, r, userID, user, removals)
	if !ok {
		return
	}
	if len(cascadeFailures) > 0 {
		// Grants were applied — that part is done — but some virtual keys
		// did not converge. Report truthfully instead of a clean 200.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":    "convergence_failed",
			"message":  fmt.Sprintf("grants updated but %d virtual key(s) failed to converge; the gateway reconcile endpoint repairs them", len(cascadeFailures)),
			"failures": cascadeFailures,
		})
		return
	}
	models, err := s.grantedAIModels(r.Context(), userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *Server) writeGrantRemovalConflict(w http.ResponseWriter, user *domain.User, removals []pendingGrantRemoval, boundTotal int, force bool) bool {
	if boundTotal == 0 || force {
		return false
	}
	usage := make([]aiModelUsageEntry, 0, boundTotal)
	for _, rem := range removals {
		usage = append(usage, cascadeUsage(user.ID, user.Email, rem.agents, rem.modelID)...)
	}
	writeInUseConflict(w,
		fmt.Sprintf("removing grants would orphan %d bound agent(s); retry with force=true to revoke the grants, clear the bindings and converge the virtual keys", boundTotal),
		usage)
	return true
}

// pendingGrantRemoval is a grant that is about to be revoked together
// with the user-owned agents still bound to that model.
type pendingGrantRemoval struct {
	modelID string
	agents  []*domain.Agent
}

// validateDesiredModelIDs trims, dedupes and existence-checks the
// requested grant set. On invalid input it writes the HTTP error and
// returns ok=false. Extracted from setUserModels for cognitive
// complexity (S-126 / S3776).
func (s *Server) validateDesiredModelIDs(w http.ResponseWriter, r *http.Request, rawIDs []string) (map[string]bool, []string, bool) {
	desired := make(map[string]bool, len(rawIDs))
	desiredIDs := make([]string, 0, len(rawIDs))
	for _, raw := range rawIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "model_ids entries must not be empty")
			return nil, nil, false
		}
		if desired[id] {
			continue
		}
		if _, err := s.store.GetAIModel(r.Context(), id); err != nil {
			writeStorageError(w, err)
			return nil, nil, false
		}
		desired[id] = true
		desiredIDs = append(desiredIDs, id)
	}
	return desired, desiredIDs, true
}

// pendingGrantRemovals diffs current grants against the desired set and
// resolves the agents still bound to each removal. Extracted from
// setUserModels for cognitive complexity (S-126 / S3776).
func (s *Server) pendingGrantRemovals(r *http.Request, userID string, current []*domain.UserModelGrant, desired map[string]bool) ([]pendingGrantRemoval, int, error) {
	removals := make([]pendingGrantRemoval, 0)
	boundTotal := 0
	for _, grant := range current {
		if desired[grant.AIModelID] {
			continue
		}
		bound, err := s.modelBoundAgents(r.Context(), grant.AIModelID, userID)
		if err != nil {
			return nil, 0, err
		}
		if len(bound) > 0 {
			boundTotal += len(bound)
		}
		removals = append(removals, pendingGrantRemoval{modelID: grant.AIModelID, agents: bound})
	}
	return removals, boundTotal, nil
}

// grantMissingModels applies the additions (grants present in the desired
// set but not currently granted). Idempotent: an existing-grant conflict
// is a no-op. Extracted from setUserModels (S-126 / S3776).
func (s *Server) grantMissingModels(w http.ResponseWriter, r *http.Request, userID string, desiredIDs []string, currentSet map[string]bool, grantedBy string) bool {
	for _, id := range desiredIDs {
		if currentSet[id] {
			continue
		}
		if _, err := s.store.GrantModelToUser(r.Context(), &domain.UserModelGrant{
			GranteeUserID: userID,
			AIModelID:     id,
			GrantedBy:     grantedBy,
		}); err != nil && !errors.Is(err, storage.ErrConflict) {
			writeStorageError(w, err)
			return false
		}
	}
	return true
}

// revokeGrantsAndConverge revokes each pending removal and converges the
// affected virtual keys (WP4 D9). Returns the accumulated convergence
// failures; on a store error it writes the HTTP error and returns
// ok=false. Extracted from setUserModels (S-126 / S3776).
func (s *Server) revokeGrantsAndConverge(w http.ResponseWriter, r *http.Request, userID string, user *domain.User, removals []pendingGrantRemoval) ([]cascadeFailure, bool) {
	var cascadeFailures []cascadeFailure
	for _, rem := range removals {
		meta, _ := json.Marshal(map[string]any{"user_id": userID, "model_id": rem.modelID, "forced": true})
		if err := s.store.RevokeModelFromUser(s.pendingUserAuditCtx(r, "aimodel.grant.revoke", "user_model_grant", rem.modelID, "", meta), userID, rem.modelID); err != nil && !errors.Is(err, storage.ErrNotFound) {
			writeStorageError(w, err)
			return nil, false
		}
		model, err := s.store.GetAIModel(r.Context(), rem.modelID)
		modelName := rem.modelID
		if err == nil {
			modelName = model.ModelName
		}
		_, failures := s.convergeAfterModelRemoval(r, rem.modelID, modelName, "revoked", rem.agents)
		cascadeFailures = append(cascadeFailures, failures...)
		s.notifyCascadeOwners(r.Context(), modelName, "revoked from user "+user.Email, rem.agents)
	}
	return cascadeFailures, true
}

// listMyModels is the self-service read for the agent UI: only models
// granted to the CALLING user AND still active. Deprecated models must
// never appear (ADR-0010: deprecation stops new bindings).
func (s *Server) listMyModels(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	grants, err := s.store.ListUserModelGrants(r.Context(), u.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	out := make([]*domain.AIModel, 0, len(grants))
	for _, grant := range grants {
		model, err := s.store.GetAIModel(r.Context(), grant.AIModelID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			writeStorageError(w, err)
			return
		}
		if model.Status != domain.ResourceActive {
			continue
		}
		out = append(out, model)
	}
	writeJSON(w, http.StatusOK, out)
}
