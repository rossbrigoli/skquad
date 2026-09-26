// S-129 regression tests: rebinding an agent's LLM model must persist.
//
// Bug report: binding a new model on the agent page appeared to succeed,
// but after navigating away and back the old model was shown again. These
// tests replay the EXACT sequence the web LLM tab performs
// (buildBindingPayload always sends both keys; "" clears a slot) against
// a freshly bound agent and assert the GET/list read-paths — which the UI
// re-renders from on remount — reflect the change.

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

// setupS129Agent creates squad + provider + two granted models + an agent
// already bound primary=modelOne / fallback=modelTwo (the "previous
// value" from the bug report).
func setupS129Agent(t *testing.T, handler http.Handler) (squad domain.Squad, agent domain.Agent, m1, m2 domain.AIModel) {
	t.Helper()

	// Unique suffix so the test is re-runnable against a persistent DB.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "S129 Squad " + suffix}, http.StatusCreated, &squad)
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Mary " + suffix}, http.StatusCreated, &agent)
	var provider domain.LLMProvider
	doJSON(t, handler, http.MethodPost, pathProviders, map[string]any{
		"name":        "S129 Provider " + suffix,
		"kind":        openaiCompatible,
		"base_url":    localLLMBaseURL,
		"api_key_ref": "secret/s129",
	}, http.StatusCreated, &provider)

	m1 = createTestAIModel(t, handler, provider.ID, modelOne+"-"+suffix)
	m2 = createTestAIModel(t, handler, provider.ID, modelTwo+"-"+suffix)
	doJSON(t, handler, http.MethodPut, pathUsersPrefix+squad.OwnerID+pathModels,
		map[string]any{"model_ids": []string{m1.ID, m2.ID}}, http.StatusOK, &[]domain.AIModel{})

	// Initial bind (UI payload shape: both keys always present).
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"ai_model_id":          m1.ID,
		"fallback_ai_model_id": m2.ID,
	}, http.StatusOK, &agent)
	require.Equal(t, m1.ID, agent.AIModelID)
	require.Equal(t, m2.ID, agent.FallbackAIModelID)
	return
}

// TestS129RebindPrimaryModelPersistsOnGet is the core regression: after a
// successful rebind PATCH, a subsequent GET (the read path the UI uses on
// remount) must return the NEW model, not the previous one.
func TestS129RebindPrimaryModelPersistsOnGet(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())
	squad, agent, _, m2 := setupS129Agent(t, handler)

	// Rebind primary to the other granted model, clearing the fallback —
	// exactly what buildBindingPayload(newPrimary, "") produces.
	var patched domain.Agent
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"ai_model_id":          m2.ID,
		"fallback_ai_model_id": "",
	}, http.StatusOK, &patched)
	require.Equal(t, m2.ID, patched.AIModelID, "PATCH response must carry the new primary")
	require.Empty(t, patched.FallbackAIModelID, "fallback cleared")

	// The read path the UI uses when re-displaying the agent.
	var fetched domain.Agent
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, m2.ID, fetched.AIModelID, "GET must reflect the rebound primary (S-129)")
	require.Empty(t, fetched.FallbackAIModelID, "GET must reflect the cleared fallback (S-129)")

	// And via the list endpoint the page actually renders from.
	var listed []domain.Agent
	doJSON(t, handler, http.MethodGet, pathSquadsPrefix+squad.ID+pathAgents, nil, http.StatusOK, &listed)
	require.Len(t, listed, 1)
	require.Equal(t, m2.ID, listed[0].AIModelID, "list must reflect the rebound primary (S-129)")
}

// TestS129RebindPrimaryModelPersistsOnPostgres is the same regression run
// against a real Postgres store (the deployed backend). Skips without
// SKQUAD_TEST_DATABASE_URL.
func TestS129RebindPrimaryModelPersistsOnPostgres(t *testing.T) {
	dsn := os.Getenv("SKQUAD_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("SKQUAD_TEST_DATABASE_URL (or DATABASE_URL) is required for the Postgres S-129 test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Use a throwaway database per run so the test is re-runnable against a
	// persistent server: the grant-set replacement in setupS129Agent would
	// otherwise collide (409 in_use) with agents bound by earlier runs.
	testDSN := createUniqueTestDB(t, dsn)
	defer dropTestDB(t, dsn, testDSN)

	pgStore, err := storage.NewPostgresStore(ctx, testDSN)
	require.NoError(t, err)
	defer pgStore.Close()

	handler := New(testConfig(), pgStore)
	squad, agent, _, m2 := setupS129Agent(t, handler)

	var patched domain.Agent
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"ai_model_id":          m2.ID,
		"fallback_ai_model_id": "",
	}, http.StatusOK, &patched)
	require.Equal(t, m2.ID, patched.AIModelID)

	var fetched domain.Agent
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, m2.ID, fetched.AIModelID, "postgres GET must reflect the rebound primary (S-129)")
	require.Empty(t, fetched.FallbackAIModelID, "postgres GET must reflect the cleared fallback (S-129)")

	var listed []domain.Agent
	doJSON(t, handler, http.MethodGet, pathSquadsPrefix+squad.ID+pathAgents, nil, http.StatusOK, &listed)
	require.Len(t, listed, 1)
	require.Equal(t, m2.ID, listed[0].AIModelID, "postgres list must reflect the rebound primary (S-129)")
}

// TestS129RebindAdoptsExistingGatewayKeyOnAliasCollision is THE S-129
// regression: the agent's identity row is stuck at status "none" with an
// empty token, but the gateway still holds the agent's virtual key under
// the unique alias "skquad-agent-<id>". Before the fix, the rebind's
// provision path called /key/generate, LiteLLM rejected the duplicate
// alias with 400, the PATCH aborted as 502 and the new binding was never
// persisted — exactly the "bind succeeds visually, reverts on reload"
// symptom. With the fix the sync adopts the existing key and persists.
func TestS129RebindAdoptsExistingGatewayKeyOnAliasCollision(t *testing.T) {
	t.Parallel()

	gw := &recordingGateway{enforceUniqueAlias: true}
	server := httptest.NewServer(gw.handler())
	t.Cleanup(server.Close)

	cfg := testConfig()
	cfg.LiteLLMAdminURL = server.URL
	cfg.LiteLLMMasterKey = "***"
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(cfg, store, &fakeCRWriter{})

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "S129 Collision Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Mary"}, http.StatusCreated, &agent)
	provider := createTestProvider(t, handler, "collision-provider")
	modelA := createTestAIModel(t, handler, provider.ID, modelOne)
	modelB := createTestAIModel(t, handler, provider.ID, modelTwo)
	doJSON(t, handler, http.MethodPut, pathUsersPrefix+squad.OwnerID+pathModels,
		map[string]any{"model_ids": []string{modelA.ID, modelB.ID}}, http.StatusOK, &[]domain.AIModel{})

	ctx := context.Background()

	// Bind A and create the identity: this provisions the key at the
	// gateway under the agent's unique alias.
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"ai_model_id": modelA.ID, "fallback_ai_model_id": "",
	}, http.StatusOK, &agent)
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &domain.AgentIdentity{})

	gen1, _, _ := gw.counts()
	require.Equal(t, 1, gen1, "identity creation provisions exactly one key")
	alias := fmt.Sprintf("skquad-agent-%s", agent.ID)
	originalToken, found := gw.lookupAlias(alias)
	require.True(t, found, "gateway holds the agent's key under its alias")

	// Simulate the stuck/legacy state: the identity row lost its token and
	// reports "none", while the gateway key remains live.
	_, err := store.SetAgentIdentityGatewayKey(ctx, agent.ID, "", domain.GatewayKeyNone)
	require.NoError(t, err)

	// Rebind A → B. This must NOT 502; it must adopt the existing key.
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"ai_model_id": modelB.ID, "fallback_ai_model_id": "",
	}, http.StatusOK, &agent)
	require.Equal(t, modelB.ID, agent.AIModelID, "rebind response carries the new primary")

	var fetched domain.Agent
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, modelB.ID, fetched.AIModelID, "S-129: rebind persisted despite the gateway holding the key under a duplicate alias")

	// The identity recovered its token (the adopted one) and went active.
	ident, err := store.GetAgentIdentity(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, domain.GatewayKeyActive, ident.GatewayKeyStatus, "adopted key marks identity active")
	require.Equal(t, originalToken, ident.GatewayKeyToken, "adopted the pre-existing gateway key, not a new one")

	// No second /key/generate happened; the existing key was updated.
	gen2, upd, _ := gw.counts()
	require.Equal(t, 1, gen2, "no duplicate /key/generate (alias collision avoided)")
	require.GreaterOrEqual(t, upd, 1, "existing key was updated to the new binding")
	require.True(t, gw.canCall(originalToken, modelTwo), "adopted key now allows the new primary model")
}

// TestS129RebindWithStuckNoneIdentity covers the deployed Mary state:
// identity exists but gateway_key_status="none" with an empty token (key
// never provisioned under an older build). The rebind must provision the
// key and persist the new binding, not 502 or no-op.
func TestS129RebindWithStuckNoneIdentity(t *testing.T) {
	t.Parallel()

	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, "")
	f.createIdentity(t)
	// Force the deployed "stuck" state: status none, empty token.
	_, err := f.store.SetAgentIdentityGatewayKey(context.Background(), f.agentID, "", domain.GatewayKeyNone)
	require.NoError(t, err)

	f.bind(t, f.modelB.ID, "")

	var fetched domain.Agent
	doJSON(t, f.handler, http.MethodGet, pathAgentsPrefix+f.agentID, nil, http.StatusOK, &fetched)
	require.Equal(t, f.modelB.ID, fetched.AIModelID, "rebind over none-status identity must persist (S-129)")

	ident := f.identity(t)
	require.Equal(t, domain.GatewayKeyActive, ident.GatewayKeyStatus, "key must be provisioned to active")
}

// TestS129RebindWithLiveKeyPersists covers the deployed variant: the agent
// already has a provisioned gateway key. The rebind must converge the key
// AND persist the row.
func TestS129RebindWithLiveKeyPersists(t *testing.T) {
	t.Parallel()

	f := newGatewayFixture(t)
	f.bind(t, f.modelA.ID, f.modelB.ID)
	f.createIdentity(t)

	// Rebind primary A→B, clearing fallback (exact UI payload shape).
	f.bind(t, f.modelB.ID, "")

	var fetched domain.Agent
	doJSON(t, f.handler, http.MethodGet, pathAgentsPrefix+f.agentID, nil, http.StatusOK, &fetched)
	require.Equal(t, f.modelB.ID, fetched.AIModelID, "rebind with live key must persist (S-129)")
	require.Empty(t, fetched.FallbackAIModelID, "fallback clear must persist (S-129)")

	gen, upd, _ := f.gateway.counts()
	require.Equal(t, 1, gen, "identity provisioning generates one key")
	require.GreaterOrEqual(t, upd, 1, "rebind must converge the live key via /key/update")
}

// createUniqueTestDB creates a throwaway database on the same server as
// baseDSN and returns a DSN for it, so the Postgres S-129 test can run
// repeatedly without colliding with prior runs' rows.
func createUniqueTestDB(t *testing.T, baseDSN string) string {
	t.Helper()
	u, err := url.Parse(baseDSN)
	require.NoError(t, err, "parse base DSN")
	dbName := fmt.Sprintf("s129_test_%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseDSN)
	require.NoError(t, err, "connect to base DSN")
	defer pool.Close()

	_, err = pool.Exec(ctx, `CREATE DATABASE "`+dbName+`"`)
	require.NoError(t, err, "create throwaway database")

	u.Path = "/" + dbName
	return u.String()
}

// dropTestDB removes a throwaway database created by createUniqueTestDB.
func dropTestDB(t *testing.T, baseDSN, testDSN string) {
	t.Helper()
	u, err := url.Parse(testDSN)
	if err != nil {
		return
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		return
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, `DROP DATABASE IF EXISTS "`+dbName+`" WITH (FORCE)`)
}
