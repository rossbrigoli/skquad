// WP4 tests — revoke/deprecate/force-delete cascades converge virtual
// keys (ADR-0010 D9). The invariant under test: after a revoke,
// deprecation, or force-delete completes successfully, NO live virtual
// key can still call the removed model. The recording gateway tracks the
// live allow-list per key token (canCall), so a cascade that skipped key
// convergence would leave the model callable and these tests would FAIL.

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	modelOneName = "cascade/one"
	modelTwoName = "cascade/two"
)

type cascadeFixture struct {
	handler http.Handler
	store   *storage.MemoryStore
	gateway *recordingGateway
	server  *httptest.Server

	ownerID string
	squadID string

	agentFB    *domain.Agent // primary=modelOne, fallback=modelTwo
	agentSolo  *domain.Agent // primary=modelTwo, no fallback
	agentOther *domain.Agent // primary=modelOne only — untouched by modelTwo cascades

	modelOne domain.AIModel
	modelTwo domain.AIModel
}

func newCascadeFixture(t *testing.T) *cascadeFixture {
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
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Cascade Squad"}, http.StatusCreated, &squad)
	mkAgent := func(name string) *domain.Agent {
		var agent domain.Agent
		doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": name}, http.StatusCreated, &agent)
		return &agent
	}
	fb := mkAgent("Fallback Agent")
	solo := mkAgent("Solo Agent")
	other := mkAgent("Other Agent")

	provider := createTestProvider(t, handler, "cascade-prov")
	modelOne := createTestAIModel(t, handler, provider.ID, modelOneName)
	modelTwo := createTestAIModel(t, handler, provider.ID, modelTwoName)
	doJSON(t, handler, http.MethodPut, "/api/v1/users/"+squad.OwnerID+"/models",
		map[string]any{"model_ids": []string{modelOne.ID, modelTwo.ID}}, http.StatusOK, &[]domain.AIModel{})

	patch := func(agent *domain.Agent, primary, fallback string) {
		var updated domain.Agent
		doJSON(t, handler, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
			"ai_model_id":          primary,
			"fallback_ai_model_id": fallback,
		}, http.StatusOK, &updated)
		agent.AIModelID = updated.AIModelID
		agent.FallbackAIModelID = updated.FallbackAIModelID
	}
	patch(fb, modelOne.ID, modelTwo.ID)
	patch(solo, modelTwo.ID, "")
	patch(other, modelOne.ID, "")

	identity := func(agent *domain.Agent) {
		var id domain.AgentIdentity
		doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &id)
		require.Equal(t, domain.GatewayKeyActive, id.GatewayKeyStatus)
	}
	identity(fb)
	identity(solo)
	identity(other)

	return &cascadeFixture{
		handler: handler, store: store, gateway: gw, server: server,
		ownerID: squad.OwnerID, squadID: squad.ID,
		agentFB: fb, agentSolo: solo, agentOther: other,
		modelOne: modelOne, modelTwo: modelTwo,
	}
}

func (f *cascadeFixture) token(t *testing.T, agentID string) string {
	t.Helper()
	identity, err := f.store.GetAgentIdentity(context.Background(), agentID)
	require.NoError(t, err)
	return identity.GatewayKeyToken
}

func (f *cascadeFixture) auditActions(t *testing.T) []string {
	t.Helper()
	entries, err := f.store.ListAudit(context.Background(), "", 1000)
	require.NoError(t, err)
	actions := make([]string, 0, len(entries))
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	return actions
}

// ---------------------------------------------------------------------------
// A. Revoke-model-from-user cascade
// ---------------------------------------------------------------------------

func TestRevokeBoundModelReturns409WithSlots(t *testing.T) {
	f := newCascadeFixture(t)

	var conflict struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Usage   []struct {
			UserID    string `json:"user_id"`
			UserEmail string `json:"user_email"`
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			SquadID   string `json:"squad_id"`
			Slot      string `json:"slot"`
		} `json:"usage"`
	}
	doJSON(t, f.handler, http.MethodDelete, "/api/v1/users/"+f.ownerID+"/models/"+f.modelTwo.ID, nil, http.StatusConflict, &conflict)
	require.Equal(t, "in_use", conflict.Error)
	require.NotEmpty(t, conflict.Message)
	require.Len(t, conflict.Usage, 2)

	byAgent := map[string]string{}
	for _, u := range conflict.Usage {
		byAgent[u.AgentID] = u.Slot
		require.Equal(t, f.ownerID, u.UserID)
		require.NotEmpty(t, u.UserEmail)
		require.Equal(t, f.squadID, u.SquadID)
	}
	// modelTwo is the fallback of agentFB and the primary of agentSolo.
	require.Equal(t, "fallback", byAgent[f.agentFB.ID])
	require.Equal(t, "primary", byAgent[f.agentSolo.ID])
	require.NotContains(t, byAgent, f.agentOther.ID, "agent not bound to modelTwo must not appear")

	// Nothing changed without force: grant intact, keys still live.
	grants, err := f.store.ListUserModelGrants(context.Background(), f.ownerID)
	require.NoError(t, err)
	require.Len(t, grants, 2)
	require.True(t, f.gateway.canCall(f.token(t, f.agentSolo.ID), modelTwoName))
}

func TestForceRevokeClearsSlotsConvergesKeysAndAudits(t *testing.T) {
	f := newCascadeFixture(t)

	var report cascadeReport
	doJSON(t, f.handler, http.MethodDelete, "/api/v1/users/"+f.ownerID+"/models/"+f.modelTwo.ID+"?force=true", nil, http.StatusOK, &report)
	require.Equal(t, "revoke", report.Operation)
	require.Equal(t, f.modelTwo.ID, report.ModelID)
	require.Equal(t, 2, report.AffectedAgents)
	require.Equal(t, []string{f.ownerID}, report.AffectedUsers)
	require.Empty(t, report.Failures)
	require.Len(t, report.KeyActions, 2)

	// Slots cleared.
	agent, err := f.store.GetAgent(context.Background(), f.agentFB.ID)
	require.NoError(t, err)
	require.Empty(t, agent.FallbackAIModelID, "fallback slot cleared")
	require.Equal(t, f.modelOne.ID, agent.AIModelID, "primary untouched")
	agent, err = f.store.GetAgent(context.Background(), f.agentSolo.ID)
	require.NoError(t, err)
	require.Empty(t, agent.AIModelID, "primary slot cleared")

	// Grant revoked.
	grants, err := f.store.ListUserModelGrants(context.Background(), f.ownerID)
	require.NoError(t, err)
	for _, g := range grants {
		require.NotEqual(t, f.modelTwo.ID, g.AIModelID)
	}

	// Keys converged: fallback agent updated (shrunk), solo agent revoked.
	actions := map[string]string{}
	for _, a := range report.KeyActions {
		actions[a.AgentID] = a.Action
	}
	require.Equal(t, "updated", actions[f.agentFB.ID])
	require.Equal(t, "revoked", actions[f.agentSolo.ID])

	// Audit: the revoke itself, per-key convergence, and the cascade summary.
	audit := f.auditActions(t)
	require.Contains(t, audit, "aimodel.grant.revoke")
	require.Contains(t, audit, "aimodel.cascade.key_converged")
	require.Contains(t, audit, "aimodel.cascade.revoke")

	// Owner notified (inbox).
	inbox, err := f.store.ListInboxMessages(context.Background(), f.ownerID, true, 50)
	require.NoError(t, err)
	require.NotEmpty(t, inbox, "cascade must notify the affected owner")
}

func TestRevokeFallbackOnlyKeyShrinksAgentKeepsPrimary(t *testing.T) {
	f := newCascadeFixture(t)
	token := f.token(t, f.agentFB.ID)
	require.True(t, f.gateway.canCall(token, modelOneName))
	require.True(t, f.gateway.canCall(token, modelTwoName))

	doJSON(t, f.handler, http.MethodDelete, "/api/v1/users/"+f.ownerID+"/models/"+f.modelTwo.ID+"?force=true", nil, http.StatusOK, &cascadeReport{})

	// The key shrank: fallback model no longer callable, primary still is.
	require.False(t, f.gateway.canCall(token, modelTwoName), "revoked fallback must leave the key")
	require.True(t, f.gateway.canCall(token, modelOneName), "agent keeps working on its primary")

	// Identity still active — the agent was not revoked.
	identity, err := f.store.GetAgentIdentity(context.Background(), f.agentFB.ID)
	require.NoError(t, err)
	require.Equal(t, domain.GatewayKeyActive, identity.GatewayKeyStatus)

	// The router fallback mapping was cleared too (no dangling failover).
	var cleared bool
	for _, body := range f.gateway.updated {
		if body["key"] == token {
			rs := body["router_settings"].(map[string]any)
			require.Empty(t, rs["fallbacks"])
			cleared = true
		}
	}
	require.True(t, cleared, "expected an update call for the fallback agent's key")
}

func TestRevokePrimaryNoFallbackKeyRevoked(t *testing.T) {
	f := newCascadeFixture(t)
	token := f.token(t, f.agentSolo.ID)
	require.True(t, f.gateway.canCall(token, modelTwoName))

	doJSON(t, f.handler, http.MethodDelete, "/api/v1/users/"+f.ownerID+"/models/"+f.modelTwo.ID+"?force=true", nil, http.StatusOK, &cascadeReport{})

	require.False(t, f.gateway.canCall(token, modelTwoName), "agent left without a primary must not keep a live key")
	identity, err := f.store.GetAgentIdentity(context.Background(), f.agentSolo.ID)
	require.NoError(t, err)
	require.Equal(t, domain.GatewayKeyRevoked, identity.GatewayKeyStatus)
	require.Contains(t, f.gateway.deleted, token)
}

func TestRevokeUngrantedReturns404(t *testing.T) {
	f := newCascadeFixture(t)
	// An existing model the user does NOT hold → grant_not_found (distinct
	// from the model-itself-missing 404).
	unheld := createTestAIModel(t, f.handler, f.modelOne.ProviderID, "cascade/unheld")
	var body map[string]map[string]string
	doJSONAuth(t, f.handler, "Bearer admin", http.MethodDelete, "/api/v1/users/"+f.ownerID+"/models/"+unheld.ID, nil, http.StatusNotFound, &body)
	require.Equal(t, "grant_not_found", body["error"]["code"])
}

// ---------------------------------------------------------------------------
// B. Deprecation cascade
// ---------------------------------------------------------------------------

func TestDeprecateConvergesAllBoundAgentsAndReportsCounts(t *testing.T) {
	f := newCascadeFixture(t)
	fbToken := f.token(t, f.agentFB.ID)
	soloToken := f.token(t, f.agentSolo.ID)

	var report cascadeReport
	doJSON(t, f.handler, http.MethodPost, "/api/v1/ai-models/"+f.modelTwo.ID+"/deprecate", nil, http.StatusOK, &report)
	require.Equal(t, "deprecate", report.Operation)
	require.Equal(t, 2, report.AffectedAgents, "both agents bound to modelTwo")
	require.Equal(t, []string{f.ownerID}, report.AffectedUsers)
	require.Empty(t, report.Failures)

	// Fallback agent: key shrinks, keeps primary. Agent stays usable.
	require.False(t, f.gateway.canCall(fbToken, modelTwoName))
	require.True(t, f.gateway.canCall(fbToken, modelOneName))
	// Solo agent: primary deprecated → key revoked.
	require.False(t, f.gateway.canCall(soloToken, modelTwoName))

	// Bindings cleared so nothing keeps referencing the deprecated model.
	agent, err := f.store.GetAgent(context.Background(), f.agentFB.ID)
	require.NoError(t, err)
	require.Empty(t, agent.FallbackAIModelID)
	agent, err = f.store.GetAgent(context.Background(), f.agentSolo.ID)
	require.NoError(t, err)
	require.Empty(t, agent.AIModelID)

	audit := f.auditActions(t)
	require.Contains(t, audit, "aimodel.deprecate")
	require.Contains(t, audit, "aimodel.cascade.key_converged")
	require.Contains(t, audit, "aimodel.cascade.deprecate")
}

func TestDeprecateUnboundModelReportsZeroCounts(t *testing.T) {
	f := newCascadeFixture(t)
	spare := createTestAIModel(t, f.handler, f.modelOne.ProviderID, "cascade/spare")
	var report cascadeReport
	doJSON(t, f.handler, http.MethodPost, "/api/v1/ai-models/"+spare.ID+"/deprecate", nil, http.StatusOK, &report)
	require.Equal(t, 0, report.AffectedAgents)
	require.Empty(t, report.AffectedUsers)
	require.Empty(t, report.Failures)
}

// ---------------------------------------------------------------------------
// C. Force-delete cascade completion
// ---------------------------------------------------------------------------

func TestForceDeleteConvergesKeysNotJustUnbinds(t *testing.T) {
	f := newCascadeFixture(t)
	fbToken := f.token(t, f.agentFB.ID)
	otherToken := f.token(t, f.agentOther.ID)
	soloToken := f.token(t, f.agentSolo.ID)

	// Delete modelOne: agentFB (primary) and agentOther (primary) bound.
	doJSONNoBody(t, f.handler, http.MethodDelete, "/api/v1/ai-models/"+f.modelOne.ID+"?force=true", nil, http.StatusNoContent)

	// WP2 only unbound; WP4 must ALSO converge the keys.
	require.False(t, f.gateway.canCall(fbToken, modelOneName), "force-delete must converge the key, not just unbind")
	require.False(t, f.gateway.canCall(otherToken, modelOneName))
	require.Contains(t, f.gateway.deleted, fbToken)
	require.Contains(t, f.gateway.deleted, otherToken)
	// The untouched agent's key is unaffected.
	require.True(t, f.gateway.canCall(soloToken, modelTwoName))

	audit := f.auditActions(t)
	require.Contains(t, audit, "aimodel.delete")
	require.Contains(t, audit, "aimodel.cascade.key_converged")
}

func TestForceDeleteBlockedByConvergenceFailure(t *testing.T) {
	f := newCascadeFixture(t)
	f.gateway.mu.Lock()
	f.gateway.failDelete = true // revoke calls fail → convergence cannot succeed
	f.gateway.mu.Unlock()

	var body map[string]any
	doJSON(t, f.handler, http.MethodDelete, "/api/v1/ai-models/"+f.modelOne.ID+"?force=true", nil, http.StatusBadGateway, &body)
	require.Equal(t, "convergence_failed", body["error"])
	require.NotEmpty(t, body["failures"])

	// The model was NOT deleted while a key could still call it.
	_, err := f.store.GetAIModel(context.Background(), f.modelOne.ID)
	require.NoError(t, err, "delete must be skipped when key convergence fails")
}

// ---------------------------------------------------------------------------
// PUT (grant-set) path must not strand live keys either
// ---------------------------------------------------------------------------

func TestSetUserModelsRemovalGuardedAndForceCascades(t *testing.T) {
	f := newCascadeFixture(t)
	soloToken := f.token(t, f.agentSolo.ID)
	path := "/api/v1/users/" + f.ownerID + "/models"

	// Removing modelTwo from the set while agents bind it → 409 in_use.
	var conflict struct {
		Error string              `json:"error"`
		Usage []aiModelUsageEntry `json:"usage"`
	}
	doJSON(t, f.handler, http.MethodPut, path, map[string]any{"model_ids": []string{f.modelOne.ID}}, http.StatusConflict, &conflict)
	require.Equal(t, "in_use", conflict.Error)
	require.Len(t, conflict.Usage, 2)

	// With force: grant set applied AND keys converged.
	var granted []domain.AIModel
	doJSON(t, f.handler, http.MethodPut, path+"?force=true", map[string]any{"model_ids": []string{f.modelOne.ID}}, http.StatusOK, &granted)
	require.Equal(t, []string{f.modelOne.ID}, modelIDs(granted))
	require.False(t, f.gateway.canCall(soloToken, modelTwoName), "forced grant removal must converge the live key")
	require.True(t, f.gateway.canCall(f.token(t, f.agentFB.ID), modelOneName), "unrelated bindings untouched")
}

// ---------------------------------------------------------------------------
// Partial failure: truthful reporting, others still processed
// ---------------------------------------------------------------------------

func TestCascadePartialFailureSurfacedOthersProcessed(t *testing.T) {
	f := newCascadeFixture(t)
	f.gateway.mu.Lock()
	f.gateway.failUpdate = true // agentFB's converge (update) fails; agentSolo's revoke works
	f.gateway.mu.Unlock()

	var report cascadeReport
	doJSON(t, f.handler, http.MethodDelete, "/api/v1/users/"+f.ownerID+"/models/"+f.modelTwo.ID+"?force=true", nil, http.StatusOK, &report)

	// The failure is surfaced, not swallowed.
	require.Len(t, report.Failures, 1)
	require.Equal(t, f.agentFB.ID, report.Failures[0].AgentID)
	require.Equal(t, "converge", report.Failures[0].Stage)
	require.NotEmpty(t, report.Failures[0].Error)

	// The other agent was still processed: revoked (primary cleared).
	require.Len(t, report.KeyActions, 1)
	require.Equal(t, f.agentSolo.ID, report.KeyActions[0].AgentID)
	require.Equal(t, "revoked", report.KeyActions[0].Action)
	require.False(t, f.gateway.canCall(f.token(t, f.agentSolo.ID), modelTwoName))

	// The failed agent's key is untouched (still live with the revoked
	// model) — which is exactly why the failure must be reported.
	require.True(t, f.gateway.canCall(f.token(t, f.agentFB.ID), modelTwoName))

	// The grant itself was still revoked (the requested core action).
	grants, err := f.store.ListUserModelGrants(context.Background(), f.ownerID)
	require.NoError(t, err)
	for _, g := range grants {
		require.NotEqual(t, f.modelTwo.ID, g.AIModelID)
	}

	// The failure is in the audit trail too.
	entries, err := f.store.ListAudit(context.Background(), "", 1000)
	require.NoError(t, err)
	var found bool
	for _, e := range entries {
		if e.Action == "aimodel.cascade.revoke" {
			var r cascadeReport
			require.NoError(t, json.Unmarshal(e.Metadata, &r))
			require.Len(t, r.Failures, 1)
			found = true
		}
	}
	require.True(t, found, "cascade summary audit must carry the failures")
}

// ---------------------------------------------------------------------------
// D. THE invariant: no live virtual key can call the revoked model.
// This test FAILS if key convergence is skipped: the recording gateway's
// live allow-list would still contain the model for the affected tokens.
// ---------------------------------------------------------------------------

func TestNoLiveKeyCanCallRevokedModel(t *testing.T) {
	f := newCascadeFixture(t)
	agents := []string{f.agentFB.ID, f.agentSolo.ID, f.agentOther.ID}
	tokens := map[string]string{}
	for _, id := range agents {
		tokens[id] = f.token(t, id)
	}
	// Pre-condition: the revoked model IS callable by its two binders.
	require.True(t, f.gateway.canCall(tokens[f.agentFB.ID], modelTwoName))
	require.True(t, f.gateway.canCall(tokens[f.agentSolo.ID], modelTwoName))

	// 1. Forced grant revoke.
	doJSON(t, f.handler, http.MethodDelete, "/api/v1/users/"+f.ownerID+"/models/"+f.modelTwo.ID+"?force=true", nil, http.StatusOK, &cascadeReport{})
	for _, id := range agents {
		require.False(t, f.gateway.canCall(tokens[id], modelTwoName), "live key of agent %s can still call revoked model", id)
	}

	// 2. Deprecation of the remaining model.
	doJSON(t, f.handler, http.MethodPost, "/api/v1/ai-models/"+f.modelOne.ID+"/deprecate", nil, http.StatusOK, &cascadeReport{})
	for _, id := range agents {
		require.False(t, f.gateway.canCall(tokens[id], modelOneName), "live key of agent %s can still call deprecated model", id)
	}

	// And nothing anywhere in the gateway still allows the removed models.
	f.gateway.mu.Lock()
	defer f.gateway.mu.Unlock()
	for token, models := range f.gateway.live {
		for _, m := range models {
			require.NotEqual(t, modelTwoName, m, "token %s still allows revoked model", token)
			require.NotEqual(t, modelOneName, m, "token %s still allows deprecated model", token)
		}
	}
}
