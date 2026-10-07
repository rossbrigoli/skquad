// TG-8 slice B: grant-request state machine + tier routing + inbox
// notifications (docs/tg8-grant-approvals-spec.md §B).
//
// Workflow vs. effective artifact (invariant 2): a grant_request is only
// workflow. Approval materializes the real grant row via
// PermissionStore.GrantAgentPermission; the gateway keeps reading grants,
// never requests.
//
// Routing table (spec §B):
//
//	BYO (requester==owner) + tier low/medium + no block findings
//	    → auto-approve: materialize directly, NO request row.
//	BYO + block finding (any tier)          → pending_owner + findings.
//	medium shared (requester != owner)      → pending_owner.
//	high (any requester)                  → pending_owner → pending_admin.
//	owner-less resource                     → pending_admin directly
//	    (nobody holds the owner attribute; the admin step is the only
//	     authority — deviation noted in WORKLOG).
//
// Block findings ALWAYS disable auto-approval; warn findings do not block
// in v1 (they ride along on any created row for reviewer context).
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
	"github.com/rossbrigoli/skquad/control-plane/internal/grantlint"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// GrantChangeSnapshot aliases the §A pinned linter snapshot — the real
// grantlint.ChangeSnapshot (slice A landed; local mirror retired).
type GrantChangeSnapshot = grantlint.ChangeSnapshot

// GrantLinter matches grantlint.LintChange's pinned signature:
//
//	func LintChange(before, after *ChangeSnapshot) []Finding
//
// before == nil means a brand-new grant (empty baseline). Production
// wires grantlint.LintChange itself via NewWithGrantLinter; tests inject
// stubs. nil ⇒ no findings (auto-approval stays enabled).
type GrantLinter func(before, after *grantlint.ChangeSnapshot) []grantlint.Finding

// Service-level sentinels mapped to HTTP codes by the handlers.
var (
	errGrantRequestNotFound   = errors.New("grant request not found")
	errGrantRequestForbidden  = errors.New("grant request: caller lacks authority")
	errGrantTransitionInvalid = errors.New("grant request: invalid state transition")
)

// lintGrant runs the injected linter on the proposed grant snapshot. A
// nil linter (grantlint not wired at build time) yields no findings —
// auto-approval stays enabled. grantlint.Finding is converted to the
// domain shape at this boundary so storage/Inbox never import the linter.
func (s *Server) lintGrant(resource *domain.RegistryResource, scope json.RawMessage) []domain.GrantLintFinding {
	if s.grantLinter == nil {
		return nil
	}
	findings := s.grantLinter(nil, grantSnapshotFor(resource, scope))
	out := make([]domain.GrantLintFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, domain.GrantLintFinding{Code: f.Code, Severity: f.Severity, Detail: f.Detail})
	}
	return out
}

// grantScopeFields is the shared JSON shape for both the resource's
// policy_ceiling and the request's requested_scope. Parsing is best-effort:
// unknown fields are ignored so ceiling/scope evolution never breaks
// linting.
type grantScopeFields struct {
	Hosts         []string       `json:"hosts"`
	HTTPMethods   []string       `json:"http_methods"`
	MCPTools      []string       `json:"mcp_tools"`
	HasCredential *bool          `json:"has_credential"`
	NumericCaps   map[string]int `json:"numeric_caps"`
}

// grantSnapshotFor builds the "after" snapshot: the resource's ceiling
// provides the reachable baseline, the requested scope overrides the
// fields it actually sets. HasCredential also reflects the resource's
// AuthRef — a custodied credential makes any granted host credentialed.
func grantSnapshotFor(resource *domain.RegistryResource, scope json.RawMessage) *GrantChangeSnapshot {
	snap := &GrantChangeSnapshot{
		ResourceID:    resource.ID,
		RiskTier:      resource.RiskTier,
		EgressClass:   resource.EgressClass,
		HasCredential: strings.TrimSpace(resource.AuthRef) != "",
	}
	var ceiling, requested grantScopeFields
	_ = json.Unmarshal(resource.PolicyCeiling, &ceiling)
	_ = json.Unmarshal(scope, &requested)
	snap.Hosts = ceiling.Hosts
	snap.HTTPMethods = ceiling.HTTPMethods
	snap.MCPTools = ceiling.MCPTools
	snap.NumericCaps = ceiling.NumericCaps
	if requested.Hosts != nil {
		snap.Hosts = requested.Hosts
	}
	if requested.HTTPMethods != nil {
		snap.HTTPMethods = requested.HTTPMethods
	}
	if requested.MCPTools != nil {
		snap.MCPTools = requested.MCPTools
	}
	if requested.NumericCaps != nil {
		snap.NumericCaps = requested.NumericCaps
	}
	if requested.HasCredential != nil && *requested.HasCredential {
		snap.HasCredential = true
	}
	return snap
}

// ── Service ──────────────────────────────────────────────────────────────

// createGrantRequest runs the §B routing. autoApproved=true means the
// grant was materialized directly and NO request row exists (BYO
// low/medium with no block findings).
func (s *Server) createGrantRequest(ctx context.Context, caller *domain.User, resourceID, agentID string, scope json.RawMessage, expiry *time.Time) (*domain.GrantRequest, bool, error) {
	resource, err := s.store.GetResourceByID(ctx, resourceID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, false, errGrantRequestNotFound
		}
		return nil, false, err
	}
	if _, err := s.store.GetAgent(ctx, agentID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, false, errGrantRequestNotFound
		}
		return nil, false, err
	}
	tier := resource.RiskTier
	if tier == "" {
		tier = "low"
	}
	findings := s.lintGrant(resource, scope)
	hasBlock := domain.FindingsHaveBlock(findings)
	byo := resource.OwnerUserID != "" && caller.ID == resource.OwnerUserID

	if byo && tier != "high" && !hasBlock {
		if err := s.materializeGrant(ctx, agentID, resource, caller.ID, scope); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}

	state := domain.GrantRequestPendingOwner
	req := &domain.GrantRequest{
		ResourceID:      resource.ID,
		AgentID:         agentID,
		RequesterUserID: caller.ID,
		Tier:            tier,
		State:           state,
		RequestedScope:  scope,
		Findings:        findings,
		Expiry:          expiry,
	}
	if resource.OwnerUserID == "" {
		// No owner attribute ⇒ no owner step; route straight to admin.
		req.State = domain.GrantRequestPendingAdmin
	}
	created, err := s.store.CreateGrantRequest(ctx, req)
	if err != nil {
		return nil, false, err
	}
	s.notifyGrantPending(ctx, created, resource)
	return created, false, nil
}

// approveGrantOwner: caller must hold the resource's owner attribute (or
// be platform_admin). high tier hands off to pending_admin; everything
// else approves + materializes.
func (s *Server) approveGrantOwner(ctx context.Context, caller *domain.User, requestID string) (*domain.GrantRequest, error) {
	req, resource, err := s.loadGrantRequestAndResource(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if caller.ID != resource.OwnerUserID && caller.Role != domain.RolePlatformAdmin {
		return nil, errGrantRequestForbidden
	}
	if req.State != domain.GrantRequestPendingOwner {
		return nil, errGrantTransitionInvalid
	}
	now := time.Now().UTC()
	up := storage.GrantRequestUpdate{ApprovedByOwnerAt: &now}
	if req.Tier == "high" {
		up.State = domain.GrantRequestPendingAdmin
		updated, err := s.store.UpdateGrantRequestState(ctx, req.ID, domain.GrantRequestPendingOwner, up)
		if err != nil {
			return nil, mapGrantStoreErr(err)
		}
		s.notifyGrantPending(ctx, updated, resource)
		return updated, nil
	}
	up.State = domain.GrantRequestApproved
	updated, err := s.store.UpdateGrantRequestState(ctx, req.ID, domain.GrantRequestPendingOwner, up)
	if err != nil {
		return nil, mapGrantStoreErr(err)
	}
	if err := s.materializeGrant(ctx, req.AgentID, resource, req.RequesterUserID, req.RequestedScope); err != nil {
		return nil, err
	}
	s.notifyGrantOutcome(ctx, updated, "grant_approved",
		"Grant request approved",
		fmt.Sprintf("Your grant request %s for resource %s (agent %s) was approved by the resource owner.", updated.ID, resource.Name, req.AgentID))
	return updated, nil
}

// approveGrantAdmin: platform_admin only, only from pending_admin.
func (s *Server) approveGrantAdmin(ctx context.Context, caller *domain.User, requestID string) (*domain.GrantRequest, error) {
	if caller.Role != domain.RolePlatformAdmin {
		return nil, errGrantRequestForbidden
	}
	req, resource, err := s.loadGrantRequestAndResource(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if req.State != domain.GrantRequestPendingAdmin {
		return nil, errGrantTransitionInvalid
	}
	now := time.Now().UTC()
	updated, err := s.store.UpdateGrantRequestState(ctx, req.ID, domain.GrantRequestPendingAdmin, storage.GrantRequestUpdate{
		State:             domain.GrantRequestApproved,
		ApprovedByAdminAt: &now,
	})
	if err != nil {
		return nil, mapGrantStoreErr(err)
	}
	if err := s.materializeGrant(ctx, req.AgentID, resource, req.RequesterUserID, req.RequestedScope); err != nil {
		return nil, err
	}
	s.notifyGrantOutcome(ctx, updated, "grant_approved",
		"Grant request approved",
		fmt.Sprintf("Your grant request %s for resource %s (agent %s) was approved with admin co-sign.", updated.ID, resource.Name, req.AgentID))
	return updated, nil
}

// denyGrant: allowed by the CURRENT state's authority — owner (or admin)
// while pending_owner, platform_admin while pending_admin. Terminal states
// reject with a transition error.
func (s *Server) denyGrant(ctx context.Context, caller *domain.User, requestID, reason string) (*domain.GrantRequest, error) {
	req, resource, err := s.loadGrantRequestAndResource(ctx, requestID)
	if err != nil {
		return nil, err
	}
	switch req.State {
	case domain.GrantRequestPendingOwner:
		if caller.ID != resource.OwnerUserID && caller.Role != domain.RolePlatformAdmin {
			return nil, errGrantRequestForbidden
		}
	case domain.GrantRequestPendingAdmin:
		if caller.Role != domain.RolePlatformAdmin {
			return nil, errGrantRequestForbidden
		}
	default:
		return nil, errGrantTransitionInvalid
	}
	if strings.TrimSpace(reason) == "" {
		reason = "denied"
	}
	updated, err := s.store.UpdateGrantRequestState(ctx, req.ID, req.State, storage.GrantRequestUpdate{
		State:        domain.GrantRequestDenied,
		DeniedReason: reason,
	})
	if err != nil {
		return nil, mapGrantStoreErr(err)
	}
	s.notifyGrantOutcome(ctx, updated, "grant_denied",
		"Grant request denied",
		fmt.Sprintf("Your grant request %s for resource %s (agent %s) was denied: %s", updated.ID, resource.Name, req.AgentID, reason))
	return updated, nil
}

func (s *Server) loadGrantRequestAndResource(ctx context.Context, requestID string) (*domain.GrantRequest, *domain.RegistryResource, error) {
	req, err := s.store.GetGrantRequest(ctx, requestID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, errGrantRequestNotFound
		}
		return nil, nil, err
	}
	resource, err := s.store.GetResourceByID(ctx, req.ResourceID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, errGrantRequestNotFound
		}
		return nil, nil, err
	}
	return req, resource, nil
}

// materializeGrant writes the effective grant row (agent_permissions)
// with the request's scope as constraints.
func (s *Server) materializeGrant(ctx context.Context, agentID string, resource *domain.RegistryResource, grantedBy string, scope json.RawMessage) error {
	constraints := scope
	if len(constraints) == 0 {
		constraints = json.RawMessage(`{}`)
	}
	return s.store.GrantAgentPermission(ctx, &domain.AgentPermission{
		AgentID:      agentID,
		ResourceType: resource.Type,
		ResourceID:   resource.ID,
		GrantedBy:    grantedBy,
		Constraints:  constraints,
	})
}

func mapGrantStoreErr(err error) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return errGrantRequestNotFound
	case errors.Is(err, storage.ErrConflict):
		return errGrantTransitionInvalid
	default:
		return err
	}
}

// ── Inbox notifications (reuse, do not fork) ────────────────────────────

// notifyGrantPending pings the authority for the request's current
// state: the resource owner for pending_owner, platform admins for
// pending_admin. pending_owner on an owner-less resource was already
// routed to pending_admin at create time, so the owner branch always
// has a recipient.
func (s *Server) notifyGrantPending(ctx context.Context, req *domain.GrantRequest, resource *domain.RegistryResource) {
	switch req.State {
	case domain.GrantRequestPendingOwner:
		findingNote := findingsSummaryForMessage(req.Findings)
		s.notifyInboxUser(ctx, resource.OwnerUserID, domain.InboxActionRequired,
			"Grant request awaiting your review",
			fmt.Sprintf("User %s requested access for agent %s to resource %s (%s tier).%s",
				req.RequesterUserID, req.AgentID, resource.Name, req.Tier, findingNote))
	case domain.GrantRequestPendingAdmin:
		findingNote := findingsSummaryForMessage(req.Findings)
		s.notifyPlatformAdmins(ctx, domain.InboxActionRequired,
			"Grant request awaiting admin co-sign",
			fmt.Sprintf("Grant request %s for resource %s (%s tier) needs a platform admin decision.%s",
				req.ID, resource.Name, req.Tier, findingNote))
	}
}

func (s *Server) notifyGrantOutcome(ctx context.Context, req *domain.GrantRequest, kind, subject, body string) {
	s.notifyInboxUser(ctx, req.RequesterUserID, domain.InboxKind(kind), subject, body)
}

func (s *Server) notifyInboxUser(ctx context.Context, userID string, kind domain.InboxKind, subject, body string) {
	if userID == "" {
		return
	}
	// Best-effort like the other inbox emits in this codebase: a failed
	// notification must not fail the workflow transition that already
	// committed.
	_, _ = s.store.CreateInboxMessage(ctx, &domain.InboxMessage{
		UserID:  userID,
		Kind:    kind,
		Message: subject,
		Subject: subject,
		Body:    body,
	})
}

func (s *Server) notifyPlatformAdmins(ctx context.Context, kind domain.InboxKind, subject, body string) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return
	}
	for _, u := range users {
		if u.Role == domain.RolePlatformAdmin {
			s.notifyInboxUser(ctx, u.ID, kind, subject, body)
		}
	}
}

func findingsSummaryForMessage(findings []domain.GrantLintFinding) string {
	if len(findings) == 0 {
		return ""
	}
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("[%s] %s: %s", strings.ToUpper(f.Severity), f.Code, f.Detail))
	}
	return " Linter findings — " + strings.Join(parts, "; ")
}

// ── HTTP handlers ────────────────────────────────────────────────────────

func writeGrantError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errGrantRequestNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, errGrantRequestForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, errGrantTransitionInvalid):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "grant request failed")
	}
}

// createGrantRequestHandler handles POST /api/v1/grant-requests.
// Any authenticated user may request; routing decides who reviews.
// BYO auto-approval answers 200 with auto_approved=true and no row;
// a created workflow row answers 201.
func (s *Server) createGrantRequestHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceID     string          `json:"resource_id"`
		AgentID        string          `json:"agent_id"`
		RequestedScope json.RawMessage `json:"requested_scope"`
		Expiry         *time.Time      `json:"expiry"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ResourceID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "resource_id is required")
		return
	}
	// v1 materializes grants per agent; "grant to squad" requests keep
	// agent_id nullable in the schema but have no materialization path
	// yet (deviation noted in WORKLOG).
	if strings.TrimSpace(req.AgentID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent_id is required (squad-wide grants are not materialized in v1)")
		return
	}
	caller := currentUser(r.Context())
	created, autoApproved, err := s.createGrantRequest(r.Context(), caller, req.ResourceID, req.AgentID, req.RequestedScope, req.Expiry)
	if err != nil {
		writeGrantError(w, err)
		return
	}
	if autoApproved {
		writeJSON(w, http.StatusOK, map[string]any{
			"state":         string(domain.GrantRequestApproved),
			"auto_approved": true,
			"resource_id":   req.ResourceID,
			"agent_id":      req.AgentID,
		})
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// approveGrantOwnerHandler handles POST /api/v1/grant-requests/{id}/approve-owner.
func (s *Server) approveGrantOwnerHandler(w http.ResponseWriter, r *http.Request) {
	updated, err := s.approveGrantOwner(r.Context(), currentUser(r.Context()), chi.URLParam(r, "requestID"))
	if err != nil {
		writeGrantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// approveGrantAdminHandler handles POST /api/v1/grant-requests/{id}/approve-admin.
func (s *Server) approveGrantAdminHandler(w http.ResponseWriter, r *http.Request) {
	updated, err := s.approveGrantAdmin(r.Context(), currentUser(r.Context()), chi.URLParam(r, "requestID"))
	if err != nil {
		writeGrantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// denyGrantHandler handles POST /api/v1/grant-requests/{id}/deny.
func (s *Server) denyGrantHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	updated, err := s.denyGrant(r.Context(), currentUser(r.Context()), chi.URLParam(r, "requestID"), req.Reason)
	if err != nil {
		writeGrantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// listGrantRequestsHandler handles GET /api/v1/grant-requests?state=&mine=owner|admin.
// mine=owner → requests on resources the caller owns; mine=admin →
// platform_admin only, all requests; no mine → admins see everything,
// everyone else sees their own submissions.
func (s *Server) listGrantRequestsHandler(w http.ResponseWriter, r *http.Request) {
	caller := currentUser(r.Context())
	filter := storage.GrantRequestFilter{State: r.URL.Query().Get("state")}
	switch r.URL.Query().Get("mine") {
	case "":
		if caller.Role != domain.RolePlatformAdmin {
			filter.RequesterUserID = caller.ID
		}
	case "owner":
		filter.OwnerUserID = caller.ID
	case "admin":
		if caller.Role != domain.RolePlatformAdmin {
			writeError(w, http.StatusForbidden, "forbidden", "platform admin role is required")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "mine must be owner or admin")
		return
	}
	reqs, err := s.store.ListGrantRequests(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list grant requests")
		return
	}
	writeJSON(w, http.StatusOK, reqs)
}
