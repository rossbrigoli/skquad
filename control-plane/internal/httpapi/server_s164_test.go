package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-164: agent-to-agent messaging — peer roster endpoint, correlation-chain
// budget, and the send_message builtin provisioning surface.

const pathMyPeers = "/api/v1/agents/me/peers"

func TestS164PeersEndpointShowsSquadMatesOnly(t *testing.T) {
	f := newDelegationFixture(t)

	var peers []map[string]any
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodGet, pathMyPeers, nil, http.StatusOK, &peers)
	require.Len(t, peers, 1, "sender sees exactly the one squad-mate")
	require.Equal(t, f.workerID, peers[0]["id"])
	require.Equal(t, "Worker", peers[0]["name"])
	require.Contains(t, peers[0], "status")
	// The roster is identity-only: no prompts, identities, or model config.
	require.NotContains(t, peers[0], "system_prompt")
	require.NotContains(t, peers[0], "identity_id")
	require.NotContains(t, peers[0], "ai_model_id")

	var workerSees []map[string]any
	doAgentJSON(t, f.handler, f.workerID, f.workerCred, http.MethodGet, pathMyPeers, nil, http.StatusOK, &workerSees)
	require.Len(t, workerSees, 1)
	require.Equal(t, f.senderID, workerSees[0]["id"], "self is excluded from the roster")
}

func TestS164CorrelationChainBudgetStopsRunawayThreads(t *testing.T) {
	f := newDelegationFixture(t)

	// Seed consult without correlation; its id becomes the thread id.
	var seed domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID,
		"type":        "consult",
		"message":     "seed question",
	}, http.StatusCreated, &seed)
	require.NotEmpty(t, seed.ID)

	// Fill the thread: worker replies round-trips up to the budget.
	for i := 0; i < maxCorrelationChainMessages; i++ {
		var m domain.Message
		doAgentJSON(t, f.handler, f.workerID, f.workerCred, http.MethodPost, pathMyMessages, map[string]any{
			"to_agent_id":    f.senderID,
			"type":           "reply",
			"message":        "answer hop",
			"correlation_id": seed.ID,
		}, http.StatusCreated, &m)
	}

	// One past the budget: rejected with chain_exceeded, not silently dropped.
	rec := doAgentRequest(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id":    f.workerID,
		"type":           "consult",
		"message":        "one hop too many",
		"correlation_id": seed.ID,
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Equal(t, "chain_exceeded", errBody.Error.Code)

	// The denial is audited.
	audits, err := f.store.ListAudit(context.Background(), f.squadID, 100)
	require.NoError(t, err)
	found := false
	for _, a := range audits {
		if a.Action == "message.chain_exceeded" && a.ActorID == f.senderID {
			found = true
			require.Contains(t, string(a.Metadata), seed.ID)
		}
	}
	require.True(t, found, "chain_exceeded must be audited")

	// A fresh thread (no correlation) still works — the budget is per chain.
	var fresh domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID,
		"type":        "consult",
		"message":     "brand new question",
	}, http.StatusCreated, &fresh)
	require.NotEmpty(t, fresh.ID)
}

func TestS164InvalidCorrelationIDRejected(t *testing.T) {
	f := newDelegationFixture(t)

	doAgentJSONNoBody(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id":    f.workerID,
		"type":           "consult",
		"message":        "bad thread id",
		"correlation_id": "not-a-uuid",
	}, http.StatusBadRequest)
}

func TestS164CountMessagesByCorrelationRejectsEmpty(t *testing.T) {
	store := storage.NewMemoryStore()
	_, err := store.CountMessagesByCorrelation(context.Background(), "   ")
	require.ErrorIs(t, err, storage.ErrInvalidInput)
}

func TestS164SendMessageBuiltinIsSeededEnabled(t *testing.T) {
	store := storage.NewMemoryStore()
	tool, err := store.GetBuiltinTool(context.Background(), domain.BuiltinToolSendMessage)
	require.NoError(t, err)
	require.True(t, tool.Enabled, "send_message ships enabled (S-164)")

	// The original three keep their disabled-by-default posture.
	for _, name := range []string{domain.BuiltinToolExec, domain.BuiltinToolWebFetch, domain.BuiltinToolWebSearch} {
		original, err := store.GetBuiltinTool(context.Background(), name)
		require.NoError(t, err)
		require.False(t, original.Enabled, "%s must stay disabled by default", name)
	}
}

func TestS164AgentToolViewIncludesSendMessage(t *testing.T) {
	f := newDelegationFixture(t)

	var payload struct {
		Tools []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"tools"`
	}
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodGet, "/api/v1/agents/me/tools", nil, http.StatusOK, &payload)
	byName := map[string]bool{}
	for _, tool := range payload.Tools {
		byName[tool.Name] = tool.Enabled
	}
	require.Contains(t, byName, "send_message")
	require.True(t, byName["send_message"], "the runtime fetch must see send_message enabled")
}

func TestS164SendMessagePolicyValidation(t *testing.T) {
	require.Empty(t, domain.ValidateBuiltinPolicy(domain.BuiltinToolSendMessage, json.RawMessage(`{"timeoutSeconds":5,"maxMessageChars":2000}`)))
	require.NotEmpty(t, domain.ValidateBuiltinPolicy(domain.BuiltinToolSendMessage, json.RawMessage(`{"bogusKey":true}`)))
	require.NotEmpty(t, domain.ValidateBuiltinPolicy(domain.BuiltinToolSendMessage, json.RawMessage(`{"maxMessageChars":0}`)))
}
