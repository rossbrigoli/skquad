// S-222 regression tests: "Delete Squad is not working".
//
// Root cause: RevokeAgentKey posted {"key": token} to LiteLLM's
// /key/delete, which the current gateway rejects with 422 ("At least one
// of 'keys' or 'key_aliases' must be provided"). The delete handlers
// treated revoke failure as fatal → 502, blocking squad/agent deletion.
//
// Contract under test:
//  1. RevokeAgentKey sends {"keys": [token]}.
//  2. A 404 / "No keys found" from the gateway is an idempotent
//     success (key already gone), not an error.
//  3. A genuine gateway failure (5xx) surfaces as an error from the
//     client, but deleteAgent/deleteSquad still return 204 and still
//     perform the store delete + k8s credential cleanup.
//  4. The happy path revokes the live key at the gateway with the
//     corrected payload.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Client-level: payload shape + idempotency
// ---------------------------------------------------------------------------

func TestS222RevokeAgentKeySendsKeysList(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/key/delete", r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": 1})
	}))
	t.Cleanup(srv.Close)

	client, err := newLiteLLMGatewayClient(srv.URL, "sk-master")
	require.NoError(t, err)
	require.NoError(t, client.RevokeAgentKey(context.Background(), "tok-live-123"))

	// The exact wire shape LiteLLM requires (S-222): a "keys" list, no
	// bare "key" field.
	require.Equal(t, map[string]any{"keys": []any{"tok-live-123"}}, gotBody)
}

func TestS222RevokeAgentKeyNotFoundIsIdempotentSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "No keys found."})
	}))
	t.Cleanup(srv.Close)

	client, err := newLiteLLMGatewayClient(srv.URL, "sk-master")
	require.NoError(t, err)
	// Key already gone at the gateway → revoke succeeds, no error (S-222).
	require.NoError(t, client.RevokeAgentKey(context.Background(), "tok-gone"))
}

func TestS222RevokeAgentKeyGenuineFailureSurfaces(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	t.Cleanup(srv.Close)

	client, err := newLiteLLMGatewayClient(srv.URL, "sk-master")
	require.NoError(t, err)
	err = client.RevokeAgentKey(context.Background(), "tok-live")
	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
}

// ---------------------------------------------------------------------------
// Handler-level: delete flows survive gateway failures
// ---------------------------------------------------------------------------

// seedActiveGatewayKey ensures the agent has an identity row with an
// active gateway key token, so the delete flow exercises the revoke path.
func seedActiveGatewayKey(t *testing.T, store *storage.MemoryStore, agentID, token string) *domain.AgentIdentity {
	t.Helper()
	ctx := context.Background()
	identity, err := store.GetAgentIdentity(ctx, agentID)
	if errors.Is(err, storage.ErrNotFound) {
		identity, err = store.CreateAgentIdentity(ctx, &domain.AgentIdentity{
			AgentID:       agentID,
			CredentialRef: "secret/cred-" + agentID,
			VirtualKeyRef: "secret/vkey-" + agentID,
		})
		require.NoError(t, err)
	} else {
		require.NoError(t, err)
	}
	identity, err = store.SetAgentIdentityGatewayKey(ctx, agentID, token, domain.GatewayKeyActive)
	require.NoError(t, err)
	return identity
}

func s222GatewayServer(t *testing.T, gw *recordingGateway) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(gw.handler())
	t.Cleanup(srv.Close)
	return srv
}

func s222HandlerWithGateway(t *testing.T, gw *recordingGateway) (http.Handler, *storage.MemoryStore, *fakeCRWriter) {
	t.Helper()
	cfg := testConfig()
	cfg.LiteLLMAdminURL = s222GatewayServer(t, gw).URL
	cfg.LiteLLMMasterKey = "sk-test-master"
	store := storage.NewMemoryStore()
	cr := &fakeCRWriter{}
	return NewWithCRWriter(cfg, store, cr), store, cr
}

func TestS222DeleteAgentGatewayFiveHundredStillDeletes(t *testing.T) {
	t.Parallel()

	handler, store, cr := s222HandlerWithGateway(t, &recordingGateway{failDelete: true})

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "S222 Fail Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+"/agents", map[string]any{"name": "Fail Agent"}, http.StatusCreated, &agent)
	identity := seedActiveGatewayKey(t, store, agent.ID, "tok-s222-agent-fail")

	// Gateway revoke fails with 500 — the delete must still succeed (S-222).
	doJSONNoBody(t, handler, http.MethodDelete, pathAgentsPrefix+agent.ID, nil, http.StatusNoContent)

	_, err := store.GetAgent(context.Background(), agent.ID)
	require.ErrorIs(t, err, storage.ErrNotFound, "agent row must be deleted despite gateway failure")
	require.Contains(t, cr.deletedCredentialRefs, identity.CredentialRef)
	require.Contains(t, cr.deletedCredentialRefs, identity.VirtualKeyRef)
}

func TestS222DeleteSquadCascadeGatewayFiveHundredStillDeletes(t *testing.T) {
	t.Parallel()

	handler, store, cr := s222HandlerWithGateway(t, &recordingGateway{failDelete: true})

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "S222 Cascade Fail Squad"}, http.StatusCreated, &squad)
	var agentA, agentB domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+"/agents", map[string]any{"name": "Cascade A"}, http.StatusCreated, &agentA)
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+"/agents", map[string]any{"name": "Cascade B"}, http.StatusCreated, &agentB)
	idA := seedActiveGatewayKey(t, store, agentA.ID, "tok-s222-cascade-a")
	idB := seedActiveGatewayKey(t, store, agentB.ID, "tok-s222-cascade-b")

	// Gateway revoke fails with 500 for both agents — squad delete must
	// still succeed (S-222).
	doJSONNoBody(t, handler, http.MethodDelete, pathSquadsPrefix+squad.ID, nil, http.StatusNoContent)

	_, err := store.GetSquad(context.Background(), squad.ID)
	require.ErrorIs(t, err, storage.ErrNotFound, "squad must be deleted despite gateway failure")
	for _, id := range []*domain.AgentIdentity{idA, idB} {
		require.Contains(t, cr.deletedCredentialRefs, id.CredentialRef)
		require.Contains(t, cr.deletedCredentialRefs, id.VirtualKeyRef)
	}
}

func TestS222DeleteAgentAlreadyGoneAtGatewaySucceeds(t *testing.T) {
	t.Parallel()

	// unknownKeys404 mirrors the real gateway: the seeded token was never
	// issued there, so /key/delete answers 404 "No keys found" — the
	// idempotent path.
	gw := &recordingGateway{unknownKeys404: true, live: map[string][]string{}}
	handler, store, cr := s222HandlerWithGateway(t, gw)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "S222 Gone Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+"/agents", map[string]any{"name": "Gone Agent"}, http.StatusCreated, &agent)
	identity := seedActiveGatewayKey(t, store, agent.ID, "tok-never-issued")

	doJSONNoBody(t, handler, http.MethodDelete, pathAgentsPrefix+agent.ID, nil, http.StatusNoContent)

	_, err := store.GetAgent(context.Background(), agent.ID)
	require.ErrorIs(t, err, storage.ErrNotFound)
	require.Contains(t, cr.deletedCredentialRefs, identity.CredentialRef)
}

func TestS222DeleteAgentHappyPathRevokesKeysPayload(t *testing.T) {
	t.Parallel()

	// Lenient-but-faithful fake: a wrong payload shape (bare "key")
	// would be rejected 422 and never recorded in gw.deleted, so this
	// assertion proves the corrected {"keys":[token]} shape reached the
	// gateway through the full delete flow.
	gw := &recordingGateway{live: map[string][]string{"tok-live-happy": {"gpt-4o"}}}
	handler, store, _ := s222HandlerWithGateway(t, gw)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "S222 Happy Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+"/agents", map[string]any{"name": "Happy Agent"}, http.StatusCreated, &agent)
	seedActiveGatewayKey(t, store, agent.ID, "tok-live-happy")

	doJSONNoBody(t, handler, http.MethodDelete, pathAgentsPrefix+agent.ID, nil, http.StatusNoContent)

	require.Contains(t, gw.deleted, "tok-live-happy", "gateway must receive the revoke with the keys-list payload")
	_, err := store.GetAgent(context.Background(), agent.ID)
	require.ErrorIs(t, err, storage.ErrNotFound)
}
