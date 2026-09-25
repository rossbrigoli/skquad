// WP4 — revoke/deprecate/delete cascade (ADR-0010 D9).
//
// The invariant owned by this file: after a grant revoke, model
// deprecation, or force-delete completes successfully, NO live virtual
// key can still call the revoked/deprecated/deleted model. Because the
// grant compiles into the key (D5) and the runtime never re-checks
// grants, every removal path must converge the affected agents' keys via
// syncAgentGatewayKey: clear the binding slot, then update the key
// (fallback removed), re-provision it (still validly bound), or revoke
// it (agent left without a usable primary).
//
// Partial failures never abort the cascade and are never swallowed: each
// failure is recorded, the remaining agents are still processed, and the
// failures are reported in the HTTP response and the audit log.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// Binding slot labels used in the S-103-compatible 409 usage entries.
const (
	slotPrimary  = "primary"
	slotFallback = "fallback"
)

// cascadeKeyAction records one key-convergence outcome for one agent.
type cascadeKeyAction struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	SquadID   string `json:"squad_id"`
	Action    string `json:"action"` // "updated" | "revoked" | "provisioned"
}

// cascadeFailure records one step that failed during the cascade. Stage
// is "unbind" (clearing the binding slot) or "converge" (the gateway
// key sync). A failure never stops the other agents from being processed.
type cascadeFailure struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	SquadID   string `json:"squad_id"`
	Stage     string `json:"stage"`
	Error     string `json:"error"`
}

// cascadeReport is the truthful summary returned by force-revoke and
// deprecate cascades.
type cascadeReport struct {
	ModelID        string             `json:"model_id"`
	Operation      string             `json:"operation"` // revoke|deprecate|delete
	AffectedAgents int                `json:"affected_agents"`
	AffectedUsers  []string           `json:"affected_users"`
	KeyActions     []cascadeKeyAction `json:"key_actions"`
	Failures       []cascadeFailure   `json:"failures"`
}

func (r *cascadeReport) hasFailures() bool { return len(r.Failures) > 0 }

func (r *cascadeReport) metadata() json.RawMessage {
	meta, _ := json.Marshal(r)
	return meta
}

// agentBoundSlot returns the slot ("primary"/"fallback") in which the
// agent references modelID, or "" when it does not.
func agentBoundSlot(agent *domain.Agent, modelID string) string {
	switch modelID {
	case agent.AIModelID:
		return slotPrimary
	case agent.FallbackAIModelID:
		return slotFallback
	default:
		return ""
	}
}

// modelBoundAgents enumerates every agent bound to modelID in either
// slot. When ownerUserID is non-empty, only agents whose SQUAD OWNER is
// that user are returned — the authorisation boundary ADR-0010 D3 puts
// on grants (boundModelAllowList resolves grants via squad.OwnerID).
func (s *Server) modelBoundAgents(ctx context.Context, modelID, ownerUserID string) ([]*domain.Agent, error) {
	agents, err := s.store.ListAllAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Agent, 0)
	for _, agent := range agents {
		if agent.AIModelID != modelID && agent.FallbackAIModelID != modelID {
			continue
		}
		keep, err := s.passesModelOwnerFilter(ctx, agent, ownerUserID)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		out = append(out, agent)
	}
	return out, nil
}

// passesModelOwnerFilter reports whether the agent passes the ownerUserID
// authorisation boundary (all agents pass when ownerUserID is empty).
// Extracted from modelBoundAgents for cognitive complexity
// (S-126 / S3776).
func (s *Server) passesModelOwnerFilter(ctx context.Context, agent *domain.Agent, ownerUserID string) (bool, error) {
	if ownerUserID == "" {
		return true, nil
	}
	squad, err := s.store.GetSquad(ctx, agent.SquadID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return squad.OwnerID == ownerUserID, nil
}

// cascadeUsage builds the S-103-compatible usage list for the 409
// response: one entry per bound agent with the offending slot.
func cascadeUsage(userID, userEmail string, agents []*domain.Agent, modelID string) []aiModelUsageEntry {
	usage := make([]aiModelUsageEntry, 0, len(agents))
	for _, agent := range agents {
		usage = append(usage, aiModelUsageEntry{
			UserID:    userID,
			UserEmail: userEmail,
			AgentID:   agent.ID,
			AgentName: agent.Name,
			SquadID:   agent.SquadID,
			Slot:      agentBoundSlot(agent, modelID),
		})
	}
	return usage
}

// writeInUseConflict emits the 409 body in the exact S-103 delete-with-
// in-use-warning shape (error/message/usage) so the WP6 dialog can be
// reused unchanged.
func writeInUseConflict(w http.ResponseWriter, message string, usage []aiModelUsageEntry) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":   "in_use",
		"message": message,
		"usage":   usage,
	})
}

// convergeAfterModelRemoval is the heart of the D9 cascade. For each
// affected agent it clears the model's binding slot, then converges the
// agent's virtual key with syncAgentGatewayKey:
//
//   - fallback slot cleared  → key allow-list shrinks (agent keeps its primary)
//   - primary slot cleared   → agent is left unbound → key revoked
//   - agent still bound      → key updated/re-provisioned from the new binding
//
// Every convergence is audited. Failures are collected, never fatal to
// the loop. If the binding slot cannot be cleared (store failure) the
// cascade fails CLOSED: the live key is revoked directly so the removed
// model can never be called from a binding we failed to erase.
func (s *Server) convergeAfterModelRemoval(r *http.Request, modelID, modelName, operation string, agents []*domain.Agent) ([]cascadeKeyAction, []cascadeFailure) {
	ctx := r.Context()
	actions := make([]cascadeKeyAction, 0, len(agents))
	failures := make([]cascadeFailure, 0)
	for _, agent := range agents {
		cleared := *agent
		if cleared.AIModelID == modelID {
			cleared.AIModelID = ""
		}
		if cleared.FallbackAIModelID == modelID {
			cleared.FallbackAIModelID = ""
		}
		if _, err := s.store.UpdateAgent(ctx, &cleared); err != nil {
			// Fail closed: we could not erase the reference, so the key
			// must not stay live. Best-effort revoke; both errors are
			// reported together.
			revokeErr := s.revokeLiveKeyForAgent(ctx, agent.ID)
			errText := err.Error()
			if revokeErr != nil {
				errText = fmt.Sprintf("%s (fail-closed key revoke also failed: %s)", errText, revokeErr.Error())
			}
			failures = append(failures, cascadeFailure{
				AgentID: agent.ID, AgentName: agent.Name, SquadID: agent.SquadID,
				Stage: "unbind", Error: errText,
			})
			continue
		}
		action, err := s.syncAgentGatewayKey(ctx, &cleared)
		if err != nil {
			failures = append(failures, cascadeFailure{
				AgentID: agent.ID, AgentName: agent.Name, SquadID: agent.SquadID,
				Stage: "converge", Error: err.Error(),
			})
			continue
		}
		if action == "none" {
			continue
		}
		actions = append(actions, cascadeKeyAction{
			AgentID: agent.ID, AgentName: agent.Name, SquadID: agent.SquadID, Action: action,
		})
		meta, _ := json.Marshal(map[string]string{
			"model_id": modelID, "model_name": modelName, "agent_id": agent.ID, "action": action,
		})
		s.recordUserAudit(r, "aimodel.cascade.key_converged", "ai_model", modelID, agent.SquadID, meta)
	}
	return actions, failures
}

// revokeLiveKeyForAgent revokes the agent's currently-live virtual key
// directly (used fail-closed when the binding row cannot be cleared).
func (s *Server) revokeLiveKeyForAgent(ctx context.Context, agentID string) error {
	identity, err := s.store.GetAgentIdentity(ctx, agentID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil // no identity, nothing live
		}
		return err
	}
	if identity.GatewayKeyStatus != domain.GatewayKeyActive || identity.GatewayKeyToken == "" {
		return nil
	}
	if err := s.llmGateway.RevokeAgentKey(ctx, identity.GatewayKeyToken); err != nil {
		return err
	}
	_, err = s.store.SetAgentIdentityGatewayKey(ctx, agentID, identity.GatewayKeyToken, domain.GatewayKeyRevoked)
	return err
}

// notifyCascadeOwners files a best-effort owner-facing inbox
// notification per affected squad so owners know a rebind is required.
func (s *Server) notifyCascadeOwners(ctx context.Context, modelName, operation string, agents []*domain.Agent) {
	seen := map[string]bool{}
	for _, agent := range agents {
		if seen[agent.SquadID] {
			continue
		}
		seen[agent.SquadID] = true
		s.notifySquadOwner(ctx, agent.SquadID, domain.InboxActionRequired, "", "",
			fmt.Sprintf("AI model %q was %s; agents bound to it (e.g. %q) had that binding cleared. Rebind them to an active model.",
				modelName, operation, agent.Name))
	}
}

// finishCascade audits the overall cascade outcome and writes the JSON
// report. When strict is true a convergence failure suppresses the
// success status: the caller (force-delete) has NOT completed, so the
// response is 502 with the failure detail and the caller must retry —
// the dangerous half-state (model gone, keys live) is never reported as
// success.
func (s *Server) finishCascade(w http.ResponseWriter, r *http.Request, report *cascadeReport, strict bool) {
	s.recordUserAudit(r, "aimodel.cascade."+report.Operation, "ai_model", report.ModelID, "", report.metadata())
	if strict && report.hasFailures() {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":    "convergence_failed",
			"message":  fmt.Sprintf("%s aborted: %d virtual key(s) failed to converge; the model was NOT removed. Retry after fixing the gateway; already-converged agents are idempotent.", report.Operation, len(report.Failures)),
			"failures": report.Failures,
			"model_id": report.ModelID,
		})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// revokeUserModel handles DELETE /api/v1/users/{userID}/models/{modelID}.
//
// Without ?force=true, revoking a model that the user's agents still bind
// (primary or fallback) returns 409 in_use with the affected agents and
// their slots. With ?force=true the grant is revoked, every affected
// binding slot is cleared, every affected virtual key is converged
// (updated / re-provisioned / revoked per the remaining binding), each
// step is audited, and owners are notified.
func (s *Server) revokeUserModel(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	userID := chi.URLParam(r, "userID")
	modelID := chi.URLParam(r, "modelID")
	user, err := s.store.GetUser(r.Context(), userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	model, err := s.store.GetAIModel(r.Context(), modelID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	granted := false
	grants, err := s.store.ListUserModelGrants(r.Context(), userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	for _, g := range grants {
		if g.AIModelID == modelID {
			granted = true
			break
		}
	}
	if !granted {
		writeError(w, http.StatusNotFound, "grant_not_found", "user does not hold a grant for this AI model")
		return
	}
	force := r.URL.Query().Get("force") == "true"
	affected, err := s.modelBoundAgents(r.Context(), modelID, userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if len(affected) > 0 && !force {
		writeInUseConflict(w,
			fmt.Sprintf("model %q is bound by %d agent(s) owned by this user; retry with force=true to revoke the grant, clear the bindings and converge the virtual keys", model.DisplayName, len(affected)),
			cascadeUsage(user.ID, user.Email, affected, modelID))
		return
	}

	// Core action first: the grant removal is what the admin asked for.
	// The cascade follows so a cascade failure never leaves the grant
	// half-removed.
	revocationMeta, _ := json.Marshal(map[string]any{
		"user_id": userID, "model_id": modelID, "forced": len(affected) > 0,
	})
	if err := s.store.RevokeModelFromUser(s.pendingUserAuditCtx(r, "aimodel.grant.revoke", "user_model_grant", modelID, "", revocationMeta), userID, modelID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		writeStorageError(w, err)
		return
	}

	report := &cascadeReport{
		ModelID:        modelID,
		Operation:      "revoke",
		AffectedAgents: len(affected),
		AffectedUsers:  []string{userID},
	}
	if len(affected) > 0 {
		actions, failures := s.convergeAfterModelRemoval(r, modelID, model.ModelName, "revoked", affected)
		report.KeyActions = actions
		report.Failures = failures
		s.notifyCascadeOwners(r.Context(), model.ModelName, "revoked from user "+user.Email, affected)
	}
	s.finishCascade(w, r, report, false)
}

// deprecateAIModelCascade converges every agent bound to a deprecated
// model, regardless of owner. Deprecation is the intended "stop using
// this" action and therefore does NOT hard-block on in-use references —
// it clears the deprecated model out of both binding slots and
// converges the keys, then reports how many agents/users were affected.
// Called after the model row is already deprecated.
func (s *Server) deprecateAIModelCascade(r *http.Request, model *domain.AIModel) *cascadeReport {
	affected, err := s.modelBoundAgents(r.Context(), model.ID, "")
	if err != nil {
		// Enumeration failure: report it as a single cascade failure so
		// the response stays truthful; the deprecation itself stands and
		// reconcile can repair later.
		return &cascadeReport{
			ModelID:   model.ID,
			Operation: "deprecate",
			Failures: []cascadeFailure{{
				Stage: "enumerate", Error: err.Error(),
			}},
		}
	}
	userSet := map[string]bool{}
	for _, agent := range affected {
		if squad, err := s.store.GetSquad(r.Context(), agent.SquadID); err == nil && squad.OwnerID != "" {
			userSet[squad.OwnerID] = true
		}
	}
	report := &cascadeReport{
		ModelID:        model.ID,
		Operation:      "deprecate",
		AffectedAgents: len(affected),
		AffectedUsers:  make([]string, 0, len(userSet)),
	}
	for uid := range userSet {
		report.AffectedUsers = append(report.AffectedUsers, uid)
	}
	if len(affected) > 0 {
		actions, failures := s.convergeAfterModelRemoval(r, model.ID, model.ModelName, "deprecated", affected)
		report.KeyActions = actions
		report.Failures = failures
		s.notifyCascadeOwners(r.Context(), model.ModelName, "deprecated", affected)
	}
	return report
}
