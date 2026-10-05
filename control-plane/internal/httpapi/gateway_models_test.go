// S-GWREG tests: automatic LiteLLM gateway model-deployment
// registration (fail-loud) + model drift reconcile.
//
// The fake gateway is the shared recordingGateway (extended with the
// /model/* surface). The K8s rollout restart is a counting fake — the
// real kube.GatewayReloader is exercised against the API server in
// production only.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

type countingReloader struct {
	mu    sync.Mutex
	calls int
}

// jsonBody marshals a request body for httptest.NewRequest.
func jsonBody(v any) *bytes.Reader {
	payload, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(payload)
}

func (r *countingReloader) ReloadGateway(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

func (r *countingReloader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// newGWRegHarness returns an OIDC-admin handler wired to a recording
// gateway and a counting reloader.
func newGWRegHarness(t *testing.T, gw *recordingGateway) (http.Handler, *storage.MemoryStore, *countingReloader) {
	t.Helper()
	server := httptest.NewServer(gw.handler())
	t.Cleanup(server.Close)
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.LiteLLMAdminURL = server.URL
	cfg.LiteLLMMasterKey = "sk-test-master"
	store := storage.NewMemoryStore()
	reloader := &countingReloader{}
	handler := NewWithGatewayReload(cfg, store, headerOIDC{
		authAdmin: {Email: adminEmail, Name: "Admin"},
		authAlice: {Email: aliceEmail, Name: "Alice"},
	}, reloader)
	promoteAdmin(t, store, handler, authAdmin)
	return handler, store, reloader
}

// createKeyedProvider registers a provider with a (fake) API key so the
// resolved litellm_params.api_key is observable.
func createKeyedProvider(t *testing.T, handler http.Handler, name string) domain.AIProvider {
	t.Helper()
	var provider domain.AIProvider
	doJSONAuth(t, handler, authAdmin, http.MethodPost, pathProviders, map[string]any{
		"name":     name,
		"kind":     "openai",
		"base_url": "http://" + name + ".invalid/v1",
		"api_key":  "sk-fake-provider-key",
	}, http.StatusCreated, &provider)
	return provider
}

func litellmParamsOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	params, ok := body["litellm_params"].(map[string]any)
	require.True(t, ok, "request body must carry litellm_params: %v", body)
	return params
}

func decodeErrorEnvelope(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body: %s", body)
	env, ok := out["error"].(map[string]any)
	require.True(t, ok, "expected error envelope: %s", body)
	return env
}

// --- prefix mapping ---------------------------------------------------

func TestLitellmModelPrefixMapping(t *testing.T) {
	cases := []struct {
		kind   string
		prefix string
		ok     bool
	}{
		{"openai", "openai", true},
		{"anthropic", "anthropic", true},
		{"gemini", "gemini", true},
		{"ollama_chat", "ollama_chat", true},
		{"ollama", "ollama", true},
		{"azure", "azure", true},
		{"openai-compatible", "", false},
		{"", "", false},
		{"weird", "", false},
	}
	for _, tc := range cases {
		prefix, ok := litellmModelPrefix(tc.kind)
		require.Equal(t, tc.ok, ok, "kind=%q", tc.kind)
		require.Equal(t, tc.prefix, prefix, "kind=%q", tc.kind)
	}
}

// --- create: happy path ------------------------------------------------

func TestCreateAIModelRegistersGatewayDeployment(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, store, reloader := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "gwreg-prov")

	model := createTestAIModel(t, handler, provider.ID, "gwreg-model")
	require.Equal(t, "gwreg-model", model.ModelName)

	gw.mu.Lock()
	defer gw.mu.Unlock()
	require.Len(t, gw.modelDeployed, 1)
	body := gw.modelDeployed[0]
	require.Equal(t, "gwreg-model", body["model_name"])
	params := litellmParamsOf(t, body)
	require.Equal(t, "openai/gwreg-model", params["model"])
	require.Equal(t, provider.BaseURL, params["api_base"])
	require.Equal(t, "sk-fake-provider-key", params["api_key"])

	stored, err := store.GetAIModel(context.Background(), model.ID)
	require.NoError(t, err)
	require.Equal(t, domain.ResourceActive, stored.Status)
	require.Equal(t, 1, reloader.count(), "one rollout restart per create")
}

// --- create: fail-loud --------------------------------------------------

func TestCreateAIModelGatewayFailureWritesNoRegistryRow(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{failModelDeploy: true}
	handler, store, reloader := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "gwreg-dead")

	req := httptest.NewRequest(http.MethodPost, pathAIModels, jsonBody(map[string]any{
		"provider_id": provider.ID,
		"model_name":  "doomed-model",
		"pricing":     validPricing(),
	}))
	req.Header.Set("Authorization", authAdmin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	env := decodeErrorEnvelope(t, rec.Body.String())
	require.Equal(t, "gateway_provision_failed", env["code"])

	models, err := store.ListAIModels(context.Background(), "")
	require.NoError(t, err)
	for _, m := range models {
		require.NotEqual(t, "doomed-model", m.ModelName, "registry row must NOT exist after gateway failure")
	}
	require.Equal(t, 0, reloader.count(), "no reload when nothing was deployed")
}

func TestCreateAIModelUnknownProviderKindFailsLoud(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, store, _ := newGWRegHarness(t, gw)
	var provider domain.AIProvider
	doJSONAuth(t, handler, authAdmin, http.MethodPost, pathProviders, map[string]any{
		"name":     "gwreg-weird",
		"kind":     "quantum",
		"base_url": "http://gwreg-weird.invalid/v1",
	}, http.StatusCreated, &provider)

	req := httptest.NewRequest(http.MethodPost, pathAIModels, jsonBody(map[string]any{
		"provider_id": provider.ID,
		"model_name":  "weird-model",
		"pricing":     validPricing(),
	}))
	req.Header.Set("Authorization", authAdmin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Equal(t, "gateway_provision_failed", decodeErrorEnvelope(t, rec.Body.String())["code"])
	models, err := store.ListAIModels(context.Background(), "")
	require.NoError(t, err)
	require.Empty(t, models)
}

// --- update: rename / provider change -----------------------------------

func TestUpdateAIModelRenameReplacesGatewayDeployment(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, _, reloader := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "gwreg-rename")
	model := createTestAIModel(t, handler, provider.ID, "old-name")
	require.Equal(t, 1, reloader.count())

	var renamed domain.AIModel
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, pathAIModelItem+model.ID, map[string]any{
		"model_name": "new-name",
	}, http.StatusOK, &renamed)
	require.Equal(t, "new-name", renamed.ModelName)

	gw.mu.Lock()
	defer gw.mu.Unlock()
	require.Len(t, gw.modelDeployed, 2, "rename creates the new deployment")
	require.Equal(t, "new-name", gw.modelDeployed[1]["model_name"])
	require.Len(t, gw.modelDeleted, 1, "rename deletes the old deployment")
	require.Contains(t, gw.modelLive, "new-name")
	require.NotContains(t, gw.modelLive, "old-name")
	require.Empty(t, gw.modelUpdated, "rename must not use /model/update")
	require.Equal(t, 2, reloader.count(), "one reload per mutation")
}

func TestUpdateAIModelProviderChangeUsesModelUpdate(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, _, reloader := newGWRegHarness(t, gw)
	providerA := createKeyedProvider(t, handler, "gwreg-a")
	providerB := createKeyedProvider(t, handler, "gwreg-b")
	model := createTestAIModel(t, handler, providerA.ID, "stable-name")

	var patched domain.AIModel
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, pathAIModelItem+model.ID, map[string]any{
		"provider_id": providerB.ID,
	}, http.StatusOK, &patched)
	require.Equal(t, providerB.ID, patched.ProviderID)

	gw.mu.Lock()
	defer gw.mu.Unlock()
	require.Len(t, gw.modelUpdated, 1, "same-name provider change uses /model/update")
	require.Equal(t, providerB.BaseURL, litellmParamsOf(t, gw.modelUpdated[0])["api_base"])
	require.Len(t, gw.modelDeleted, 0)
	require.Equal(t, 2, reloader.count())
}

func TestUpdateAIModelGatewayFailureKeepsRegistry(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, store, _ := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "gwreg-keep")
	model := createTestAIModel(t, handler, provider.ID, "keep-me")

	gw.mu.Lock()
	gw.failModelDeploy = true
	gw.mu.Unlock()

	req := httptest.NewRequest(http.MethodPatch, pathAIModelItem+model.ID, jsonBody(map[string]any{
		"model_name": "broken-rename",
	}))
	req.Header.Set("Authorization", authAdmin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Equal(t, "gateway_provision_failed", decodeErrorEnvelope(t, rec.Body.String())["code"])

	stored, err := store.GetAIModel(context.Background(), model.ID)
	require.NoError(t, err)
	require.Equal(t, "keep-me", stored.ModelName, "registry keeps the old working config on gateway failure")
}

// --- display-only PATCH must not touch the gateway ----------------------

func TestUpdateAIModelDisplayOnlySkipsGateway(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, _, reloader := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "gwreg-display")
	model := createTestAIModel(t, handler, provider.ID, "display-model")
	before := reloader.count()

	var patched domain.AIModel
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, pathAIModelItem+model.ID, map[string]any{
		"display_name": "Much Nicer Name",
	}, http.StatusOK, &patched)
	require.Equal(t, before, reloader.count(), "display-only PATCH triggers no gateway work")
}

// --- reconcile ---------------------------------------------------------

func TestReconcileGatewayModelsRegistersMissingAndReportsExtras(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, _, reloader := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "gwreg-recon")
	createTestAIModel(t, handler, provider.ID, "recon-a")
	createTestAIModel(t, handler, provider.ID, "recon-b")

	// Simulate drift: modelA's deployment vanished from the gateway and
	// a ghost deployment exists with no registry entry.
	gw.mu.Lock()
	delete(gw.modelLive, "recon-a")
	gw.modelLive["ghost-model"] = "dep-ghost"
	gw.mu.Unlock()
	before := reloader.count()

	var report gatewayModelReconcileReport
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/admin/gateway/models/reconcile", nil, http.StatusOK, &report)

	require.Equal(t, []string{"recon-a"}, report.Registered)
	require.Contains(t, report.AlreadyPresent, "recon-b")
	require.Empty(t, report.Failed)
	require.Len(t, report.Extras, 1)
	require.Equal(t, "ghost-model", report.Extras[0].ModelName)
	require.Equal(t, "dep-ghost", report.Extras[0].DeploymentID)
	require.Equal(t, before+1, reloader.count(), "exactly one reload when something was registered")

	// Extras must NOT be deleted.
	gw.mu.Lock()
	defer gw.mu.Unlock()
	require.Contains(t, gw.modelLive, "ghost-model")
	require.Contains(t, gw.modelLive, "recon-a")
}

func TestReconcileGatewayModelsNoopWithoutDrift(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, _, reloader := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "gwreg-clean")
	createTestAIModel(t, handler, provider.ID, "clean-model")
	before := reloader.count()

	var report gatewayModelReconcileReport
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/admin/gateway/models/reconcile", nil, http.StatusOK, &report)
	require.Empty(t, report.Registered)
	require.Contains(t, report.AlreadyPresent, "clean-model")
	require.Empty(t, report.Extras)
	require.Equal(t, before, reloader.count(), "no reload when nothing was registered")
}

func TestReconcileReportsPerModelFailures(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, _, _ := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "gwreg-part")
	createTestAIModel(t, handler, provider.ID, "part-a")

	// Drift: part-a vanished; add a second model whose registration will
	// fail (gateway starts failing /model/new).
	gw.mu.Lock()
	delete(gw.modelLive, "part-a")
	gw.mu.Unlock()
	var modelB domain.AIModel
	// part-b registers fine first, then we remove it and break the gateway.
	doJSONAuth(t, handler, authAdmin, http.MethodPost, pathAIModels, map[string]any{
		"provider_id": provider.ID,
		"model_name":  "part-b",
		"pricing":     validPricing(),
	}, http.StatusCreated, &modelB)
	gw.mu.Lock()
	delete(gw.modelLive, "part-a")
	delete(gw.modelLive, "part-b")
	gw.failModelDeploy = true
	gw.mu.Unlock()

	var report gatewayModelReconcileReport
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/admin/gateway/models/reconcile", nil, http.StatusOK, &report)
	require.Len(t, report.Failed, 2)
	require.Empty(t, report.Registered)
}

// --- dev mode (gateway unconfigured) ------------------------------------

func TestCreateAIModelWithoutGatewayConfiguredSucceeds(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.LiteLLMAdminURL = ""
	cfg.LiteLLMMasterKey = ""
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authAdmin: {Email: adminEmail, Name: "Admin"},
	})
	promoteAdmin(t, store, handler, authAdmin)
	provider := createTestProvider(t, handler, "dev-prov")
	model := createTestAIModel(t, handler, provider.ID, "dev-model")
	stored, err := store.GetAIModel(context.Background(), model.ID)
	require.NoError(t, err)
	require.Equal(t, "dev-model", stored.ModelName, "dev mode keeps the pre-S-GWREG skip-with-warning behaviour")
}

func TestReconcileWithoutGatewayConfiguredReturns503(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.LiteLLMAdminURL = ""
	cfg.LiteLLMMasterKey = ""
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authAdmin: {Email: adminEmail, Name: "Admin"},
	})
	promoteAdmin(t, store, handler, authAdmin)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gateway/models/reconcile", nil)
	req.Header.Set("Authorization", authAdmin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
}

// --- auth ---------------------------------------------------------------

func TestReconcileGatewayModelsRequiresPlatformAdmin(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, _, _ := newGWRegHarness(t, gw)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/gateway/models/reconcile", nil)
	req.Header.Set("Authorization", authAlice)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}
