// WP3 tests: binding-derived virtual-key allow-lists, precise provisioning
// errors, the agent binding PATCH API, and bidirectional key convergence
// (ADR-0010 D4/D5/D6/D7). These replace the WP-era grant-union tests:
// no key provisioning path may derive models from llm_provider grants.

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
// needed to drive binding-based key-lifecycle scenarios: one squad (owned by
// the dev admin), one agent, and two granted AI models named "openai/one"
// and "openai/two" plus a third model ("openai/three") that is deliberately
// NOT granted to the owner.
type gatewayFixture struct {
	handler      http.Handler
	store        *storage.MemoryStore
	gateway      *recordingGateway
	squadID      string
	ownerID      string
	agentID      string
	modelA       domain.AIModel
	modelB       domain.AIModel
	modelNoGrant domain.AIModel
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
	provider := createTestProvider(t, handler, "gw-provider")
	modelA := createTestAIModel(t, handler, provider.ID, "openai/one")
	modelB := createTestAIModel(t, handler, provider.ID, "openai/two")
	modelNoGrant := createTestAIModel(t, handler, provider.ID, "openai/three")

	// Grant A and B (but NOT modelNoGrant) to the squad owner.
	doJSON(t, handler, http.MethodPut, "/api/v1/users/"+squad.OwnerID+"/models",
		map[string]any{"model_ids": []string{modelA.ID, modelB.ID}}, http.StatusOK, &[]domain.AIModel{})

	return &gatewayFixture{
		handler:      handler,
		store:        store,
		gateway:      gw,
		squadID:      squad.ID,
		ownerID:      squad.OwnerID,
		agentID:      agent.ID,
		modelA:       modelA,
		modelB:       modelB,
		modelNoGrant: modelNoGrant,
	}
}

// bind sets the agent's primary/fallback through the public PATCH API.
// Empty strings clear the respective slot.
func (f *gatewayFixture) bind(t *testing.T, primary, fallback string) domain.Agent {
	t.Helper()
	var agent domain.Agent
	doJSON(t, f.handler, http.MethodPatch, "/api/v1/agents/"+f.agentID, map[string]any{
		"ai_model_id":          primary,
		"fallback_ai_model_id": fallback,
	}, http.StatusOK, &agent)
	return agent
}

// seedBinding writes the binding directly to the store, bypassing PATCH
// validation and key convergence — used to exercise the provisioning-time
// error paths that the write API would normally block earlier.
func (f *gatewayFixture) seedBinding(t *testing.T, primary, fallback string) {
	t.Helper()
	agent, err := f.store.GetAgent(context.Background(), f.agentID)
	require.NoError(t, err)
	agent.AIModelID = primary
	agent.FallbackAIModelID = fallback
	_, err = f.store.UpdateAgent(context.Background(), agent)
	require.NoError(t, err)
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

func requireErrorCode(t *testing.T, handler http.Handler, method, path string, body any, wantStatus int, wantCode string) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
	errObj := errorEnvelope(t, rec)
	require.Equal(t, wantCode, errObj["code"])
	require.NotEmpty(t, errObj["message"])
}

// fallbacksOf extracts the router_settings.fallbacks mapping recorded on a
// key generate/update body.
func fallbacksOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	rs, ok := body["router_settings"].(map[string]any)
	require.True(t, ok, "key request carried no router_settings: %v", body)
	fbs, ok := rs["fallbacks"].([]any)
	require.True(t, ok, "router_settings carried no fallbacks: %v", rs)
	return fbs
}

// ---------------------------------------------------------------------------
// THE CRITICAL INVARIANT (ADR-0010 D5)
// ---------------------------------------------------------------------------

func TestBoundKeyContainsPrimaryAndFallback(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)

	gen, _, _ := f.gateway.counts()
	require.Equal(t, 1, gen)
	keyReq := f.gateway.generated[0]

	// The invariant: the virtual key's allow-list MUST contain BOTH the
	// primary and the fallback. If it only contained the primary, the
	// fallback would fail authorisation at the gateway during a primary
	// outage — the exact moment it is needed.
	require.ElementsMatch(t, []any{"openai/one", "openai/two"}, keyReq["models"])

	// The gateway (not the runtime) owns the failover: the key carries a
	// primary→fallback router mapping.
	require.Equal(t, []any{map[string]any{"openai/one": []any{"openai/two"}}}, fallbacksOf(t, keyReq))

	// D7 failure-class policy travels with the key.
	rs := keyReq["router_settings"].(map[string]any)
	rp := rs["retry_policy"].(map[string]any)
	require.EqualValues(t, 0, rp["BadRequestErrorRetries"], "400-class must not be retried")
	require.EqualValues(t, 0, rp["AuthenticationErrorRetries"], "upstream auth failures must not be retried with the same dead key")
	require.Greater(t, rp["RateLimitErrorRetries"], float64(0), "429 gets retries before fallback")
	require.Greater(t, rp["TimeoutErrorRetries"], float64(0), "timeouts get retries before fallback")
}

func TestBoundKeyWithoutFallbackHasNoFallbackMapping(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, "")
	f.createIdentity(t)

	_, _, _ = f.gateway.counts()
	keyReq := f.gateway.generated[0]
	require.ElementsMatch(t, []any{"openai/one"}, keyReq["models"])
	require.Empty(t, fallbacksOf(t, keyReq))
}

// ---------------------------------------------------------------------------
// Precise provisioning errors (replaces the S-104 no_llm_models_granted)
// ---------------------------------------------------------------------------

func TestUnboundAgentIdentityRejected(t *testing.T) {
	f := newGatewayFixture(t)
	requireErrorCode(t, f.handler, http.MethodPost, "/api/v1/agents/"+f.agentID+"/identity", nil,
		http.StatusConflict, "model_not_bound")
}

func TestNotGrantedToOwnerRejectedOnProvision(t *testing.T) {
	f := newGatewayFixture(t)
	// Seed past the PATCH validation so the provisioning path itself is
	// what rejects the ungranted binding.
	f.seedBinding(t, f.modelNoGrant.ID, "")
	requireErrorCode(t, f.handler, http.MethodPost, "/api/v1/agents/"+f.agentID+"/identity", nil,
		http.StatusForbidden, "model_not_granted")
}

func TestUngrantedFallbackRejectedOnProvision(t *testing.T) {
	f := newGatewayFixture(t)
	f.seedBinding(t, f.modelA.ID, f.modelNoGrant.ID)
	requireErrorCode(t, f.handler, http.MethodPost, "/api/v1/agents/"+f.agentID+"/identity", nil,
		http.StatusForbidden, "model_not_granted")
}

func TestDeprecatedModelRejectedOnProvision(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	doAdminNoBody(t, f.handler, http.MethodPost, "/api/v1/ai-models/"+f.modelB.ID+"/deprecate", http.StatusNoContent)
	requireErrorCode(t, f.handler, http.MethodPost, "/api/v1/agents/"+f.agentID+"/identity", nil,
		http.StatusConflict, "model_deprecated")
}

func TestDeprecatedFallbackRejectedOnProvision(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	doAdminNoBody(t, f.handler, http.MethodPost, "/api/v1/ai-models/"+f.modelA.ID+"/deprecate", http.StatusNoContent)
	requireErrorCode(t, f.handler, http.MethodPost, "/api/v1/agents/"+f.agentID+"/identity", nil,
		http.StatusConflict, "model_deprecated")
}

func TestGatewayFailureStillSurfacesAsBadGateway(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.gateway.mu.Lock()
	f.gateway.failGenerate = true
	f.gateway.mu.Unlock()
	requireErrorCode(t, f.handler, http.MethodPost, "/api/v1/agents/"+f.agentID+"/identity", nil,
		http.StatusBadGateway, "llm_gateway_unavailable")
}

// ---------------------------------------------------------------------------
// Agent binding PATCH API
// ---------------------------------------------------------------------------

func TestPatchBindingPersistsAndConvergesKey(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, "")
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	agent := f.bind(t, f.modelA.ID, f.modelB.ID)
	require.Equal(t, f.modelA.ID, agent.AIModelID)
	require.Equal(t, f.modelB.ID, agent.FallbackAIModelID)

	gen, upd, del := f.gateway.counts()
	require.Equal(t, 1, gen)
	require.Equal(t, 1, upd, "binding change must converge the live key")
	require.Empty(t, del)
	require.Equal(t, token, f.gateway.updated[0]["key"])
	require.ElementsMatch(t, []any{"openai/one", "openai/two"}, f.gateway.updated[0]["models"])
	require.Equal(t, []any{map[string]any{"openai/one": []any{"openai/two"}}}, fallbacksOf(t, f.gateway.updated[0]))
}

func TestPatchFallbackEqualsPrimaryRejected(t *testing.T) {
	f := newGatewayFixture(t)
	requireErrorCode(t, f.handler, http.MethodPatch, "/api/v1/agents/"+f.agentID, map[string]any{
		"ai_model_id":          f.modelA.ID,
		"fallback_ai_model_id": f.modelA.ID,
	}, http.StatusBadRequest, "fallback_same_as_primary")
}

func TestPatchUngrantedModelRejected(t *testing.T) {
	f := newGatewayFixture(t)
	requireErrorCode(t, f.handler, http.MethodPatch, "/api/v1/agents/"+f.agentID, map[string]any{
		"ai_model_id": f.modelNoGrant.ID,
	}, http.StatusForbidden, "model_not_granted")
}

func TestPatchUnknownModelRejected(t *testing.T) {
	f := newGatewayFixture(t)
	requireErrorCode(t, f.handler, http.MethodPatch, "/api/v1/agents/"+f.agentID, map[string]any{
		"ai_model_id": "00000000-0000-4000-8000-000000000000",
	}, http.StatusBadRequest, "model_not_found")
}

func TestPatchDeprecatedModelRejected(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, "")
	doAdminNoBody(t, f.handler, http.MethodPost, "/api/v1/ai-models/"+f.modelB.ID+"/deprecate", http.StatusNoContent)
	requireErrorCode(t, f.handler, http.MethodPatch, "/api/v1/agents/"+f.agentID, map[string]any{
		"fallback_ai_model_id": f.modelB.ID,
	}, http.StatusConflict, "model_deprecated")
}

func TestPatchFallbackWithoutPrimaryRejected(t *testing.T) {
	f := newGatewayFixture(t)
	requireErrorCode(t, f.handler, http.MethodPatch, "/api/v1/agents/"+f.agentID, map[string]any{
		"ai_model_id":          "",
		"fallback_ai_model_id": f.modelB.ID,
	}, http.StatusBadRequest, "bad_request")
}

func TestPatchClearFallbackShrinksAllowList(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)

	agent := f.bind(t, f.modelA.ID, "")
	require.Empty(t, agent.FallbackAIModelID)

	_, upd, _ := f.gateway.counts()
	require.Equal(t, 1, upd)
	require.ElementsMatch(t, []any{"openai/one"}, f.gateway.updated[0]["models"])
	require.Empty(t, fallbacksOf(t, f.gateway.updated[0]), "cleared fallback must clear the router fallbacks mapping")
}

func TestGetAgentReturnsBindingFields(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	var agent domain.Agent
	doJSON(t, f.handler, http.MethodGet, "/api/v1/agents/"+f.agentID, nil, http.StatusOK, &agent)
	require.Equal(t, f.modelA.ID, agent.AIModelID)
	require.Equal(t, f.modelB.ID, agent.FallbackAIModelID)
}

// ---------------------------------------------------------------------------
// Convergence: sync in both directions
// ---------------------------------------------------------------------------

func TestSyncPrimaryChangeUpdatesKey(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)

	f.bind(t, f.modelB.ID, f.modelA.ID)
	_, upd, del := f.gateway.counts()
	require.Equal(t, 1, upd)
	require.Empty(t, del)
	require.ElementsMatch(t, []any{"openai/two", "openai/one"}, f.gateway.updated[0]["models"])
	require.Equal(t, []any{map[string]any{"openai/two": []any{"openai/one"}}}, fallbacksOf(t, f.gateway.updated[0]))
}

func TestSyncUnbindRevokesKey(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	f.bind(t, "", "")
	_, _, del := f.gateway.counts()
	require.Equal(t, []string{token}, del)
	require.Equal(t, domain.GatewayKeyRevoked, f.identity(t).GatewayKeyStatus)
}

func TestSyncRebindReprovisionsKey(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, "")
	f.createIdentity(t)
	f.bind(t, "", "") // revoked
	genBefore, _, _ := f.gateway.counts()

	f.bind(t, f.modelB.ID, f.modelA.ID)
	genAfter, upd, _ := f.gateway.counts()
	require.Equal(t, genBefore+1, genAfter)
	require.Equal(t, 0, upd)
	require.Equal(t, domain.GatewayKeyActive, f.identity(t).GatewayKeyStatus)
}

func TestGatewayFailureSurfacesInReconcile(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	f.gateway.mu.Lock()
	f.gateway.failUpdate = true
	f.gateway.mu.Unlock()

	// Change the binding out-of-band so reconcile is what tries (and
	// fails) to converge the key.
	f.seedBinding(t, f.modelB.ID, f.modelA.ID)
	var summary map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary)
	require.EqualValues(t, 1, summary["errors"])
	require.Contains(t, summary, "failures")

	// Key state untouched: still active with the original token.
	identity := f.identity(t)
	require.Equal(t, domain.GatewayKeyActive, identity.GatewayKeyStatus)
	require.Equal(t, token, identity.GatewayKeyToken)
}

func TestReconcileGatewayKeysRepairsDrift(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)

	// Simulate drift: identity says revoked but a binding is active.
	_, err := f.store.SetAgentIdentityGatewayKey(context.Background(), f.agentID, "tok-lost", domain.GatewayKeyRevoked)
	require.NoError(t, err)

	var summary map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary)
	require.EqualValues(t, 1, summary["provisioned"])

	identity := f.identity(t)
	require.Equal(t, domain.GatewayKeyActive, identity.GatewayKeyStatus)
	require.NotEmpty(t, identity.GatewayKeyToken)

	// Idempotent: second run re-converges without error and without
	// generating another key.
	var summary2 map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary2)
	require.EqualValues(t, 1, summary2["updated"])
}

func TestReconcileRevokesUnboundAgentKey(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	// Drift: binding cleared out-of-band but key still marked active.
	f.seedBinding(t, "", "")

	var summary map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary)
	require.EqualValues(t, 1, summary["revoked"])
	require.Equal(t, domain.GatewayKeyRevoked, f.identity(t).GatewayKeyStatus)
	_, _, del := f.gateway.counts()
	require.Equal(t, []string{token}, del)
}

func TestReconcileReportsUngrantedBindingAsError(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, "")
	f.createIdentity(t)

	// Grant revoked out-of-band (WP4 will own the forced cascade; WP3's
	// sync must refuse to converge rather than silently keep the key).
	doJSON(t, f.handler, http.MethodPut, "/api/v1/users/"+f.ownerID+"/models",
		map[string]any{"model_ids": []string{f.modelB.ID}}, http.StatusOK, &[]domain.AIModel{})

	var summary map[string]any
	doJSON(t, f.handler, http.MethodPost, "/api/v1/admin/gateway/keys/reconcile", nil, http.StatusOK, &summary)
	require.EqualValues(t, 1, summary["errors"])
	require.Equal(t, domain.GatewayKeyActive, f.identity(t).GatewayKeyStatus, "refused convergence must not mutate the key")
}

func TestDeleteAgentRevokesGatewayKey(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	doJSONNoBody(t, f.handler, http.MethodDelete, "/api/v1/agents/"+f.agentID, nil, http.StatusNoContent)

	_, _, del := f.gateway.counts()
	require.Equal(t, []string{token}, del)
}

func TestDeleteSquadRevokesGatewayKeys(t *testing.T) {
	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)
	token := f.identity(t).GatewayKeyToken

	doJSONNoBody(t, f.handler, http.MethodDelete, "/api/v1/squads/"+f.squadID, nil, http.StatusNoContent)

	_, _, del := f.gateway.counts()
	require.Equal(t, []string{token}, del)
}
