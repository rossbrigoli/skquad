// TG-8 slice C: confirmation gates + standing grants
// (docs/tg8-grant-approvals-spec.md §C).
//
// Flow: a gated tool call hits POST /internal/v1/confirmation/check.
// Standing grants are consulted FIRST (spec: auto-approve path checks
// standing_grants first and logs the matched id); a live match answers
// `auto` without paging the owner. Otherwise a pending_confirmation row +
// owner action_required inbox message are created (slice B's inbox
// pattern reused, not forked). The owner's decision — deny / approve
// once (15-min TTL, bound to the exact args_hash) / approve this and
// future (standing_grants upsert, default +90d) — is race-safe via
// expectedFrom state transitions (double decision ⇒ 409).
//
// The gateway releases the original call via POST .../confirmation/consume:
// approved_once is single-use (consumed_at; second consume ⇒
// denied_replayed) and expires 15 min after approval; approved_standing
// re-validates the live standing grant so revocation is immediately
// effective (invariant 2).
//
// Authz: the resource owner attribute (registry_resources.owner_user_id)
// or platform_admin decides. Internal endpoints share the /internal/v1
// trust class (no app-layer auth; cluster NetworkPolicy enforces
// reachability) — same contract as /policy and /credentials.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	// confirmationOnceTTL: an approved_once decision releases the
	// bound call only within this window of its approval timestamp.
	confirmationOnceTTL = 15 * time.Minute
	// standingGrantDefaultExpiry: "approve this and future" defaults to
	// 90 days (spec §C).
	standingGrantDefaultExpiry = 90 * 24 * time.Hour
	// argsHashDisplayLen: how much of the args hash the owner-facing
	// inbox message shows.
	argsHashDisplayLen = 12
)

// Service-level sentinels mapped to HTTP codes by the handlers.
var (
	errConfirmationNotFound  = errors.New("confirmation not found")
	errConfirmationForbidden = errors.New("confirmation: caller lacks authority")
	errConfirmationConflict  = errors.New("confirmation: invalid state transition")
	errConfirmationNoOwner   = errors.New("confirmation: resource has no owner to decide")
)

// confirmationTime is the injectable clock (TTL/expiry evaluation).
// Production uses wall time; tests drive it to age approvals.
func (s *Server) confirmationTime() time.Time {
	if s.confirmationNow != nil {
		return s.confirmationNow()
	}
	return time.Now().UTC()
}

// ConfirmationCheckResult is the §C check response: `auto` when a live
// standing grant short-circuits the ask, `pending` with the confirmation
// id the owner decides on.
type ConfirmationCheckResult struct {
	Mode                   string `json:"mode"` // "auto" | "pending"
	ConfirmationID         string `json:"confirmation_id,omitempty"`
	MatchedStandingGrantID string `json:"matched_standing_grant_id,omitempty"`
}

// ConfirmationConsumeResult is the §C consume response the gateway acts
// on. Reason is agent-visible on denials (e.g. denied_by_owner: <why>).
type ConfirmationConsumeResult struct {
	Allowed                bool   `json:"allowed"`
	Mode                   string `json:"mode,omitempty"` // "once" | "standing"
	Reason                 string `json:"reason,omitempty"`
	MatchedStandingGrantID string `json:"matched_standing_grant_id,omitempty"`
}

// ── Service ──────────────────────────────────────────────────────────────

// requestConfirmation runs the §C check: standing-first, then create
// pending row + owner inbox message.
func (s *Server) requestConfirmation(ctx context.Context, resourceID, agentID, tool, argsHash string) (*ConfirmationCheckResult, error) {
	resource, err := s.store.GetResourceByID(ctx, resourceID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, errConfirmationNotFound
		}
		return nil, err
	}
	if _, err := s.store.GetAgent(ctx, agentID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, errConfirmationNotFound
		}
		return nil, err
	}
	now := s.confirmationTime()

	// Standing grants are checked FIRST (spec §C). A live match
	// (not revoked, not expired) auto-approves and is audited with the
	// matched grant id.
	if grant, err := s.store.FindLiveStandingGrant(ctx, resourceID, agentID, tool, now); err == nil {
		s.auditConfirmation(ctx, "user", resource.OwnerUserID, "confirmation.standing_match", resourceID, map[string]any{
			"agent_id":               agentID,
			"tool":                   tool,
			"matched_standing_grant": grant.ID,
		})
		return &ConfirmationCheckResult{Mode: "auto", MatchedStandingGrantID: grant.ID}, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}

	if strings.TrimSpace(resource.OwnerUserID) == "" {
		// Nobody holds the owner attribute ⇒ nobody can decide. The
		// gated call cannot proceed (deviation noted in WORKLOG: spec
		// assumes an owner exists for gated operations).
		return nil, errConfirmationNoOwner
	}

	created, err := s.store.CreatePendingConfirmation(ctx, &domain.PendingConfirmation{
		ResourceID:  resource.ID,
		AgentID:     agentID,
		Tool:        tool,
		ArgsHash:    argsHash,
		State:       domain.ConfirmationPending,
		RequestedBy: resource.OwnerUserID,
	})
	if err != nil {
		return nil, err
	}

	// Owner inbox message — best-effort like every other inbox emit in
	// this codebase: a failed notification must not fail the workflow
	// row that already committed. The 3-action payload: resource name,
	// tool, args_hash prefix, created.
	msg, mErr := s.store.CreateInboxMessage(ctx, &domain.InboxMessage{
		UserID:  resource.OwnerUserID,
		Kind:    domain.InboxActionRequired,
		Message: fmt.Sprintf("Confirmation required: %s on %s", tool, resource.Name),
		Subject: fmt.Sprintf("Approve %s on %s?", tool, resource.Name),
		Body: fmt.Sprintf("Agent %s wants to run %s on resource %s (args %s). Created %s. Actions: deny, approve once (single call, 15-min TTL), approve this and future (standing grant).",
			agentID, tool, resource.Name, shortArgsHash(argsHash), created.CreatedAt.Format(time.RFC3339)),
	})
	if mErr == nil {
		updated, uErr := s.store.UpdateConfirmationState(ctx, created.ID, domain.ConfirmationPending, storage.ConfirmationUpdate{
			State:          domain.ConfirmationPending,
			InboxMessageID: &msg.ID,
		})
		if uErr == nil {
			created = updated
		}
	}
	return &ConfirmationCheckResult{Mode: "pending", ConfirmationID: created.ID}, nil
}

// approveConfirmationOnce: owner (or admin) releases the single bound
// call. TTL starts here (approved_at); consume enforces it.
func (s *Server) approveConfirmationOnce(ctx context.Context, caller *domain.User, confID string) (*domain.PendingConfirmation, error) {
	conf, resource, err := s.loadConfirmationAndResource(ctx, confID)
	if err != nil {
		return nil, err
	}
	if err := requireConfirmationAuthority(caller, resource); err != nil {
		return nil, err
	}
	if conf.State != domain.ConfirmationPending {
		return nil, errConfirmationConflict
	}
	now := s.confirmationTime()
	updated, err := s.store.UpdateConfirmationState(ctx, conf.ID, domain.ConfirmationPending, storage.ConfirmationUpdate{
		State:      domain.ConfirmationApprovedOnce,
		ApprovedAt: &now,
	})
	if err != nil {
		return nil, mapConfirmationStoreErr(err)
	}
	s.auditConfirmation(ctx, "user", caller.ID, "confirmation.approve_once", resource.ID, map[string]any{
		"confirmation_id": conf.ID, "agent_id": conf.AgentID, "tool": conf.Tool,
	})
	return updated, nil
}

// approveConfirmationStanding: owner (or admin) upserts the standing
// grant (default +90d) AND satisfies the current call — consume of this
// confirmation with the same args_hash behaves like approved_once's
// first consume, backed by the standing grant.
func (s *Server) approveConfirmationStanding(ctx context.Context, caller *domain.User, confID string, expiry *time.Time) (*domain.PendingConfirmation, *domain.StandingGrant, error) {
	conf, resource, err := s.loadConfirmationAndResource(ctx, confID)
	if err != nil {
		return nil, nil, err
	}
	if err := requireConfirmationAuthority(caller, resource); err != nil {
		return nil, nil, err
	}
	if conf.State != domain.ConfirmationPending {
		return nil, nil, errConfirmationConflict
	}
	now := s.confirmationTime()
	updated, err := s.store.UpdateConfirmationState(ctx, conf.ID, domain.ConfirmationPending, storage.ConfirmationUpdate{
		State:      domain.ConfirmationApprovedStanding,
		ApprovedAt: &now,
	})
	if err != nil {
		return nil, nil, mapConfirmationStoreErr(err)
	}
	expiresAt := now.Add(standingGrantDefaultExpiry)
	if expiry != nil {
		expiresAt = *expiry
	}
	grant, err := s.store.UpsertStandingGrant(ctx, &domain.StandingGrant{
		ResourceID: resource.ID,
		AgentID:    conf.AgentID,
		Tool:       conf.Tool,
		ExpiresAt:  expiresAt,
		CreatedBy:  caller.ID,
	})
	if err != nil {
		return nil, nil, err
	}
	s.auditConfirmation(ctx, "user", caller.ID, "confirmation.approve_standing", resource.ID, map[string]any{
		"confirmation_id": conf.ID, "agent_id": conf.AgentID, "tool": conf.Tool,
		"standing_grant_id": grant.ID, "expires_at": expiresAt.Format(time.RFC3339),
	})
	return updated, grant, nil
}

// denyConfirmation: owner (or admin) denies. The reason is stored,
// audited, and surfaced to the agent through the consume response
// (`denied_by_owner: <reason>`) plus a confirmation_denied inbox
// outcome message to the deciding owner's record (mirror of slice B's
// grant_denied pattern; the inbox is user-only in v1, so the agent's
// visible channel is the consume reply — deviation noted in WORKLOG).
func (s *Server) denyConfirmation(ctx context.Context, caller *domain.User, confID, reason string) (*domain.PendingConfirmation, error) {
	conf, resource, err := s.loadConfirmationAndResource(ctx, confID)
	if err != nil {
		return nil, err
	}
	if err := requireConfirmationAuthority(caller, resource); err != nil {
		return nil, err
	}
	if conf.State != domain.ConfirmationPending {
		return nil, errConfirmationConflict
	}
	if strings.TrimSpace(reason) == "" {
		reason = "denied"
	}
	updated, err := s.store.UpdateConfirmationState(ctx, conf.ID, domain.ConfirmationPending, storage.ConfirmationUpdate{
		State:        domain.ConfirmationDenied,
		DeniedReason: reason,
	})
	if err != nil {
		return nil, mapConfirmationStoreErr(err)
	}
	s.auditConfirmation(ctx, "user", caller.ID, "confirmation.deny", resource.ID, map[string]any{
		"confirmation_id": conf.ID, "agent_id": conf.AgentID, "tool": conf.Tool, "reason": reason,
	})
	// Best-effort outcome message (slice B pattern: never fail the
	// committed transition on notification trouble).
	_, _ = s.store.CreateInboxMessage(ctx, &domain.InboxMessage{
		UserID:  conf.RequestedBy,
		Kind:    domain.InboxKind("confirmation_denied"),
		Message: "Confirmation denied",
		Subject: fmt.Sprintf("Denied: %s on %s", conf.Tool, resource.Name),
		Body: fmt.Sprintf("You denied confirmation %s for agent %s running %s (args %s): %s",
			conf.ID, conf.AgentID, conf.Tool, shortArgsHash(conf.ArgsHash), reason),
	})
	return updated, nil
}

// consumeConfirmation releases (or refuses) the gated call. Called by
// the gateway after the owner's decision. The args_hash must match the
// hash the confirmation was bound to — approvals never leak across
// argument sets.
func (s *Server) consumeConfirmation(ctx context.Context, confID, argsHash string) (*ConfirmationConsumeResult, error) {
	conf, err := s.store.GetPendingConfirmation(ctx, confID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, errConfirmationNotFound
		}
		return nil, err
	}
	if conf.ArgsHash != argsHash {
		return &ConfirmationConsumeResult{Allowed: false, Reason: "args_hash_mismatch"}, nil
	}
	now := s.confirmationTime()
	switch conf.State {
	case domain.ConfirmationApprovedOnce:
		if conf.ConsumedAt != nil {
			return &ConfirmationConsumeResult{Allowed: false, Reason: "denied_replayed"}, nil
		}
		if conf.ApprovedAt == nil || now.Sub(*conf.ApprovedAt) > confirmationOnceTTL {
			// TTL elapsed: mark expired (best-effort; the answer
			// stands even if the write races away).
			_, _ = s.store.UpdateConfirmationState(ctx, conf.ID, domain.ConfirmationApprovedOnce,
				storage.ConfirmationUpdate{State: domain.ConfirmationExpired})
			return &ConfirmationConsumeResult{Allowed: false, Reason: "approval_expired"}, nil
		}
		consumed := now
		// Single-use claim is race-safe: expectedFrom=approved_once.
		// A concurrent consume that won the race leaves us conflicting
		// ⇒ the call is a replay.
		if _, uErr := s.store.UpdateConfirmationState(ctx, conf.ID, domain.ConfirmationApprovedOnce,
			storage.ConfirmationUpdate{State: domain.ConfirmationApprovedOnce, ConsumedAt: &consumed}); uErr != nil {
			if errors.Is(uErr, storage.ErrConflict) {
				return &ConfirmationConsumeResult{Allowed: false, Reason: "denied_replayed"}, nil
			}
			return nil, uErr
		}
		return &ConfirmationConsumeResult{Allowed: true, Mode: "once"}, nil
	case domain.ConfirmationApprovedStanding:
		// Standing is re-validated at consume so a revoke between
		// approval and consume is immediately effective (invariant 2).
		grant, gErr := s.store.FindLiveStandingGrant(ctx, conf.ResourceID, conf.AgentID, conf.Tool, now)
		if gErr != nil {
			if errors.Is(gErr, storage.ErrNotFound) {
				return &ConfirmationConsumeResult{Allowed: false, Reason: "standing_grant_not_live"}, nil
			}
			return nil, gErr
		}
		s.auditConfirmation(ctx, "system", "", "confirmation.standing_match", conf.ResourceID, map[string]any{
			"confirmation_id": conf.ID, "agent_id": conf.AgentID, "tool": conf.Tool,
			"matched_standing_grant": grant.ID,
		})
		return &ConfirmationConsumeResult{Allowed: true, Mode: "standing", MatchedStandingGrantID: grant.ID}, nil
	case domain.ConfirmationDenied:
		reason := "denied_by_owner"
		if conf.DeniedReason != "" {
			reason = "denied_by_owner: " + conf.DeniedReason
		}
		return &ConfirmationConsumeResult{Allowed: false, Reason: reason}, nil
	case domain.ConfirmationExpired:
		return &ConfirmationConsumeResult{Allowed: false, Reason: "confirmation_expired"}, nil
	default: // pending
		return &ConfirmationConsumeResult{Allowed: false, Reason: "confirmation_pending"}, nil
	}
}

func (s *Server) loadConfirmationAndResource(ctx context.Context, confID string) (*domain.PendingConfirmation, *domain.RegistryResource, error) {
	conf, err := s.store.GetPendingConfirmation(ctx, confID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, errConfirmationNotFound
		}
		return nil, nil, err
	}
	resource, err := s.store.GetResourceByID(ctx, conf.ResourceID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, errConfirmationNotFound
		}
		return nil, nil, err
	}
	return conf, resource, nil
}

// requireConfirmationAuthority: the resource owner attribute or
// platform_admin decides confirmations (same authority model as
// slice B's grant workflow).
func requireConfirmationAuthority(caller *domain.User, resource *domain.RegistryResource) error {
	if caller == nil {
		return errConfirmationForbidden
	}
	if caller.ID == resource.OwnerUserID || caller.Role == domain.RolePlatformAdmin {
		return nil
	}
	return errConfirmationForbidden
}

func mapConfirmationStoreErr(err error) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return errConfirmationNotFound
	case errors.Is(err, storage.ErrConflict):
		return errConfirmationConflict
	default:
		return err
	}
}

func shortArgsHash(h string) string {
	if len(h) <= argsHashDisplayLen {
		return h
	}
	return h[:argsHashDisplayLen] + "…"
}

// auditConfirmation writes a best-effort audit entry with a JSON
// metadata payload (mirrors the record*Audit helpers, ctx-based for
// service callers that have no *http.Request).
func (s *Server) auditConfirmation(ctx context.Context, actorType, actorID, action, resourceID string, metadata map[string]any) {
	payload, err := json.Marshal(metadata)
	if err != nil {
		payload = json.RawMessage(`{}`)
	}
	_ = s.store.RecordAudit(ctx, &domain.AuditEntry{
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       action,
		ResourceType: "tool_resource",
		ResourceID:   resourceID,
		Metadata:     payload,
	})
}

// ── HTTP handlers ────────────────────────────────────────────────────────

func writeConfirmationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errConfirmationNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, errConfirmationForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, errConfirmationConflict):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, errConfirmationNoOwner):
		writeError(w, http.StatusBadRequest, "no_owner", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "confirmation request failed")
	}
}

// confirmationCheckHandler handles POST /internal/v1/confirmation/check
// (gateway trust class — no app-layer auth, internal-only by
// NetworkPolicy, same as /policy and /credentials).
// Request:  {resource_id, agent_id, tool, args_hash}
// Response: {mode: "auto"|"pending", confirmation_id?, matched_standing_grant_id?}
func (s *Server) confirmationCheckHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceID string `json:"resource_id"`
		AgentID    string `json:"agent_id"`
		Tool       string `json:"tool"`
		ArgsHash   string `json:"args_hash"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ResourceID) == "" || strings.TrimSpace(req.AgentID) == "" ||
		strings.TrimSpace(req.Tool) == "" || strings.TrimSpace(req.ArgsHash) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "resource_id, agent_id, tool and args_hash are required")
		return
	}
	result, err := s.requestConfirmation(r.Context(), req.ResourceID, req.AgentID, req.Tool, req.ArgsHash)
	if err != nil {
		writeConfirmationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// confirmationConsumeHandler handles POST /internal/v1/confirmation/consume.
// Request:  {id, args_hash}
// Response: {allowed, mode?, reason?, matched_standing_grant_id?}
func (s *Server) confirmationConsumeHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		ArgsHash string `json:"args_hash"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.ArgsHash) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "id and args_hash are required")
		return
	}
	result, err := s.consumeConfirmation(r.Context(), req.ID, req.ArgsHash)
	if err != nil {
		writeConfirmationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// listConfirmationsHandler handles GET /api/v1/confirmations?mine=true&state=.
// mine=true → confirmations the caller owns the decision for
// (requested_by = caller). Admins see everything without mine.
func (s *Server) listConfirmationsHandler(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r.Context())
	filter := storage.ConfirmationFilter{State: r.URL.Query().Get("state")}
	if r.URL.Query().Get("mine") == "true" {
		filter.RequestedBy = caller.ID
	} else if caller.Role != domain.RolePlatformAdmin {
		filter.RequestedBy = caller.ID
	}
	confs, err := s.store.ListPendingConfirmations(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list confirmations")
		return
	}
	writeJSON(w, http.StatusOK, confs)
}

// approveConfirmationOnceHandler handles POST /api/v1/confirmations/{confID}/approve-once.
func (s *Server) approveConfirmationOnceHandler(w http.ResponseWriter, r *http.Request) {
	updated, err := s.approveConfirmationOnce(r.Context(), currentUser(r.Context()), chi.URLParam(r, "confID"))
	if err != nil {
		writeConfirmationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// approveConfirmationStandingHandler handles POST /api/v1/confirmations/{confID}/approve-standing
// with optional body {expiry: RFC3339}.
func (s *Server) approveConfirmationStandingHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Expiry *time.Time `json:"expiry"`
	}
	// Body is optional; a malformed one is a 400 only when present.
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	updated, grant, err := s.approveConfirmationStanding(r.Context(), currentUser(r.Context()), chi.URLParam(r, "confID"), req.Expiry)
	if err != nil {
		writeConfirmationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"confirmation":   updated,
		"standing_grant": grant,
	})
}

// denyConfirmationHandler handles POST /api/v1/confirmations/{confID}/deny {reason}.
func (s *Server) denyConfirmationHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	updated, err := s.denyConfirmation(r.Context(), currentUser(r.Context()), chi.URLParam(r, "confID"), req.Reason)
	if err != nil {
		writeConfirmationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// listStandingGrantsHandler handles GET /api/v1/standing-grants.
// Owner sees grants on their own resources; platform_admin sees all.
func (s *Server) listStandingGrantsHandler(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r.Context())
	filter := storage.StandingGrantFilter{}
	if caller.Role != domain.RolePlatformAdmin {
		filter.OwnerUserID = caller.ID
	}
	grants, err := s.store.ListStandingGrants(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list standing grants")
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

// revokeStandingGrantHandler handles DELETE /api/v1/standing-grants/{grantID}.
// Owner of the grant's resource (or platform_admin) revokes; the soft
// delete is one row update, effective immediately (invariant 2 — the
// gateway's own cache TTL ≤30s bounds the cluster-wide view).
func (s *Server) revokeStandingGrantHandler(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r.Context())
	grantID := chi.URLParam(r, "grantID")
	grant, err := s.store.GetStandingGrant(r.Context(), grantID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "standing grant not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to load standing grant")
		return
	}
	resource, err := s.store.GetResourceByID(r.Context(), grant.ResourceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "standing grant resource not found")
		return
	}
	if err := requireConfirmationAuthority(caller, resource); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", "standing grant: caller lacks authority")
		return
	}
	revoked, err := s.store.RevokeStandingGrant(r.Context(), grantID, s.confirmationTime())
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "standing grant not found or already revoked")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to revoke standing grant")
		return
	}
	s.auditConfirmation(r.Context(), "user", caller.ID, "standing_grant.revoke", grant.ResourceID, map[string]any{
		"standing_grant_id": grantID, "agent_id": grant.AgentID, "tool": grant.Tool,
	})
	writeJSON(w, http.StatusOK, revoked)
}
