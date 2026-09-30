package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-183: platform-admin idle scale-to-zero setting API.
func newIdleSettingsFixture(t *testing.T) (http.Handler, *storage.MemoryStore) {
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

func TestAdminSettingsGetDefault(t *testing.T) {
	handler, _ := newIdleSettingsFixture(t)
	var view adminSettingsView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/settings", nil, http.StatusOK, &view)
	// Memory store seeds the 900s (15 min) default like migration 0026.
	require.Equal(t, 15, view.IdleScaleToZeroMinutes)
}

func TestAdminSettingsUpdateAndFanOut(t *testing.T) {
	handler, store := newIdleSettingsFixture(t)
	ctx := context.Background()

	// One agent so the fan-out count is observable.
	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{"name": "Idle Squad"}, http.StatusCreated, &squad)
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Worker"}, http.StatusCreated, &domain.Agent{})

	var out map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", map[string]any{"idle_scale_to_zero_minutes": 20}, http.StatusOK, &out)
	require.Equal(t, float64(20), out["idle_scale_to_zero_minutes"])
	require.Equal(t, float64(1), out["mirrored_agents"], "one agent mirror expected")

	raw, found, err := store.GetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "1200", raw, "stored in seconds")

	var view adminSettingsView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/settings", nil, http.StatusOK, &view)
	require.Equal(t, 20, view.IdleScaleToZeroMinutes)
}

func TestAdminSettingsValidation(t *testing.T) {
	handler, _ := newIdleSettingsFixture(t)
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", map[string]any{"idle_scale_to_zero_minutes": 0}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", map[string]any{"idle_scale_to_zero_minutes": -3}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", map[string]any{"idle_scale_to_zero_minutes": 241}, http.StatusBadRequest, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", map[string]any{}, http.StatusBadRequest, &map[string]any{})
	// Bounds are inclusive.
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", map[string]any{"idle_scale_to_zero_minutes": 1}, http.StatusOK, &map[string]any{})
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", map[string]any{"idle_scale_to_zero_minutes": 240}, http.StatusOK, &map[string]any{})
}

func TestAdminSettingsRequiresPlatformAdmin(t *testing.T) {
	handler, _ := newIdleSettingsFixture(t)
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/admin/settings", nil, http.StatusForbidden, &map[string]any{})
	doJSONAuth(t, handler, authAlice, http.MethodPut, "/api/v1/admin/settings", map[string]any{"idle_scale_to_zero_minutes": 5}, http.StatusForbidden, &map[string]any{})
}

// A corrupt/zero stored value falls back to the deploy-time default
// (SKQUAD_DEFAULT_IDLE_TIMEOUT = 5 min in testConfig).
func TestAdminSettingsFallsBackToConfigDefault(t *testing.T) {
	handler, store := newIdleSettingsFixture(t)
	require.NoError(t, store.SetPlatformSetting(context.Background(), domain.PlatformSettingIdleScaleToZeroSeconds, "0", "x"))
	var view adminSettingsView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/settings", nil, http.StatusOK, &view)
	require.Equal(t, 5, view.IdleScaleToZeroMinutes)
}
