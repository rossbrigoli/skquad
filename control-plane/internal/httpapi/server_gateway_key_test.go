package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// recordingGateway is a LiteLLM admin-API stand-in that records key lifecycle
// calls and can be told to fail a specific operation.
type recordingGateway struct {
	mu           sync.Mutex
	generated    []map[string]any
	updated      []map[string]any
	deleted      []string
	failGenerate bool
	failUpdate   bool
	failDelete   bool
	seq          int
}

func (g *recordingGateway) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		defer g.mu.Unlock()
		switch r.URL.Path {
		case "/key/generate":
			if g.failGenerate {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			g.seq++
			g.generated = append(g.generated, body)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"key":   "sk-generated-" + string(rune('a'+g.seq)),
				"token": "tok-" + string(rune('a'+g.seq)),
			})
		case "/key/update":
			if g.failUpdate {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			g.updated = append(g.updated, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"updated": true})
		case "/key/delete":
			if g.failDelete {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			g.deleted = append(g.deleted, body["key"].(string))
			_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (g *recordingGateway) counts() (gen, upd int, del []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.generated), len(g.updated), append([]string{}, g.deleted...)
}

// gatewayFixture wires a server to a recording gateway and returns the pieces
// needed to drive key-lifecycle scenarios: one squad, one agent, two LLM
// providers (models "openai/one" and "openai/two").
type gatewayFixture struct {
	handler   http.Handler
	store     *storage.MemoryStore
	gateway   *recordingGateway
	squadID   string
	agentID   string
	provider  string
	provider2 string
}

func newGatewayFixture(t *testing.T) *gatewayFixture {
	t.Helper()
	gw := &recordingGateway{}
	server := httptest.NewServer(gw.handler())
	t.Cleanup(server.Close)

	cfg := testConfig()
	cfg.LiteLLMAdminURL = server.URL
	cfg.LiteLLMMasterKey = "sk-test-master"
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(cfg, store, &fakeCRWriter{})

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Key Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "Key Agent"}, http.StatusCreated, &agent)
	var p1 domain.LLMProvider
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name": "Provider One", "kind": "openai", "base_url": "http://one.local/v1",
		"api_key_ref": "secret/one", "default_model": "openai/one", "models": []string{"openai/one"},
	}, http.StatusCreated, &p1)
	var p2 domain.LLMProvider
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name": "Provider Two", "kind": "openai", "base_url": "http://two.local/v1",
		"api_key_ref": "secret/two", "default_model": "openai/two", "models": []string{"openai/two"},
	}, http.StatusCreated, &p2)

	return &gatewayFixture{
		handler:   handler,
		store:     store,
		gateway:   gw,
		squadID:   squad.ID,
		agentID:   agent.ID,
		provider:  p1.ID,
		provider2: p2.ID,
	}
}

func (f *gatewayFixture) setPerms(t *testing.T, providerIDs ...string) {
	t.Helper()
	// The llm_provider grant type is closed at the API (ADR-0010 / S-107).
	// Seed the grants directly and converge the gateway key through the
	// admin reconcile endpoint — the same convergence machinery the grant
	// path used to drive inline.
	perms := make([]domain.AgentPermission, 0, len(providerIDs))
	for _, id := range providerIDs {
		perms = append(perms, domain.AgentPermission{
			AgentID:      f.agentID,
			ResourceType: domain.ResLLMProvider,
			ResourceID:   id,
			GrantedBy:    "test",
		})
	}
	require.NoError(t, f.store.SetAgentPermissions(context.Background(), f.agentID, perms))
	doJSONNoBody(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK)
}

func (f *gatewayFixture) identity(t *testing.T) *domain.AgentIdentity {
	t.Helper()
	identity, err := f.store.GetAgentIdentity(context.Background(), f.agentID)
	require.NoError(t, err)
	return identity
}

func (f *gatewayFixture) createIdentity(t *testing.T) {
	t.Helper()
	var identity domain.AgentIdentity
	doJSON(t, f.handler, http.MethodPost, "/api/v1/agents/"+f.agentID+"/identity", nil, http.StatusCreated, &identity)
	require.NotEmpty(t, identity.ID)
}

func TestGatewayKeyRevokedWhenLastLLMGrantRemoved(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider)
	f.createIdentity(t)
	gen, _, _ := f.gateway.counts()
	require.Equal(t, 1, gen)
	require.Equal(t, domain.GatewayKeyActive, f.identity(t).GatewayKeyStatus)
	token := f.identity(t).GatewayKeyToken
	require.NotEmpty(t, token)

	// Remove the only LLM grant → key must be revoked at the gateway.
	f.setPerms(t)

	gen, upd, del := f.gateway.counts()
	require.Equal(t, 1, gen, "no new key should be generated")
	require.Equal(t, 0, upd, "no update expected on full revocation")
	require.Equal(t, []string{token}, del)
	identity := f.identity(t)
	require.Equal(t, domain.GatewayKeyRevoked, identity.GatewayKeyStatus)
}

func TestGatewayKeyUpdatedOnPermissionChange(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	// Add a second provider → key allow-list updated, not re-created.
	f.setPerms(t, f.provider, f.provider2)

	gen, upd, del := f.gateway.counts()
	require.Equal(t, 1, gen)
	require.Equal(t, 1, upd)
	require.Empty(t, del)
	require.Equal(t, token, f.gateway.updated[0]["key"])
	require.ElementsMatch(t, []any{"openai/one", "openai/two"}, f.gateway.updated[0]["models"])
	require.Equal(t, domain.GatewayKeyActive, f.identity(t).GatewayKeyStatus)
}

func TestGatewayKeyReProvisionedOnReGrant(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider)
	f.createIdentity(t)
	f.setPerms(t) // revoked
	genBefore, _, _ := f.gateway.counts()

	// Re-grant → a fresh key is provisioned against the existing identity.
	f.setPerms(t, f.provider)

	genAfter, upd, del := f.gateway.counts()
	require.Equal(t, genBefore+1, genAfter)
	require.Equal(t, 0, upd)
	require.Len(t, del, 1)
	identity := f.identity(t)
	require.Equal(t, domain.GatewayKeyActive, identity.GatewayKeyStatus)
	require.NotEmpty(t, identity.GatewayKeyToken)
}

func TestGatewayKeyFailureSurfacesInReconcile(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	f.gateway.mu.Lock()
	f.gateway.failUpdate = true
	f.gateway.mu.Unlock()

	// The grant API can no longer carry llm_provider (S-107), so the
	// gateway-failure path is exercised through reconcile: a failing
	// gateway update must be reported without corrupting key state.
	require.NoError(t, f.store.SetAgentPermissions(context.Background(), f.agentID, []domain.AgentPermission{
		{AgentID: f.agentID, ResourceType: domain.ResLLMProvider, ResourceID: f.provider, GrantedBy: "test"},
		{AgentID: f.agentID, ResourceType: domain.ResLLMProvider, ResourceID: f.provider2, GrantedBy: "test"},
	}))
	var summary map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary)
	require.EqualValues(t, 1, summary["errors"])
	require.Contains(t, summary, "failures")

	// Key state untouched: still active with the original token.
	identity := f.identity(t)
	require.Equal(t, domain.GatewayKeyActive, identity.GatewayKeyStatus)
	require.Equal(t, token, identity.GatewayKeyToken)
}

func TestDeleteAgentRevokesGatewayKey(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	doJSONNoBody(t, f.handler, http.MethodDelete, "/api/v1/agents/"+f.agentID, nil, http.StatusNoContent)

	_, _, del := f.gateway.counts()
	require.Equal(t, []string{token}, del)
}

func TestDeleteSquadRevokesGatewayKeys(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	doJSONNoBody(t, f.handler, http.MethodDelete, "/api/v1/squads/"+f.squadID, nil, http.StatusNoContent)

	_, _, del := f.gateway.counts()
	require.Equal(t, []string{token}, del)
}

func TestReconcileGatewayKeysRepairsDrift(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider)
	f.createIdentity(t)

	// Simulate drift: identity says revoked but a grant is active.
	_, err := f.store.SetAgentIdentityGatewayKey(context.Background(), f.agentID, "tok-lost", domain.GatewayKeyRevoked)
	require.NoError(t, err)

	var summary map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary)
	require.EqualValues(t, 1, summary["provisioned"])

	identity := f.identity(t)
	require.Equal(t, domain.GatewayKeyActive, identity.GatewayKeyStatus)
	require.NotEmpty(t, identity.GatewayKeyToken)

	// Idempotent: second run changes nothing.
	var summary2 map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary2)
	require.EqualValues(t, 1, summary2["updated"])
}

func TestReconcileRevokesKeyWithNoGrants(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	// Drift: grants removed out-of-band but key still marked active.
	require.NoError(t, f.store.SetAgentPermissions(context.Background(), f.agentID, nil))

	var summary map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary)
	require.EqualValues(t, 1, summary["revoked"])
	require.Equal(t, domain.GatewayKeyRevoked, f.identity(t).GatewayKeyStatus)
	_, _, del := f.gateway.counts()
	require.Equal(t, []string{token}, del)
}

func TestDeprecateProviderConvergesKeys(t *testing.T) {
	f := newGatewayFixture(t)
	f.setPerms(t, f.provider, f.provider2)
	f.createIdentity(t)

	doJSONNoBody(t, f.handler, http.MethodPost, "/api/v1/registry/llm-providers/"+f.provider+"/deprecate", nil, http.StatusNoContent)

	gen, upd, del := f.gateway.counts()
	require.Equal(t, 1, gen)
	require.Equal(t, 1, upd, "deprecation should trigger a key update")
	require.Empty(t, del)
	require.ElementsMatch(t, []any{"openai/two"}, f.gateway.updated[0]["models"])
}
