package httpapi

// S-287: platform-configurable agent pod resources — API surface tests.
// Separate file from admin_settings_test.go to keep the new block
// self-contained (fake writer + fixture + cases).

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

type fakeAgentResourcesWriter struct {
	written []domain.AgentResourceSettings
	err     error
}

func (f *fakeAgentResourcesWriter) SetAgentResources(_ context.Context, res domain.AgentResourceSettings) error {
	if f.err != nil {
		return f.err
	}
	f.written = append(f.written, res)
	return nil
}

func newAgentResourcesFixture(t *testing.T) (http.Handler, *storage.MemoryStore, *fakeAgentResourcesWriter) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	fake := &fakeAgentResourcesWriter{}
	handler := NewWithAgentResourcesWriter(cfg, store, headerOIDC{
		authOwner: {Issuer: testIssuer, Subject: "own-1", Email: "owner@example.com", EmailVerified: true, Name: "Owner"},
		authAdmin: {Issuer: testIssuer, Subject: "adm-1", Email: adminEmail, EmailVerified: true, Name: "Admin", Groups: []string{platformAdminGroup}},
		authAlice: {Issuer: testIssuer, Subject: "alice-1", Email: aliceEmail, EmailVerified: true, Name: "Alice"},
	}, &fakeCRWriter{}, fake)
	return handler, store, fake
}

func validAgentResourcesBody() map[string]any {
	return map[string]any{
		"agent_resources": map[string]any{
			"cpu_request":    "250m",
			"cpu_limit":      "1",
			"memory_request": "256Mi",
			"memory_limit":   "1Gi",
		},
	}
}

func TestAgentResourcesGetDefaults(t *testing.T) {
	handler, _, _ := newAgentResourcesFixture(t)
	var view adminSettingsView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/settings", nil, http.StatusOK, &view)
	require.Equal(t, "100m", view.AgentResources.CPURequest)
	require.Equal(t, "500m", view.AgentResources.CPULimit)
	require.Equal(t, "128Mi", view.AgentResources.MemoryRequest)
	require.Equal(t, "512Mi", view.AgentResources.MemoryLimit)
}

func TestAgentResourcesSetPersistsAndMirrors(t *testing.T) {
	handler, store, fake := newAgentResourcesFixture(t)
	var out adminSettingsView
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", validAgentResourcesBody(), http.StatusOK, &out)
	require.Equal(t, "250m", out.AgentResources.CPURequest)
	require.Equal(t, "1", out.AgentResources.CPULimit)
	require.Equal(t, "256Mi", out.AgentResources.MemoryRequest)
	require.Equal(t, "1Gi", out.AgentResources.MemoryLimit)

	require.Len(t, fake.written, 1, "operator ConfigMap must be written")
	require.Equal(t, domain.AgentResourceSettings{
		CPURequest: "250m", CPULimit: "1", MemoryRequest: "256Mi", MemoryLimit: "1Gi",
	}, fake.written[0])

	ctx := context.Background()
	for key, want := range map[string]string{
		domain.PlatformSettingAgentCPURequest:    "250m",
		domain.PlatformSettingAgentCPULimit:      "1",
		domain.PlatformSettingAgentMemoryRequest: "256Mi",
		domain.PlatformSettingAgentMemoryLimit:   "1Gi",
	} {
		raw, found, err := store.GetPlatformSetting(ctx, key)
		require.NoError(t, err)
		require.True(t, found, key)
		require.Equal(t, want, raw, key)
	}

	// GET reflects the stored values afterwards.
	var view adminSettingsView
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/admin/settings", nil, http.StatusOK, &view)
	require.Equal(t, "250m", view.AgentResources.CPURequest)
	require.Equal(t, "1Gi", view.AgentResources.MemoryLimit)
}

func TestAgentResourcesValidation(t *testing.T) {
	cases := []struct {
		name     string
		body     map[string]any
		contains string
	}{
		{"bad cpu quantity", map[string]any{"agent_resources": map[string]any{
			"cpu_request": "banana", "cpu_limit": "500m", "memory_request": "128Mi", "memory_limit": "512Mi",
		}}, "cpu_request"},
		{"cpu request over limit", map[string]any{"agent_resources": map[string]any{
			"cpu_request": "600m", "cpu_limit": "500m", "memory_request": "128Mi", "memory_limit": "512Mi",
		}}, "exceeds cpu_limit"},
		{"bad memory quantity", map[string]any{"agent_resources": map[string]any{
			"cpu_request": "100m", "cpu_limit": "500m", "memory_request": "12X", "memory_limit": "512Mi",
		}}, "memory_request"},
		{"memory request over limit", map[string]any{"agent_resources": map[string]any{
			"cpu_request": "100m", "cpu_limit": "500m", "memory_request": "2Gi", "memory_limit": "512Mi",
		}}, "exceeds memory_limit"},
		{"missing field", map[string]any{"agent_resources": map[string]any{
			"cpu_request": "100m", "cpu_limit": "500m", "memory_request": "128Mi",
		}}, "memory_limit"},
		{"empty field", map[string]any{"agent_resources": map[string]any{
			"cpu_request": "", "cpu_limit": "500m", "memory_request": "128Mi", "memory_limit": "512Mi",
		}}, "cpu_request"},
	}
	for _, tc := range cases {
		handler, store, fake := newAgentResourcesFixture(t)
		var out map[string]any
		doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", tc.body, http.StatusBadRequest, &out)
		require.Empty(t, fake.written, tc.name)
		ctx := context.Background()
		_, found, err := store.GetPlatformSetting(ctx, domain.PlatformSettingAgentCPURequest)
		require.NoError(t, err)
		require.False(t, found, tc.name+": nothing must be stored on validation failure")
	}
}

func TestAgentResourcesRequiresWriter(t *testing.T) {
	// Fixture without the K8s override writer (plain deps) → 503.
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	handler := NewWithDependencies(cfg, store, headerOIDC{
		authOwner: {Issuer: testIssuer, Subject: "own-1", Email: "owner@example.com", EmailVerified: true, Name: "Owner"},
		authAdmin: {Issuer: testIssuer, Subject: "adm-1", Email: adminEmail, EmailVerified: true, Name: "Admin", Groups: []string{platformAdminGroup}},
		authAlice: {Issuer: testIssuer, Subject: "alice-1", Email: aliceEmail, EmailVerified: true, Name: "Alice"},
	}, &fakeCRWriter{}, nil)
	var out map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", validAgentResourcesBody(), http.StatusServiceUnavailable, &out)
	_, found, err := store.GetPlatformSetting(context.Background(), domain.PlatformSettingAgentCPURequest)
	require.NoError(t, err)
	require.False(t, found, "no DB write when the ConfigMap writer is unavailable")
}

func TestAgentResourcesWriterFailureStoresNothing(t *testing.T) {
	handler, store, fake := newAgentResourcesFixture(t)
	fake.err = context.DeadlineExceeded
	var out map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", validAgentResourcesBody(), http.StatusServiceUnavailable, &out)
	_, found, err := store.GetPlatformSetting(context.Background(), domain.PlatformSettingAgentCPURequest)
	require.NoError(t, err)
	require.False(t, found, "ConfigMap failure must not store the setting")
}

func TestAgentResourcesRequiresPlatformAdmin(t *testing.T) {
	handler, _, fake := newAgentResourcesFixture(t)
	var out map[string]any
	doJSONAuth(t, handler, authAlice, http.MethodPut, "/api/v1/admin/settings", validAgentResourcesBody(), http.StatusForbidden, &out)
	require.Empty(t, fake.written)
}

func TestAgentResourcesAlongsideOtherSettings(t *testing.T) {
	handler, _, fake := newAgentResourcesFixture(t)
	body := validAgentResourcesBody()
	body["idle_scale_to_zero_minutes"] = 25
	var out map[string]any
	doJSONAuth(t, handler, authAdmin, http.MethodPut, "/api/v1/admin/settings", body, http.StatusOK, &out)
	require.Equal(t, float64(25), out["idle_scale_to_zero_minutes"])
	require.Equal(t, "250m", out["agent_resources"].(map[string]any)["cpu_request"])
	require.Len(t, fake.written, 1)
}
