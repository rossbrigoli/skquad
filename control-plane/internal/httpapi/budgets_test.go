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

// S-203 WP1: budget control-plane API tests.
func newBudgetFixture(t *testing.T) (http.Handler, *storage.MemoryStore) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	handler := NewWithDependencies(cfg, store, headerOIDC{
		authOwner: {Issuer: testIssuer, Subject: "own-1", Email: "owner@example.com", EmailVerified: true, Name: "Owner"},
		authAdmin: {Issuer: testIssuer, Subject: "adm-1", Email: adminEmail, EmailVerified: true, Name: "Admin", Groups: []string{platformAdminGroup}},
		authAlice: {Issuer: testIssuer, Subject: "alice-1", Email: aliceEmail, EmailVerified: true, Name: "Alice"},
	}, &fakeCRWriter{}, nil)
	return handler, store
}

func userIDFor(t *testing.T, handler http.Handler, auth string) string {
	t.Helper()
	var me domain.User
	doJSONAuth(t, handler, auth, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &me)
	require.NotEmpty(t, me.ID)
	return me.ID
}

// Every budget endpoint must be platform-admin only.
func TestBudgetEndpointsRequirePlatformAdmin(t *testing.T) {
	handler, _ := newBudgetFixture(t)
	paths := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/admin/budgets", nil},
		{http.MethodGet, "/api/v1/admin/budgets/platform", nil},
		{http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"max_usd": 10}},
		{http.MethodGet, "/api/v1/admin/budgets/users/some-user", nil},
		{http.MethodPut, "/api/v1/admin/budgets/users/some-user", map[string]any{"monthly_budget_usd": 10}},
	}
	for _, p := range paths {
		doJSONAuth(t, handler, authAlice, p.method, p.path, p.body, http.StatusForbidden, &map[string]any{})
	}
}

func TestPlatformBudgetSetGetClear(t *testing.T) {
	handler, _ := newBudgetFixture(t)

	var empty platformBudgetView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/budgets/platform", nil, http.StatusOK, &empty)
	require.Nil(t, empty.DefaultMonthlyUSD)
	require.Nil(t, empty.MaxUSD)
	require.Nil(t, empty.PlatformMonthlyLimitUSD)

	var out map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{
		"default_monthly_usd":        25.5,
		"max_usd":                    500,
		"platform_monthly_limit_usd": 4000,
	}, http.StatusOK, &out)
	require.Equal(t, 25.5, out["default_monthly_usd"])
	require.Equal(t, 500.0, out["max_usd"])
	require.Equal(t, 4000.0, out["platform_monthly_limit_usd"])

	var got platformBudgetView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/budgets/platform", nil, http.StatusOK, &got)
	require.NotNil(t, got.DefaultMonthlyUSD)
	require.Equal(t, 25.5, *got.DefaultMonthlyUSD)
	require.NotNil(t, got.MaxUSD)
	require.Equal(t, 500.0, *got.MaxUSD)
	require.NotNil(t, got.PlatformMonthlyLimitUSD)
	require.Equal(t, 4000.0, *got.PlatformMonthlyLimitUSD)

	// null clears the knob (back to "no platform limit").
	var clearOut map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{
		"platform_monthly_limit_usd": nil,
	}, http.StatusOK, &clearOut)
	require.NotContains(t, clearOut, "clamped_user_budgets", "clearing the limit must not clamp")
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/budgets/platform", nil, http.StatusOK, &got)
	require.Nil(t, got.PlatformMonthlyLimitUSD)
	// Untouched knobs stay.
	require.NotNil(t, got.MaxUSD)

	// Empty body rejected.
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{}, http.StatusBadRequest, &map[string]any{})
}

func TestPlatformBudgetValidation(t *testing.T) {
	handler, _ := newBudgetFixture(t)
	// Negative rejected.
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"max_usd": -1}, http.StatusBadRequest, &map[string]any{})
	// Default above max rejected (both in one request).
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"max_usd": 50, "default_monthly_usd": 60}, http.StatusBadRequest, &map[string]any{})
	// Default above an already-stored max rejected.
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"max_usd": 50}, http.StatusOK, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"default_monthly_usd": 60}, http.StatusBadRequest, &map[string]any{})
	// Raising the max above the stored default is fine.
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"max_usd": 100, "default_monthly_usd": 60}, http.StatusOK, &map[string]any{})
}

// Epic S-203 req 7: lowering the global max clamps ALL user budgets
// above it in the same operation; equal/below budgets are untouched.
func TestClampOnLowerMax(t *testing.T) {
	handler, store := newBudgetFixture(t)
	ctx := context.Background()
	aliceID := userIDFor(t, handler, authAlice)
	ownerID := userIDFor(t, handler, authOwner)
	adminID := userIDFor(t, handler, authAdmin)

	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+aliceID, map[string]any{"monthly_budget_usd": 100}, http.StatusOK, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+ownerID, map[string]any{"monthly_budget_usd": 50}, http.StatusOK, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+adminID, map[string]any{"monthly_budget_usd": 30}, http.StatusOK, &map[string]any{})

	// Lower the max to 40: 100 and 50 clamp down; 30 (equal-below) untouched.
	var out map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"max_usd": 40}, http.StatusOK, &out)
	require.EqualValues(t, 2, out["clamped_user_budgets"])

	b, err := store.GetUserBudget(ctx, aliceID)
	require.NoError(t, err)
	require.Equal(t, 40.0, b.MonthlyBudgetUSD)
	require.NotEqual(t, "system:default", b.UpdatedBy, "clamped by the admin, not the default path")

	b, err = store.GetUserBudget(ctx, ownerID)
	require.NoError(t, err)
	require.Equal(t, 40.0, b.MonthlyBudgetUSD)

	b, err = store.GetUserBudget(ctx, adminID)
	require.NoError(t, err)
	require.Equal(t, 30.0, b.MonthlyBudgetUSD, "budget below the new max must not change")

	// Re-setting the same max clamps nothing.
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"max_usd": 40}, http.StatusOK, &out)
	require.EqualValues(t, 0, out["clamped_user_budgets"])
}

func TestUserBudgetSetGet(t *testing.T) {
	handler, _ := newBudgetFixture(t)
	aliceID := userIDFor(t, handler, authAlice)

	// Unset budget: null in the view, no error.
	var view userBudgetStatusView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/budgets/users/"+aliceID, nil, http.StatusOK, &view)
	require.Equal(t, aliceID, view.UserID)
	require.Nil(t, view.MonthlyBudgetUSD)
	require.False(t, view.OverBudget)

	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+aliceID, map[string]any{"monthly_budget_usd": 42.25}, http.StatusOK, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/budgets/users/"+aliceID, nil, http.StatusOK, &view)
	require.NotNil(t, view.MonthlyBudgetUSD)
	require.Equal(t, 42.25, *view.MonthlyBudgetUSD)

	// Negative rejected; missing field rejected; unknown user 404.
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+aliceID, map[string]any{"monthly_budget_usd": -5}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+aliceID, map[string]any{}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/11111111-1111-4111-8111-111111111111", map[string]any{"monthly_budget_usd": 5}, http.StatusNotFound, &map[string]any{})
}

func TestUserBudgetExceedsMaxRejected(t *testing.T) {
	handler, _ := newBudgetFixture(t)
	aliceID := userIDFor(t, handler, authAlice)
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"max_usd": 50}, http.StatusOK, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+aliceID, map[string]any{"monthly_budget_usd": 60}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+aliceID, map[string]any{"monthly_budget_usd": 50}, http.StatusOK, &map[string]any{})
}

// The platform default is auto-applied to a user on first login
// (UpsertUser path) and never overwrites an existing budget.
func TestDefaultBudgetAppliedOnFirstLogin(t *testing.T) {
	handler, store := newBudgetFixture(t)
	ctx := context.Background()

	// Alice exists already (auth/me above provisions via login). Set the
	// default AFTER her first login: her row must not be back-filled by
	// the login hook until she logs in again.
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &domain.User{})
	aliceID := userIDFor(t, handler, authAlice)

	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"default_monthly_usd": 20}, http.StatusOK, &map[string]any{})

	// First login after the default exists provisions the budget.
	// (auth/me is served through the same OIDC middleware that calls
	// ensureDefaultBudget after UpsertUser.)
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &domain.User{})
	b, err := store.GetUserBudget(ctx, aliceID)
	require.NoError(t, err)
	require.Equal(t, 20.0, b.MonthlyBudgetUSD)
	require.Equal(t, "system:default", b.UpdatedBy)

	// An explicit budget survives later logins (DO NOTHING semantics).
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+aliceID, map[string]any{"monthly_budget_usd": 15}, http.StatusOK, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/platform", map[string]any{"default_monthly_usd": 99}, http.StatusOK, &map[string]any{})
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &domain.User{})
	b, err = store.GetUserBudget(ctx, aliceID)
	require.NoError(t, err)
	require.Equal(t, 15.0, b.MonthlyBudgetUSD, "explicit budget must not be overwritten by the default")
}

// Budget status must reflect the user's current-month spend over their
// owned squads (WP3 consumption shape).
func TestUserBudgetStatusReflectsSpend(t *testing.T) {
	handler, store := newBudgetFixture(t)
	ctx := context.Background()
	ownerID := userIDFor(t, handler, authOwner)

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{"name": "Budget Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Spender"}, http.StatusCreated, &agent)

	now := time.Now().UTC()
	require.NoError(t, store.RecordMetering(ctx, &domain.MeteringEvent{
		AgentID: agent.ID, SquadID: squad.ID, Model: "m", InputTokens: 100, OutputTokens: 10,
		Cost: 12.5, Currency: "USD", Timestamp: now,
	}))
	// Last month's cost must NOT count toward MTD.
	lastMonth := mtdStartUTC(now).AddDate(0, 0, -1)
	require.NoError(t, store.RecordMetering(ctx, &domain.MeteringEvent{
		AgentID: agent.ID, SquadID: squad.ID, Model: "m", InputTokens: 100, OutputTokens: 10,
		Cost: 99, Currency: "USD", Timestamp: lastMonth,
	}))

	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+ownerID, map[string]any{"monthly_budget_usd": 10}, http.StatusOK, &map[string]any{})

	var view userBudgetStatusView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/budgets/users/"+ownerID, nil, http.StatusOK, &view)
	require.InDelta(t, 12.5, view.MTDCost, 1e-9)
	require.NotNil(t, view.MonthlyBudgetUSD)
	require.InDelta(t, -2.5, *view.RemainingUSD, 1e-9)
	require.True(t, view.OverBudget)
}

func TestListBudgetsAdminOverview(t *testing.T) {
	handler, _ := newBudgetFixture(t)
	aliceID := userIDFor(t, handler, authAlice)
	userIDFor(t, handler, authOwner) // provision the owner user row
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/budgets/users/"+aliceID, map[string]any{"monthly_budget_usd": 33}, http.StatusOK, &map[string]any{})

	var out struct {
		Platform platformBudgetView     `json:"platform"`
		Users    []userBudgetStatusView `json:"users"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/budgets", nil, http.StatusOK, &out)
	require.Len(t, out.Users, 3) // owner, admin, alice
	var alice *userBudgetStatusView
	for i := range out.Users {
		if out.Users[i].UserID == aliceID {
			alice = &out.Users[i]
		}
	}
	require.NotNil(t, alice)
	require.NotNil(t, alice.MonthlyBudgetUSD)
	require.Equal(t, 33.0, *alice.MonthlyBudgetUSD)
}
