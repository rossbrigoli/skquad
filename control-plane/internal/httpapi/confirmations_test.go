// TG-8 slice C tests: confirmation gates + standing grants
// (docs/tg8-grant-approvals-spec.md §C). Mirrors slice B's test style:
// OIDC-mode harness with promoted admin + plain users, MemoryStore,
// doJSONAuth; TTL/expiry tests run at the service level with the
// injectable confirmation clock (budget_guard_test.go style seeding).

package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const confArgsHash = "a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899001122334455ff"

func confHarness(t *testing.T) (http.Handler, *storage.MemoryStore) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithGrantLinter(cfg, store, headerOIDC{
		authAdmin: {Email: "admin@example.com", Name: "Admin"},
		authAlice: {Email: aliceEmail, Name: "Alice"},
		authBob:   {Email: "bob@example.com", Name: "Bob"},
	}, nil)
	promoteAdmin(t, store, handler, authAdmin)
	return handler, store
}

func checkConfirmation(t *testing.T, handler http.Handler, resourceID, agentID, tool, argsHash string) ConfirmationCheckResult {
	t.Helper()
	var res ConfirmationCheckResult
	doJSONAuth(t, handler, authBob, http.MethodPost, "/internal/v1/confirmation/check", map[string]any{
		"resource_id": resourceID,
		"agent_id":    agentID,
		"tool":        tool,
		"args_hash":   argsHash,
	}, http.StatusOK, &res)
	return res
}

func consumeConfirmationHTTP(t *testing.T, handler http.Handler, confID, argsHash string) ConfirmationConsumeResult {
	t.Helper()
	var res ConfirmationConsumeResult
	doJSONAuth(t, handler, authBob, http.MethodPost, "/internal/v1/confirmation/consume", map[string]any{
		"id":        confID,
		"args_hash": argsHash,
	}, http.StatusOK, &res)
	return res
}

// confServiceSetup builds a service-level *Server with a controllable
// clock plus one owned resource and one agent, seeded directly in the
// store (no HTTP) so TTL/expiry can be aged deterministically.
func confServiceSetup(t *testing.T) (*Server, *storage.MemoryStore, *domain.RegistryResource, *domain.Agent, time.Time) {
	t.Helper()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	owner, err := store.UpsertUser(ctx, &domain.User{Email: "conf-owner@example.com", Name: "ConfOwner", Role: domain.RoleUser})
	require.NoError(t, err)
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "conf-squad", OwnerID: owner.ID})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{Name: "conf-agent", SquadID: squad.ID, Role: "worker", Status: domain.AgentIdle})
	require.NoError(t, err)
	res, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type: domain.ResTool, Name: "conf-gated-tool", OwnerUserID: owner.ID, RiskTier: "high",
	})
	require.NoError(t, err)
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	now := start
	s := &Server{store: store, confirmationNow: func() time.Time { return now }}
	return s, store, res, agent, start
}

// ── 1. check → pending + owner inbox message

func TestConfirmationCheckCreatesPendingAndInbox(t *testing.T) {
	t.Parallel()
	handler, store := confHarness(t)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "conf-res-1", "high", alice)
	agent := createAgentOf(t, handler, authBob, "conf-squad-1")

	checked := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)
	require.Equal(t, "pending", checked.Mode)
	require.NotEmpty(t, checked.ConfirmationID)
	require.Empty(t, checked.MatchedStandingGrantID)

	// Owner got the action_required inbox message (slice B pattern).
	require.True(t, hasKind(inboxKinds(t, store, alice), domain.InboxActionRequired))

	// Owner sees it via the user-facing list.
	var mine []domain.PendingConfirmation
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/confirmations?mine=true", nil, http.StatusOK, &mine)
	require.Len(t, mine, 1)
	require.Equal(t, checked.ConfirmationID, mine[0].ID)
	require.Equal(t, domain.ConfirmationPending, mine[0].State)
	require.Equal(t, alice, mine[0].RequestedBy)
	require.Equal(t, confArgsHash, mine[0].ArgsHash)
	require.NotEmpty(t, mine[0].InboxMessageID)
}

// ── 2. approve-once → consume same hash allowed once, replay denied

func TestConfirmationApproveOnceSingleUse(t *testing.T) {
	t.Parallel()
	handler, _ := confHarness(t)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "conf-res-2", "high", alice)
	agent := createAgentOf(t, handler, authBob, "conf-squad-2")

	checked := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)
	var approved domain.PendingConfirmation
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-once", nil, http.StatusOK, &approved)
	require.Equal(t, domain.ConfirmationApprovedOnce, approved.State)
	require.NotNil(t, approved.ApprovedAt)

	first := consumeConfirmationHTTP(t, handler, checked.ConfirmationID, confArgsHash)
	require.True(t, first.Allowed)
	require.Equal(t, "once", first.Mode)

	second := consumeConfirmationHTTP(t, handler, checked.ConfirmationID, confArgsHash)
	require.False(t, second.Allowed)
	require.Equal(t, "denied_replayed", second.Reason)
}

// ── 3. consume with wrong args hash → denied, approval not burned

func TestConfirmationConsumeWrongHash(t *testing.T) {
	t.Parallel()
	handler, _ := confHarness(t)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "conf-res-3", "high", alice)
	agent := createAgentOf(t, handler, authBob, "conf-squad-3")

	checked := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-once", nil, http.StatusOK, &map[string]any{})

	wrong := consumeConfirmationHTTP(t, handler, checked.ConfirmationID, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	require.False(t, wrong.Allowed)
	require.Equal(t, "args_hash_mismatch", wrong.Reason)

	// The mismatched consume did not burn the single-use approval.
	right := consumeConfirmationHTTP(t, handler, checked.ConfirmationID, confArgsHash)
	require.True(t, right.Allowed)
}

// ── 4. approve-standing → standing row + subsequent check auto-matches + current call satisfied

func TestConfirmationApproveStandingAutoMatch(t *testing.T) {
	t.Parallel()
	handler, _ := confHarness(t)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "conf-res-4", "high", alice)
	agent := createAgentOf(t, handler, authBob, "conf-squad-4")

	checked := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)
	var resp struct {
		Confirmation  domain.PendingConfirmation `json:"confirmation"`
		StandingGrant domain.StandingGrant       `json:"standing_grant"`
	}
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-standing", nil, http.StatusOK, &resp)
	require.Equal(t, domain.ConfirmationApprovedStanding, resp.Confirmation.State)
	require.Equal(t, res.ID, resp.StandingGrant.ResourceID)
	require.Equal(t, agent.ID, resp.StandingGrant.AgentID)
	require.Equal(t, "write_object", resp.StandingGrant.Tool)
	require.Equal(t, alice, resp.StandingGrant.CreatedBy)
	require.True(t, resp.StandingGrant.ExpiresAt.After(time.Now().UTC().Add(89*24*time.Hour)))

	// The current call is satisfied with the original args hash.
	cur := consumeConfirmationHTTP(t, handler, checked.ConfirmationID, confArgsHash)
	require.True(t, cur.Allowed)
	require.Equal(t, "standing", cur.Mode)
	require.Equal(t, resp.StandingGrant.ID, cur.MatchedStandingGrantID)

	// A NEW call (different args) short-circuits to auto with the grant id.
	other := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", "9999e3d4e5f60718293a4b5c6d7e8f9011223344556677889900112233445500")
	require.Equal(t, "auto", other.Mode)
	require.Equal(t, resp.StandingGrant.ID, other.MatchedStandingGrantID)
	require.Empty(t, other.ConfirmationID)

	// Owner sees the grant in the panel.
	var grants []domain.StandingGrant
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/standing-grants", nil, http.StatusOK, &grants)
	require.Len(t, grants, 1)
	require.Equal(t, resp.StandingGrant.ID, grants[0].ID)
}

// ── 5. standing grant expiry passed → no match (service-level, injected clock)

func TestConfirmationStandingExpiryNoMatch(t *testing.T) {
	t.Parallel()
	s, _, res, agent, start := confServiceSetup(t)
	ctx := context.Background()

	checked, err := s.requestConfirmation(ctx, res.ID, agent.ID, "write_object", confArgsHash)
	require.NoError(t, err)
	require.Equal(t, "pending", checked.Mode)

	expiry := start.Add(1 * time.Hour)
	_, grant, err := s.approveConfirmationStanding(ctx, &domain.User{ID: res.OwnerUserID, Role: domain.RoleUser}, checked.ConfirmationID, &expiry)
	require.NoError(t, err)

	// Before expiry: auto-match.
	before, err := s.requestConfirmation(ctx, res.ID, agent.ID, "write_object", confArgsHash)
	require.NoError(t, err)
	require.Equal(t, "auto", before.Mode)
	require.Equal(t, grant.ID, before.MatchedStandingGrantID)

	// Past expiry: no match → fresh pending.
	timeAdvance(s, 2*time.Hour)
	after, err := s.requestConfirmation(ctx, res.ID, agent.ID, "write_object", confArgsHash)
	require.NoError(t, err)
	require.Equal(t, "pending", after.Mode)
	require.NotEmpty(t, after.ConfirmationID)
}

// timeAdvance shifts the injected clock forward. (Closure variable in
// confServiceSetup; helper keeps tests readable.)
func timeAdvance(s *Server, d time.Duration) {
	// The clock closure reads a captured variable; wrap by replacing the
	// field with a function that adds d to the previous value.
	prev := s.confirmationNow
	s.confirmationNow = func() time.Time { return prev().Add(d) }
}

// ── 6. revoke → no match, row kept (soft delete), second revoke 404

func TestConfirmationRevokeStopsMatchKeepsRow(t *testing.T) {
	t.Parallel()
	handler, store := confHarness(t)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "conf-res-6", "high", alice)
	agent := createAgentOf(t, handler, authBob, "conf-squad-6")

	checked := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)
	var resp struct {
		Confirmation  domain.PendingConfirmation `json:"confirmation"`
		StandingGrant domain.StandingGrant       `json:"standing_grant"`
	}
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-standing", nil, http.StatusOK, &resp)

	var revoked domain.StandingGrant
	doJSONAuth(t, handler, authAlice, http.MethodDelete, "/api/v1/standing-grants/"+resp.StandingGrant.ID, nil, http.StatusOK, &revoked)
	require.NotNil(t, revoked.RevokedAt)

	// No live match anymore → fresh pending.
	after := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)
	require.Equal(t, "pending", after.Mode)
	require.Empty(t, after.MatchedStandingGrantID)

	// One row survives as audit trail (soft delete).
	all, err := store.ListStandingGrants(context.Background(), storage.StandingGrantFilter{IncludeRevoked: true})
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, resp.StandingGrant.ID, all[0].ID)
	require.NotNil(t, all[0].RevokedAt)

	// Second revoke: 404 (no live row).
	doJSONAuth(t, handler, authAlice, http.MethodDelete, "/api/v1/standing-grants/"+resp.StandingGrant.ID, nil, http.StatusNotFound, &map[string]any{})
}

// ── 7. owner-only authz: wrong user 403, admin sees all

func TestConfirmationOwnerOnlyAuthz(t *testing.T) {
	t.Parallel()
	handler, _ := confHarness(t)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "conf-res-7", "high", alice)
	agent := createAgentOf(t, handler, authBob, "conf-squad-7")

	checked := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)

	// Bob (not the resource owner) cannot decide.
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-once", nil, http.StatusForbidden, &map[string]any{})
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-standing", nil, http.StatusForbidden, &map[string]any{})
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/deny", map[string]any{"reason": "nope"}, http.StatusForbidden, &map[string]any{})

	// Bob sees no standing grants; admin sees all.
	var bobGrants []domain.StandingGrant
	doJSONAuth(t, handler, authBob, http.MethodGet, "/api/v1/standing-grants", nil, http.StatusOK, &bobGrants)
	require.Empty(t, bobGrants)

	// Admin can decide and sees everything.
	var adminResp struct {
		Confirmation  domain.PendingConfirmation `json:"confirmation"`
		StandingGrant domain.StandingGrant       `json:"standing_grant"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-standing", nil, http.StatusOK, &adminResp)
	require.Equal(t, domain.ConfirmationApprovedStanding, adminResp.Confirmation.State)

	var allConfs []domain.PendingConfirmation
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/confirmations", nil, http.StatusOK, &allConfs)
	require.Len(t, allConfs, 1)

	var adminGrants []domain.StandingGrant
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/standing-grants", nil, http.StatusOK, &adminGrants)
	require.Len(t, adminGrants, 1)

	// Bob cannot revoke the grant either.
	doJSONAuth(t, handler, authBob, http.MethodDelete, "/api/v1/standing-grants/"+adminGrants[0].ID, nil, http.StatusForbidden, &map[string]any{})
}

// ── 8. deny flow: reason stored, agent-visible via consume, inbox outcome

func TestConfirmationDenyFlow(t *testing.T) {
	t.Parallel()
	handler, store := confHarness(t)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "conf-res-8", "high", alice)
	agent := createAgentOf(t, handler, authBob, "conf-squad-8")

	checked := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)
	var denied domain.PendingConfirmation
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/deny", map[string]any{"reason": "too risky"}, http.StatusOK, &denied)
	require.Equal(t, domain.ConfirmationDenied, denied.State)
	require.Equal(t, "too risky", denied.DeniedReason)

	// Agent-visible reason through the gateway consume path.
	res2 := consumeConfirmationHTTP(t, handler, checked.ConfirmationID, confArgsHash)
	require.False(t, res2.Allowed)
	require.Equal(t, "denied_by_owner: too risky", res2.Reason)

	// Outcome inbox message (slice B grant_denied pattern).
	require.True(t, hasKind(inboxKinds(t, store, alice), domain.InboxKind("confirmation_denied")))
}

// ── 9. race: double approve → 409

func TestConfirmationDoubleApproveRace(t *testing.T) {
	t.Parallel()
	handler, _ := confHarness(t)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "conf-res-9", "high", alice)
	agent := createAgentOf(t, handler, authBob, "conf-squad-9")

	checked := checkConfirmation(t, handler, res.ID, agent.ID, "write_object", confArgsHash)
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-once", nil, http.StatusOK, &map[string]any{})

	// Any second decision on a non-pending row is a conflict.
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-standing", nil, http.StatusConflict, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/deny", map[string]any{"reason": "too late"}, http.StatusConflict, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/confirmations/"+checked.ConfirmationID+"/approve-once", nil, http.StatusConflict, &map[string]any{})
}

// ── 10. TTL: approved_once aged past 15 min → expired at consume (injected clock)

func TestConfirmationOnceTTLExpired(t *testing.T) {
	t.Parallel()
	s, store, res, agent, _ := confServiceSetup(t)
	ctx := context.Background()

	checked, err := s.requestConfirmation(ctx, res.ID, agent.ID, "write_object", confArgsHash)
	require.NoError(t, err)
	caller := &domain.User{ID: res.OwnerUserID, Role: domain.RoleUser}
	approved, err := s.approveConfirmationOnce(ctx, caller, checked.ConfirmationID)
	require.NoError(t, err)
	require.Equal(t, domain.ConfirmationApprovedOnce, approved.State)

	// Inside the window: allowed.
	inside, err := s.consumeConfirmation(ctx, checked.ConfirmationID, confArgsHash)
	require.NoError(t, err)
	require.True(t, inside.Allowed)

	// Fresh approval, aged past the TTL: expired.
	checked2, err := s.requestConfirmation(ctx, res.ID, agent.ID, "delete_object", confArgsHash)
	require.NoError(t, err)
	_, err = s.approveConfirmationOnce(ctx, caller, checked2.ConfirmationID)
	require.NoError(t, err)
	timeAdvance(s, 16*time.Minute)
	aged, err := s.consumeConfirmation(ctx, checked2.ConfirmationID, confArgsHash)
	require.NoError(t, err)
	require.False(t, aged.Allowed)
	require.Equal(t, "approval_expired", aged.Reason)

	fresh, err := store.GetPendingConfirmation(ctx, checked2.ConfirmationID)
	require.NoError(t, err)
	require.Equal(t, domain.ConfirmationExpired, fresh.State)
}

// ── 11. owner-less resource cannot be gated → 400; validation + 404s

func TestConfirmationValidationAndNoOwner(t *testing.T) {
	t.Parallel()
	handler, _ := confHarness(t)
	agent := createAgentOf(t, handler, authBob, "conf-squad-11")
	orphan := createOwnedResource(t, handler, "conf-orphan", "high", "")

	// No owner ⇒ nobody can decide.
	doJSONAuth(t, handler, authBob, http.MethodPost, "/internal/v1/confirmation/check", map[string]any{
		"resource_id": orphan.ID, "agent_id": agent.ID, "tool": "write_object", "args_hash": confArgsHash,
	}, http.StatusBadRequest, &map[string]any{})

	// Missing fields ⇒ 400.
	doJSONAuth(t, handler, authBob, http.MethodPost, "/internal/v1/confirmation/check", map[string]any{
		"resource_id": orphan.ID,
	}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authBob, http.MethodPost, "/internal/v1/confirmation/consume", map[string]any{
		"id": "x",
	}, http.StatusBadRequest, &map[string]any{})

	// Unknown resource / confirmation ⇒ 404.
	doJSONAuth(t, handler, authBob, http.MethodPost, "/internal/v1/confirmation/check", map[string]any{
		"resource_id": "11111111-1111-1111-1111-111111111111", "agent_id": agent.ID, "tool": "t", "args_hash": "h",
	}, http.StatusNotFound, &map[string]any{})
	doJSONAuth(t, handler, authBob, http.MethodPost, "/internal/v1/confirmation/consume", map[string]any{
		"id": "22222222-2222-2222-2222-222222222222", "args_hash": "h",
	}, http.StatusNotFound, &map[string]any{})
}
