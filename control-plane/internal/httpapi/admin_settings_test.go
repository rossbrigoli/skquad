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

// S-212 (ADR-0013 §5): embedder runtime override via admin settings.

type fakeEmbedderConfigWriter struct {
	written []string
	err     error
}

func (f *fakeEmbedderConfigWriter) SetEmbedderRuntime(_ context.Context, runtime string) error {
	if f.err != nil {
		return f.err
	}
	f.written = append(f.written, runtime)
	return nil
}

func newEmbedderRuntimeFixture(t *testing.T) (http.Handler, *storage.MemoryStore, *fakeEmbedderConfigWriter) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	fake := &fakeEmbedderConfigWriter{}
	handler := NewWithEmbedderRuntimeWriter(cfg, store, headerOIDC{
		authOwner: {Issuer: testIssuer, Subject: "own-1", Email: "owner@example.com", EmailVerified: true, Name: "Owner"},
		authAdmin: {Issuer: testIssuer, Subject: "adm-1", Email: adminEmail, EmailVerified: true, Name: "Admin", Groups: []string{platformAdminGroup}},
		authAlice: {Issuer: testIssuer, Subject: "alice-1", Email: aliceEmail, EmailVerified: true, Name: "Alice"},
	}, &fakeCRWriter{}, fake)
	return handler, store, fake
}

func TestAdminSettingsEmbedderRuntimeDefaultAuto(t *testing.T) {
	handler, _ := newIdleSettingsFixture(t)
	var view adminSettingsView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/settings", nil, http.StatusOK, &view)
	require.Equal(t, "auto", view.EmbedderRuntime)
}

func TestAdminSettingsEmbedderRuntimeSet(t *testing.T) {
	handler, store, fake := newEmbedderRuntimeFixture(t)
	ctx := context.Background()

	var out map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings",
		map[string]any{"embedder_runtime": "cuda"}, http.StatusOK, &out)
	require.Equal(t, "cuda", out["embedder_runtime"])
	require.Equal(t, []string{"cuda"}, fake.written)
	raw, found, err := store.GetPlatformSetting(ctx, domain.PlatformSettingEmbedderRuntime)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "cuda", raw)

	// GET reflects the stored choice.
	var view adminSettingsView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/settings", nil, http.StatusOK, &view)
	require.Equal(t, "cuda", view.EmbedderRuntime)

	// Case-insensitive input is normalized to lowercase.
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings",
		map[string]any{"embedder_runtime": " Vulkan "}, http.StatusOK, &out)
	require.Equal(t, "vulkan", out["embedder_runtime"])
	require.Equal(t, []string{"cuda", "vulkan"}, fake.written)
}

func TestAdminSettingsEmbedderRuntimeValidation(t *testing.T) {
	handler, store, fake := newEmbedderRuntimeFixture(t)
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings",
		map[string]any{"embedder_runtime": "quantum"}, http.StatusBadRequest, &map[string]any{})
	require.Empty(t, fake.written)
	_, found, err := store.GetPlatformSetting(context.Background(), domain.PlatformSettingEmbedderRuntime)
	require.NoError(t, err)
	require.False(t, found, "invalid value must not be stored")
}

func TestAdminSettingsEmbedderRuntimeWriterUnavailable(t *testing.T) {
	// Fixture WITHOUT the writer (dev mode): PUT must 503, not silently store.
	handler, store := newIdleSettingsFixture(t)
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings",
		map[string]any{"embedder_runtime": "cuda"}, http.StatusServiceUnavailable, &map[string]any{})
	_, found, err := store.GetPlatformSetting(context.Background(), domain.PlatformSettingEmbedderRuntime)
	require.NoError(t, err)
	require.False(t, found)
}

func TestAdminSettingsEmbedderRuntimeWriterFailureStoresNothing(t *testing.T) {
	handler, store, fake := newEmbedderRuntimeFixture(t)
	fake.err = context.DeadlineExceeded
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings",
		map[string]any{"embedder_runtime": "cpu"}, http.StatusServiceUnavailable, &map[string]any{})
	_, found, err := store.GetPlatformSetting(context.Background(), domain.PlatformSettingEmbedderRuntime)
	require.NoError(t, err)
	require.False(t, found, "ConfigMap write failure must not store the setting")
}

func TestAdminSettingsBothFieldsTogether(t *testing.T) {
	handler, _, fake := newEmbedderRuntimeFixture(t)
	var out map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings",
		map[string]any{"embedder_runtime": "cpu", "idle_scale_to_zero_minutes": 30}, http.StatusOK, &out)
	require.Equal(t, "cpu", out["embedder_runtime"])
	require.EqualValues(t, 30, out["idle_scale_to_zero_minutes"])
	require.Contains(t, out, "mirrored_agents")
	require.Equal(t, []string{"cpu"}, fake.written)
}
