// S-200 tests: AI-model vision capability round-trip, gateway model_info
// passthrough, and the agent-facing capability read endpoint.

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// Vision capability round-trips through the admin create API and is
// carried into the gateway deployment's model_info.
func TestCreateAIModelCarriesVisionIntoGatewayModelInfo(t *testing.T) {
	t.Parallel()
	gw := &recordingGateway{}
	handler, store, _ := newGWRegHarness(t, gw)
	provider := createKeyedProvider(t, handler, "vision-prov")

	var model domain.AIModel
	doJSONAuth(t, handler, authAdmin, http.MethodPost, pathAIModels, map[string]any{
		"provider_id":     provider.ID,
		"model_name":      "vision-model",
		"supports_vision": true,
		"pricing":         validPricing(),
	}, http.StatusCreated, &model)
	require.True(t, model.SupportsVision, "create response must reflect supports_vision")

	stored, err := store.GetAIModel(context.Background(), model.ID)
	require.NoError(t, err)
	require.True(t, stored.SupportsVision)

	gw.mu.Lock()
	defer gw.mu.Unlock()
	require.Len(t, gw.modelDeployed, 1)
	info, ok := gw.modelDeployed[0]["model_info"].(map[string]any)
	require.True(t, ok, "gateway deployment body must carry model_info: %v", gw.modelDeployed[0])
	require.Equal(t, true, info["supports_vision"])
}

// Default is non-vision (a model that doesn't opt in stays text-only).
func TestCreateAIModelDefaultsToNonVision(t *testing.T) {
	t.Parallel()
	handler, store := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "novision-prov")
	model := createTestAIModel(t, handler, provider.ID, "novision-model")
	require.False(t, model.SupportsVision)
	stored, err := store.GetAIModel(context.Background(), model.ID)
	require.NoError(t, err)
	require.False(t, stored.SupportsVision)
}

// PATCH toggles vision on an existing model.
func TestUpdateAIModelTogglesVision(t *testing.T) {
	t.Parallel()
	handler, store := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "toggle-prov")
	model := createTestAIModel(t, handler, provider.ID, "toggle-model")
	require.False(t, model.SupportsVision)

	var updated domain.AIModel
	doJSONAuth(t, handler, authAdmin, http.MethodPatch, pathAIModels+"/"+model.ID, map[string]any{
		"supports_vision": true,
	}, http.StatusOK, &updated)
	require.True(t, updated.SupportsVision)
	stored, err := store.GetAIModel(context.Background(), model.ID)
	require.NoError(t, err)
	require.True(t, stored.SupportsVision)
}

func getMyModel(t *testing.T, h http.Handler, cred, agentID string) agentModelResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/me/model", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("X-Skquad-Agent-ID", agentID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out agentModelResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// An agent bound to a vision model reports supports_vision true.
func TestGetMyModelBoundVision(t *testing.T) {
	f := newDelegationFixture(t)
	ctx := context.Background()

	provider, err := f.store.CreateAIProvider(ctx, &domain.AIProvider{
		Name: "dm-prov", Kind: "openai", BaseURL: "http://dm.invalid", APIKeyRef: "k",
		Status: domain.ResourceActive,
	})
	require.NoError(t, err)
	model, err := f.store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID: provider.ID, DisplayName: "Visionary", ModelName: "vision-x",
		SupportsVision: true, SupportsTools: true, Pricing: json.RawMessage(`{}`),
		Status: domain.ResourceActive,
	})
	require.NoError(t, err)

	agent, err := f.store.GetAgent(ctx, f.workerID)
	require.NoError(t, err)
	agent.AIModelID = model.ID
	_, err = f.store.UpdateAgent(ctx, agent)
	require.NoError(t, err)

	got := getMyModel(t, f.handler, f.workerCred, f.workerID)
	require.Equal(t, "vision-x", got.ModelName)
	require.True(t, got.SupportsVision)
}

// An agent with no bound model degrades to all-false (never an error).
func TestGetMyModelUnboundIsFalse(t *testing.T) {
	f := newDelegationFixture(t)
	got := getMyModel(t, f.handler, f.workerCred, f.workerID)
	require.False(t, got.SupportsVision)
	require.Equal(t, "", got.ModelName)
}
