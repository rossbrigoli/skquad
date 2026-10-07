// TG-8 slice B tests: grant-request state machine + tier routing + inbox
// notifications (docs/tg8-grant-approvals-spec.md §B). Mirrors the CP
// httpapi test style: OIDC-mode harness with promoted admin + plain users,
// MemoryStore, doJSONAuth.

package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/grantlint"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const authBob = "Bearer bob"

// grantHarness returns an OIDC-mode handler (admin promoted, alice and
// bob plain users) with the given linter injected.
func grantHarness(t *testing.T, linter GrantLinter) (http.Handler, *storage.MemoryStore) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithGrantLinter(cfg, store, headerOIDC{
		authAdmin: {Email: "admin@example.com", Name: "Admin"},
		authAlice: {Email: aliceEmail, Name: "Alice"},
		authBob:   {Email: "bob@example.com", Name: "Bob"},
	}, linter)
	promoteAdmin(t, store, handler, authAdmin)
	return handler, store
}

// createOwnedResource registers a tool resource with an explicit owner
// attribute and risk tier (admin-only registry write).
func createOwnedResource(t *testing.T, handler http.Handler, name, tier, ownerID string) domain.RegistryResource {
	t.Helper()
	var res domain.RegistryResource
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/registry/tools", map[string]any{
		"name":          name,
		"risk_tier":     tier,
		"owner_user_id": ownerID,
	}, http.StatusCreated, &res)
	return res
}

// createUsersAndAgent provisions alice + bob principals and one agent
// owned by alice's squad (agents must exist for grant materialization).
func createAgentOf(t *testing.T, handler http.Handler, authz, squadName string) domain.Agent {
	t.Helper()
	var squad domain.Squad
	doJSONAuth(t, handler, authz, http.MethodPost, pathSquads, map[string]any{"name": squadName}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, authz, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "worker-" + squadName}, http.StatusCreated, &agent)
	return agent
}

func userIDOf(t *testing.T, handler http.Handler, authz string) string {
	t.Helper()
	var u domain.User
	doJSONAuth(t, handler, authz, http.MethodGet, pathAuthMe, nil, http.StatusOK, &u)
	return u.ID
}

func inboxKinds(t *testing.T, store *storage.MemoryStore, userID string) []domain.InboxKind {
	t.Helper()
	msgs, err := store.ListInboxMessages(context.Background(), userID, false, 100)
	require.NoError(t, err)
	kinds := make([]domain.InboxKind, 0, len(msgs))
	for _, m := range msgs {
		kinds = append(kinds, m.Kind)
	}
	return kinds
}

func hasKind(kinds []domain.InboxKind, want domain.InboxKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

func hasGrant(t *testing.T, store *storage.MemoryStore, agentID, resourceID string) bool {
	t.Helper()
	perms, err := store.ListAgentPermissions(context.Background(), agentID)
	require.NoError(t, err)
	for _, p := range perms {
		if p.ResourceID == resourceID {
			return true
		}
	}
	return false
}

// ── 1. medium shared happy path: create → pending_owner → owner approve → grant exists

func TestGrantRequestMediumSharedHappyPath(t *testing.T) {
	t.Parallel()
	handler, store := grantHarness(t, func(before, after *GrantChangeSnapshot) []grantlint.Finding { return nil })
	alice := userIDOf(t, handler, authAlice)
	bob := userIDOf(t, handler, authBob)
	res := createOwnedResource(t, handler, "medium-shared", "medium", alice)
	agent := createAgentOf(t, handler, authBob, "bobsquad")

	var created domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusCreated, &created)
	require.Equal(t, domain.GrantRequestPendingOwner, created.State)
	require.Equal(t, "medium", created.Tier)
	require.Empty(t, created.Findings)
	require.False(t, hasGrant(t, store, agent.ID, res.ID))

	// Owner sees it in their queue.
	var mine []domain.GrantRequest
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/grant-requests?mine=owner", nil, http.StatusOK, &mine)
	require.Len(t, mine, 1)
	require.Equal(t, created.ID, mine[0].ID)

	// Owner approve → approved + materialized grant.
	var approved domain.GrantRequest
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-owner", nil, http.StatusOK, &approved)
	require.Equal(t, domain.GrantRequestApproved, approved.State)
	require.NotNil(t, approved.ApprovedByOwnerAt)
	require.True(t, hasGrant(t, store, agent.ID, res.ID))

	// Inbox: owner got the action request, requester got the outcome.
	require.True(t, hasKind(inboxKinds(t, store, alice), domain.InboxActionRequired))
	require.True(t, hasKind(inboxKinds(t, store, bob), domain.InboxKind("grant_approved")))
}

// ── 2. high tier two-step: pending_owner → owner approve → pending_admin → admin approve

func TestGrantRequestHighTwoStep(t *testing.T) {
	t.Parallel()
	handler, store := grantHarness(t, nil)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "high-side", "high", alice)
	agent := createAgentOf(t, handler, authBob, "hsquad")

	var created domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusCreated, &created)
	require.Equal(t, domain.GrantRequestPendingOwner, created.State)
	require.Equal(t, "high", created.Tier)

	// Requester cannot self-approve the owner step.
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-owner", nil, http.StatusForbidden, &map[string]any{})

	// Owner approve on high tier hands off to admin — NOT yet approved.
	var stepped domain.GrantRequest
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-owner", nil, http.StatusOK, &stepped)
	require.Equal(t, domain.GrantRequestPendingAdmin, stepped.State)
	require.NotNil(t, stepped.ApprovedByOwnerAt)
	require.Nil(t, stepped.ApprovedByAdminAt)
	require.False(t, hasGrant(t, store, agent.ID, res.ID))

	// Non-admin cannot co-sign.
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-admin", nil, http.StatusForbidden, &map[string]any{})

	// Admin co-sign → approved + materialized.
	var approved domain.GrantRequest
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-admin", nil, http.StatusOK, &approved)
	require.Equal(t, domain.GrantRequestApproved, approved.State)
	require.NotNil(t, approved.ApprovedByAdminAt)
	require.True(t, hasGrant(t, store, agent.ID, res.ID))

	// Admins were notified for the co-sign step.
	require.True(t, hasKind(inboxKinds(t, store, userIDOf(t, handler, authAdmin)), domain.InboxActionRequired))
}

// ── 3. BYO auto-approve: no request row, grant materialized directly

func TestGrantRequestBYOAutoApprove(t *testing.T) {
	t.Parallel()
	handler, store := grantHarness(t, func(before, after *GrantChangeSnapshot) []grantlint.Finding { return nil })
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "byo-medium", "medium", alice)
	agent := createAgentOf(t, handler, authAlice, "alicesquad")

	var resp struct {
		State        string `json:"state"`
		AutoApproved bool   `json:"auto_approved"`
	}
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusOK, &resp)
	require.Equal(t, string(domain.GrantRequestApproved), resp.State)
	require.True(t, resp.AutoApproved)

	// Spec: BYO with no findings leaves NO request row.
	var mine []domain.GrantRequest
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/grant-requests?mine=owner", nil, http.StatusOK, &mine)
	require.Empty(t, mine)

	require.True(t, hasGrant(t, store, agent.ID, res.ID))
}

// ── 4. findings force review: block finding on BYO → pending_owner with findings

func TestGrantRequestFindingsForceReview(t *testing.T) {
	t.Parallel()
	blocker := func(before, after *GrantChangeSnapshot) []grantlint.Finding {
		return []grantlint.Finding{{Code: "new_credentialed_reach", Severity: "block", Detail: "agent gains credentialed reach"}}
	}
	handler, store := grantHarness(t, blocker)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "flagged-byo", "low", alice)
	agent := createAgentOf(t, handler, authAlice, "flagged-squad")

	// BYO + low tier would auto-approve without findings; the block
	// finding overrides auto-approval.
	var created domain.GrantRequest
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusCreated, &created)
	require.Equal(t, domain.GrantRequestPendingOwner, created.State)
	require.Len(t, created.Findings, 1)
	require.Equal(t, "block", created.Findings[0].Severity)
	require.Equal(t, "new_credentialed_reach", created.Findings[0].Code)
	require.False(t, hasGrant(t, store, agent.ID, res.ID))

	// Owner reviews the findings and approves → grant materializes.
	var approved domain.GrantRequest
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-owner", nil, http.StatusOK, &approved)
	require.Equal(t, domain.GrantRequestApproved, approved.State)
	require.True(t, hasGrant(t, store, agent.ID, res.ID))
}

// ── 5. invalid transitions → 409

func TestGrantRequestInvalidTransition(t *testing.T) {
	t.Parallel()
	handler, _ := grantHarness(t, nil)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "transition-res", "medium", alice)
	agent := createAgentOf(t, handler, authBob, "tsquad")

	var created domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusCreated, &created)
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-owner", nil, http.StatusOK, &map[string]any{})

	// Terminal: re-approve and post-approval deny are both 409.
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-owner", nil, http.StatusConflict, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/deny", map[string]any{"reason": "too late"}, http.StatusConflict, &map[string]any{})
	// Admin approve on a request that never reached pending_admin: 409.
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-admin", nil, http.StatusConflict, &map[string]any{})
}

// ── 6. wrong-owner approve-owner → 403

func TestGrantRequestWrongOwnerForbidden(t *testing.T) {
	t.Parallel()
	handler, _ := grantHarness(t, nil)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "owned-by-alice", "medium", alice)
	agent := createAgentOf(t, handler, authBob, "wo-squad")

	var created domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusCreated, &created)

	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-owner", nil, http.StatusForbidden, &map[string]any{})
}

// ── 7. deny from pending_admin (admin authority) + requester notified

func TestGrantRequestDenyFromPendingAdmin(t *testing.T) {
	t.Parallel()
	handler, store := grantHarness(t, nil)
	alice := userIDOf(t, handler, authAlice)
	bob := userIDOf(t, handler, authBob)
	res := createOwnedResource(t, handler, "deny-high", "high", alice)
	agent := createAgentOf(t, handler, authBob, "deny-squad")

	var created domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusCreated, &created)
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-owner", nil, http.StatusOK, &map[string]any{})

	// Non-admin cannot deny at the admin step.
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/deny", map[string]any{"reason": "nope"}, http.StatusForbidden, &map[string]any{})

	var denied domain.GrantRequest
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/deny", map[string]any{"reason": "scope too wide"}, http.StatusOK, &denied)
	require.Equal(t, domain.GrantRequestDenied, denied.State)
	require.Equal(t, "scope too wide", denied.DeniedReason)
	require.False(t, hasGrant(t, store, agent.ID, res.ID))
	require.True(t, hasKind(inboxKinds(t, store, bob), domain.InboxKind("grant_denied")))
}

// ── 8. expiry stored when provided

func TestGrantRequestExpiryStored(t *testing.T) {
	t.Parallel()
	handler, _ := grantHarness(t, nil)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "expiry-res", "medium", alice)
	agent := createAgentOf(t, handler, authBob, "exp-squad")
	expiry := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)

	var created domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
		"expiry":      expiry.Format(time.RFC3339),
	}, http.StatusCreated, &created)
	require.NotNil(t, created.Expiry)
	require.True(t, created.Expiry.Equal(expiry))

	var listed []domain.GrantRequest
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/grant-requests?mine=owner", nil, http.StatusOK, &listed)
	require.Len(t, listed, 1)
	require.NotNil(t, listed[0].Expiry)
	require.True(t, listed[0].Expiry.Equal(expiry))
}

// ── 9. list filters + authz on mine=admin

func TestGrantRequestListFilters(t *testing.T) {
	t.Parallel()
	handler, _ := grantHarness(t, nil)
	alice := userIDOf(t, handler, authAlice)
	res := createOwnedResource(t, handler, "filter-res", "medium", alice)
	agent := createAgentOf(t, handler, authBob, "filter-squad")

	var created domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusCreated, &created)

	// mine=admin requires platform_admin.
	doJSONAuth(t, handler, authBob, http.MethodGet, "/api/v1/grant-requests?mine=admin", nil, http.StatusForbidden, &map[string]any{})
	// mine=bogus is a 400.
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/grant-requests?mine=bogus", nil, http.StatusBadRequest, &map[string]any{})

	// Admin sees everything.
	var all []domain.GrantRequest
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/grant-requests?mine=admin", nil, http.StatusOK, &all)
	require.Len(t, all, 1)

	// Default (no mine): bob sees only his own submissions.
	var mine []domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodGet, "/api/v1/grant-requests", nil, http.StatusOK, &mine)
	require.Len(t, mine, 1)
	require.Equal(t, created.ID, mine[0].ID)

	// state filter narrows to nothing once approved.
	var none []domain.GrantRequest
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/grant-requests?mine=owner&state=approved", nil, http.StatusOK, &none)
	require.Empty(t, none)
}

// ── 10. unknown request / resource → 404; missing fields → 400

func TestGrantRequestNotFoundAndValidation(t *testing.T) {
	t.Parallel()
	handler, _ := grantHarness(t, nil)
	agent := createAgentOf(t, handler, authAlice, "nf-squad")

	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests/22222222-2222-2222-2222-222222222222/approve-owner", nil, http.StatusNotFound, &map[string]any{})
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests", map[string]any{"agent_id": agent.ID}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests", map[string]any{"resource_id": "11111111-1111-1111-1111-111111111111"}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authAlice, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": "11111111-1111-1111-1111-111111111111",
		"agent_id":    agent.ID,
	}, http.StatusNotFound, &map[string]any{})
}

// ── 11. owner-less resource routes straight to pending_admin

func TestGrantRequestOwnerlessRoutesToAdmin(t *testing.T) {
	t.Parallel()
	handler, store := grantHarness(t, nil)
	res := createOwnedResource(t, handler, "no-owner", "medium", "")
	agent := createAgentOf(t, handler, authBob, "orphan-squad")

	var created domain.GrantRequest
	doJSONAuth(t, handler, authBob, http.MethodPost, "/api/v1/grant-requests", map[string]any{
		"resource_id": res.ID,
		"agent_id":    agent.ID,
	}, http.StatusCreated, &created)
	require.Equal(t, domain.GrantRequestPendingAdmin, created.State)

	// Owner step is not available to anyone but effectively skipped;
	// admin approves directly.
	var approved domain.GrantRequest
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/grant-requests/"+created.ID+"/approve-admin", nil, http.StatusOK, &approved)
	require.Equal(t, domain.GrantRequestApproved, approved.State)
	require.True(t, hasGrant(t, store, agent.ID, res.ID))
}
