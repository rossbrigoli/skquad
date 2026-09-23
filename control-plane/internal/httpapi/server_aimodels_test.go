// WP2 tests: AI Model CRUD, user-level model grants, /models/me, and the
// closed llm_provider grant path (ADR-0010 / S-107).

package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// doAdminNoBody performs an admin-authenticated request expecting no
// response body (204-style endpoints; doJSONNoBody carries no auth).
func doAdminNoBody(t *testing.T, handler http.Handler, method, path string, wantStatus int) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
}

// newAIModelHarness returns an OIDC-mode handler with one promoted admin and
// two plain users (alice, bob).
func newAIModelHarness(t *testing.T) (http.Handler, *storage.MemoryStore) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer admin": {Email: "admin@example.com", Name: "Admin"},
		"Bearer alice": {Email: "alice@example.com", Name: "Alice"},
		"Bearer bob":   {Email: "bob@example.com", Name: "Bob"},
	})
	promoteAdmin(t, store, handler, "Bearer admin")
	return handler, store
}

func validPricing() map[string]any {
	return map[string]any{
		"input_per_1m":        2.5,
		"cached_input_per_1m": 0.30,
		"cache_write_per_1m":  3.75,
		"output_per_1m":       10.0,
	}
}

func createTestProvider(t *testing.T, handler http.Handler, name string) domain.LLMProvider {
	t.Helper()
	var provider domain.LLMProvider
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":     name,
		"kind":     "openai",
		"base_url": "http://" + name + ".invalid/v1",
	}, http.StatusCreated, &provider)
	return provider
}

func createTestAIModel(t *testing.T, handler http.Handler, providerID, modelName string) domain.AIModel {
	t.Helper()
	body := map[string]any{
		"provider_id": providerID,
		"model_name":  modelName,
		"pricing":     validPricing(),
	}
	var model domain.AIModel
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/ai-models", body, http.StatusCreated, &model)
	return model
}

func modelIDs(models []domain.AIModel) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestAIModelAdminOnlySurfaces(t *testing.T) {
	t.Parallel()
	handler, _ := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "admin-only-prov")
	model := createTestAIModel(t, handler, provider.ID, "admin-only-model")

	assertForbidden := func(method, path string, body any) {
		var denied map[string]map[string]string
		doJSONAuth(t, handler, "Bearer alice", method, path, body, http.StatusForbidden, &denied)
		require.Equal(t, "forbidden", denied["error"]["code"], method+" "+path)
	}
	assertForbidden(http.MethodGet, "/api/v1/ai-models", nil)
	assertForbidden(http.MethodPost, "/api/v1/ai-models", map[string]any{"provider_id": provider.ID, "model_name": "x", "pricing": validPricing()})
	assertForbidden(http.MethodGet, "/api/v1/ai-models/"+model.ID, nil)
	assertForbidden(http.MethodPatch, "/api/v1/ai-models/"+model.ID, map[string]any{"display_name": "nope"})
	assertForbidden(http.MethodPost, "/api/v1/ai-models/"+model.ID+"/deprecate", nil)
	assertForbidden(http.MethodDelete, "/api/v1/ai-models/"+model.ID, nil)
	assertForbidden(http.MethodGet, "/api/v1/users/admin-id/models", nil)
	assertForbidden(http.MethodPut, "/api/v1/users/admin-id/models", map[string]any{"model_ids": []string{model.ID}})

	// Admin can use the same surfaces.
	var models []domain.AIModel
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/ai-models", nil, http.StatusOK, &models)
	require.Len(t, models, 1)
	var fetched domain.AIModel
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/ai-models/"+model.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, model.ID, fetched.ID)

	// Self-service read is available to non-admins.
	var mine []domain.AIModel
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/models/me", nil, http.StatusOK, &mine)
	require.Empty(t, mine, "ungranted user sees no models")
}

func TestAIModelCreateValidation(t *testing.T) {
	t.Parallel()
	handler, _ := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "validate-prov")

	cases := []struct {
		name        string
		body        map[string]any
		wantMessage string
	}{
		{"missing provider", map[string]any{"model_name": "m", "pricing": validPricing()}, "provider_id is required"},
		{"missing model_name", map[string]any{"provider_id": provider.ID, "pricing": validPricing()}, "model_name is required"},
		{"unknown provider", map[string]any{"provider_id": "nope", "model_name": "m", "pricing": validPricing()}, "provider_id must reference an existing provider"},
		{"missing pricing", map[string]any{"provider_id": provider.ID, "model_name": "m"}, "pricing is required"},
		{"missing rate", map[string]any{"provider_id": provider.ID, "model_name": "m", "pricing": map[string]any{
			"input_per_1m": 1.0, "cache_write_per_1m": 2.0, "output_per_1m": 3.0,
		}}, "cached_input_per_1m is required in pricing"},
		{"non-numeric rate", map[string]any{"provider_id": provider.ID, "model_name": "m", "pricing": map[string]any{
			"input_per_1m": "free", "cached_input_per_1m": 0.3, "cache_write_per_1m": 3.75, "output_per_1m": 10.0,
		}}, "input_per_1m in pricing must be numeric"},
		{"negative rate", map[string]any{"provider_id": provider.ID, "model_name": "m", "pricing": map[string]any{
			"input_per_1m": 2.5, "cached_input_per_1m": 0.3, "cache_write_per_1m": 3.75, "output_per_1m": -1,
		}}, "output_per_1m in pricing must be non-negative"},
		{"negative threshold", map[string]any{"provider_id": provider.ID, "model_name": "m", "pricing": validPricing(),
			"long_context_threshold_tokens": -1}, "long_context_threshold_tokens must be non-negative"},
	}
	for _, tc := range cases {
		var body map[string]map[string]string
		doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/ai-models", tc.body, http.StatusBadRequest, &body)
		require.Equal(t, "bad_request", body["error"]["code"], tc.name)
		require.Contains(t, body["error"]["message"], tc.wantMessage, tc.name)
	}

	// Happy path with all fields; display_name defaults to model_name.
	var model domain.AIModel
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/ai-models", map[string]any{
		"provider_id":                   provider.ID,
		"model_name":                    "gpt-big",
		"context_window":                200000,
		"supports_tools":                true,
		"pricing":                       validPricing(),
		"long_context_threshold_tokens": 272000,
	}, http.StatusCreated, &model)
	require.Equal(t, "gpt-big", model.DisplayName)
	require.Equal(t, 200000, model.ContextWindow)
	require.True(t, model.SupportsTools)
	require.Equal(t, 272000, model.LongContextThresholdTokens)
	require.Equal(t, domain.ResourceActive, model.Status)
}

func TestAIModelPatchValidationAndDefaults(t *testing.T) {
	t.Parallel()
	handler, _ := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "patch-prov")
	model := createTestAIModel(t, handler, provider.ID, "patch-model")

	var patched domain.AIModel
	doJSONAuth(t, handler, "Bearer admin", http.MethodPatch, "/api/v1/ai-models/"+model.ID, map[string]any{
		"display_name":   "Patched Model",
		"context_window": 128000,
		"supports_tools": false,
	}, http.StatusOK, &patched)
	require.Equal(t, "Patched Model", patched.DisplayName)
	require.Equal(t, 128000, patched.ContextWindow)
	require.False(t, patched.SupportsTools)
	require.Equal(t, "patch-model", patched.ModelName, "untouched fields survive PATCH")

	var body map[string]map[string]string
	doJSONAuth(t, handler, "Bearer admin", http.MethodPatch, "/api/v1/ai-models/"+model.ID, map[string]any{
		"pricing": map[string]any{"input_per_1m": -0.5, "cached_input_per_1m": 0, "cache_write_per_1m": 0, "output_per_1m": 0},
	}, http.StatusBadRequest, &body)
	require.Contains(t, body["error"]["message"], "input_per_1m in pricing must be non-negative")

	doJSONAuth(t, handler, "Bearer admin", http.MethodPatch, "/api/v1/ai-models/"+model.ID, map[string]any{
		"provider_id": "ghost",
	}, http.StatusBadRequest, &body)
	require.Contains(t, body["error"]["message"], "provider_id must reference an existing provider")
}

func TestAIModelDuplicateRejected(t *testing.T) {
	t.Parallel()
	handler, _ := newAIModelHarness(t)
	p1 := createTestProvider(t, handler, "dup-prov-1")
	p2 := createTestProvider(t, handler, "dup-prov-2")
	createTestAIModel(t, handler, p1.ID, "shared-name")

	var dup map[string]map[string]string
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/ai-models", map[string]any{
		"provider_id": p1.ID, "model_name": "shared-name", "pricing": validPricing(),
	}, http.StatusConflict, &dup)
	require.Equal(t, "duplicate_model", dup["error"]["code"])

	// Same model_name under a different provider is fine.
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/ai-models", map[string]any{
		"provider_id": p2.ID, "model_name": "shared-name", "pricing": validPricing(),
	}, http.StatusCreated, &domain.AIModel{})

	// PATCH into a duplicate is also rejected.
	var other domain.AIModel
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/ai-models", map[string]any{
		"provider_id": p1.ID, "model_name": "unique-name", "pricing": validPricing(),
	}, http.StatusCreated, &other)
	doJSONAuth(t, handler, "Bearer admin", http.MethodPatch, "/api/v1/ai-models/"+other.ID, map[string]any{
		"model_name": "shared-name",
	}, http.StatusConflict, &dup)
	require.Equal(t, "duplicate_model", dup["error"]["code"])
}

func TestAIModelDeleteInUseAndForce(t *testing.T) {
	t.Parallel()
	handler, store := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "delete-prov")
	model := createTestAIModel(t, handler, provider.ID, "delete-model")

	// Grant to alice and bind one of alice's agents to the model.
	var alice domain.User
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &alice)
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, "/api/v1/users/"+alice.ID+"/models", map[string]any{
		"model_ids": []string{model.ID},
	}, http.StatusOK, &[]domain.AIModel{})

	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer alice", http.MethodPost, "/api/v1/squads", map[string]any{"name": "Delete Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, "Bearer alice", http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "Bound Agent"}, http.StatusCreated, &agent)
	agent.AIModelID = model.ID
	_, err := store.UpdateAgent(context.Background(), &agent)
	require.NoError(t, err)

	// DELETE without force → 409 in_use with both the grant and the binding.
	var conflict struct {
		Error   string              `json:"error"`
		Message string              `json:"message"`
		Usage   []aiModelUsageEntry `json:"usage"`
	}
	doJSONAuth(t, handler, "Bearer admin", http.MethodDelete, "/api/v1/ai-models/"+model.ID, nil, http.StatusConflict, &conflict)
	require.Equal(t, "in_use", conflict.Error)
	require.Len(t, conflict.Usage, 2)

	var grantEntry, agentEntry aiModelUsageEntry
	for _, u := range conflict.Usage {
		if u.AgentID == "" {
			grantEntry = u
		} else {
			agentEntry = u
		}
	}
	require.Equal(t, alice.ID, grantEntry.UserID)
	require.Equal(t, "alice@example.com", grantEntry.UserEmail)
	require.Equal(t, agent.ID, agentEntry.AgentID)
	require.Equal(t, "Bound Agent", agentEntry.AgentName)
	require.Equal(t, squad.ID, agentEntry.SquadID)
	require.Equal(t, alice.ID, agentEntry.UserID, "bound-agent entry identifies the squad owner")

	// Force delete: model gone, grants cascaded, agent binding cleared.
	doAdminNoBody(t, handler, http.MethodDelete, "/api/v1/ai-models/"+model.ID+"?force=true", http.StatusNoContent)
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/ai-models/"+model.ID, nil, http.StatusNotFound, &map[string]any{})

	grants, err := store.ListUserModelGrants(context.Background(), alice.ID)
	require.NoError(t, err)
	require.Empty(t, grants, "force delete cascades user grants")

	reloaded, err := store.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Empty(t, reloaded.AIModelID, "force delete unbinds agents")

	// Unused models delete without force.
	spare := createTestAIModel(t, handler, provider.ID, "spare-model")
	doAdminNoBody(t, handler, http.MethodDelete, "/api/v1/ai-models/"+spare.ID, http.StatusNoContent)
}

func TestSetUserModelsGrantsIdempotent(t *testing.T) {
	t.Parallel()
	handler, _ := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "grant-prov")
	m1 := createTestAIModel(t, handler, provider.ID, "grant-one")
	m2 := createTestAIModel(t, handler, provider.ID, "grant-two")
	m3 := createTestAIModel(t, handler, provider.ID, "grant-three")

	var alice domain.User
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &alice)
	path := "/api/v1/users/" + alice.ID + "/models"

	var granted []domain.AIModel
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, path, map[string]any{"model_ids": []string{m1.ID, m2.ID}}, http.StatusOK, &granted)
	require.ElementsMatch(t, []string{m1.ID, m2.ID}, modelIDs(granted))

	// Replace: m1 removed, m3 added, m2 kept.
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, path, map[string]any{"model_ids": []string{m2.ID, m3.ID}}, http.StatusOK, &granted)
	require.ElementsMatch(t, []string{m2.ID, m3.ID}, modelIDs(granted))

	// Re-granting the same set is a no-op, not an error.
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, path, map[string]any{"model_ids": []string{m2.ID, m3.ID}}, http.StatusOK, &granted)
	require.ElementsMatch(t, []string{m2.ID, m3.ID}, modelIDs(granted))

	// GET reflects the same set.
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, path, nil, http.StatusOK, &granted)
	require.ElementsMatch(t, []string{m2.ID, m3.ID}, modelIDs(granted))

	// Empty set revokes everything.
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, path, map[string]any{"model_ids": []string{}}, http.StatusOK, &granted)
	require.Empty(t, granted)

	// Unknown model id → 404, existing set untouched.
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, path, map[string]any{"model_ids": []string{m1.ID}}, http.StatusOK, &granted)
	var body map[string]map[string]string
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, path, map[string]any{"model_ids": []string{"ghost-model"}}, http.StatusNotFound, &body)
	require.Equal(t, "not_found", body["error"]["code"])
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, path, nil, http.StatusOK, &granted)
	require.ElementsMatch(t, []string{m1.ID}, modelIDs(granted))

	// Unknown user → 404.
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, "/api/v1/users/ghost-user/models", map[string]any{"model_ids": []string{}}, http.StatusNotFound, &body)
}

func TestMyModelsOnlyGrantedAndActive(t *testing.T) {
	t.Parallel()
	handler, _ := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "me-prov")
	m1 := createTestAIModel(t, handler, provider.ID, "me-one")
	m2 := createTestAIModel(t, handler, provider.ID, "me-two")
	m3 := createTestAIModel(t, handler, provider.ID, "me-three")

	var alice domain.User
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &alice)
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, "/api/v1/users/"+alice.ID+"/models", map[string]any{
		"model_ids": []string{m1.ID, m2.ID, m3.ID},
	}, http.StatusOK, &[]domain.AIModel{})

	// Deprecate m2; revoke m3 (m1 remains granted + active). WP4:
	// deprecate returns 200 with the cascade report (affected counts).
	doAdminNoBody(t, handler, http.MethodPost, "/api/v1/ai-models/"+m2.ID+"/deprecate", http.StatusOK)
	doJSONAuth(t, handler, "Bearer admin", http.MethodPut, "/api/v1/users/"+alice.ID+"/models", map[string]any{
		"model_ids": []string{m1.ID, m2.ID},
	}, http.StatusOK, &[]domain.AIModel{})

	var mine []domain.AIModel
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/models/me", nil, http.StatusOK, &mine)
	require.ElementsMatch(t, []string{m1.ID}, modelIDs(mine), "revoked and deprecated models must not appear")

	// Admin without grants sees none either — /models/me is scoped to the caller.
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/models/me", nil, http.StatusOK, &mine)
	require.Empty(t, mine)
}

func TestLLMProviderNoLongerGrantableToAgents(t *testing.T) {
	t.Parallel()
	handler, store := newAIModelHarness(t)
	provider := createTestProvider(t, handler, "closed-door-prov")

	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer alice", http.MethodPost, "/api/v1/squads", map[string]any{"name": "Closed Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, "Bearer alice", http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "Closed Agent"}, http.StatusCreated, &agent)

	var body map[string]map[string]string
	doJSONAuth(t, handler, "Bearer alice", http.MethodPut, "/api/v1/agents/"+agent.ID+"/permissions", []map[string]string{
		{"resource_type": "llm_provider", "resource_id": provider.ID},
	}, http.StatusBadRequest, &body)
	require.Equal(t, "provider_not_grantable", body["error"]["code"])
	require.Contains(t, body["error"]["message"], "AI Models")

	perms, err := store.ListAgentPermissions(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Empty(t, perms, "rejected grant must not persist")

	// Unknown types keep the generic invalid-type error.
	doJSONAuth(t, handler, "Bearer alice", http.MethodPut, "/api/v1/agents/"+agent.ID+"/permissions", []map[string]string{
		{"resource_type": "not-a-type", "resource_id": "x"},
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])

	// Non-LLM grants still work.
	var skill domain.RegistryResource
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/registry/skills", map[string]any{
		"name": "still-works-skill", "manifest": map[string]any{"version": "1"},
	}, http.StatusCreated, &skill)
	var permsOut []domain.AgentPermission
	doJSONAuth(t, handler, "Bearer alice", http.MethodPut, "/api/v1/agents/"+agent.ID+"/permissions", []map[string]string{
		{"resource_type": "skill", "resource_id": skill.ID},
	}, http.StatusOK, &permsOut)
	require.Len(t, permsOut, 1)
}

// WP6 gap-fill: the Settings → Access tab needs a platform_admin
// directory read. GET /users must be admin-only and must not leak OIDC
// identifiers (lean projection by design).
func TestListUsersAdminDirectory(t *testing.T) {
	t.Parallel()
	handler, _ := newAIModelHarness(t)

	var denied map[string]map[string]string
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/users", nil, http.StatusForbidden, &denied)
	require.Equal(t, "forbidden", denied["error"]["code"])

	// First authenticated call provisions each OIDC user.
	var mine []domain.AIModel
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/models/me", nil, http.StatusOK, &mine)
	doJSONAuth(t, handler, "Bearer bob", http.MethodGet, "/api/v1/models/me", nil, http.StatusOK, &mine)

	var users []map[string]any
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/users", nil, http.StatusOK, &users)
	require.Len(t, users, 3)

	rolesByEmail := map[string]string{}
	for _, u := range users {
		require.NotContains(t, u, "oidc_issuer", "directory projection must not leak OIDC identifiers")
		require.NotContains(t, u, "oidc_subject", "directory projection must not leak OIDC identifiers")
		rolesByEmail[u["email"].(string)] = u["role"].(string)
	}
	require.Equal(t, "platform_admin", rolesByEmail["admin@example.com"])
	require.Equal(t, "user", rolesByEmail["alice@example.com"])
	require.Equal(t, "user", rolesByEmail["bob@example.com"])
}
