// S-175: chat turn cancellation tests — the stop button endpoint and the
// runtime's status-poll endpoint.

package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func TestCancelAgentChatTurn(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{
		"name": "Cancel Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Cancel Agent",
	}, http.StatusCreated, &agent)

	var sent domain.Message
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/chat", map[string]any{
		"message": "stop me before you reply",
	}, http.StatusCreated, &sent)

	var cancelled struct {
		Cancelled bool   `json:"cancelled"`
		MessageID string `json:"message_id"`
	}
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/chat/cancel", nil, http.StatusOK, &cancelled)
	require.True(t, cancelled.Cancelled)
	require.Equal(t, sent.ID, cancelled.MessageID)

	var history []domain.Message
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID+"/chat", nil, http.StatusOK, &history)
	require.Len(t, history, 1)
	require.Equal(t, domain.MessageCancelled, history[0].Status)
	require.Equal(t, "cancelled by user", history[0].TerminalReason)
}

func TestCancelAgentChatTurnNothingToCancel(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{
		"name": "No Turn Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Idle Agent",
	}, http.StatusCreated, &agent)

	var cancelled struct {
		Cancelled bool `json:"cancelled"`
	}
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/chat/cancel", nil, http.StatusOK, &cancelled)
	require.False(t, cancelled.Cancelled)
}

func TestCancelAgentChatTurnAlreadyAnswered(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{
		"name": "Answered Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Answered Agent",
	}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	var sent domain.Message
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/chat", map[string]any{
		"message": "already answered",
	}, http.StatusCreated, &sent)

	// The agent replies with the trigger message as correlation id.
	var reply domain.Message
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id":    agent.ID,
		"type":           "reply",
		"message":        "too late, here is my answer",
		"correlation_id": sent.ID,
	}, http.StatusCreated, &reply)

	var cancelled struct {
		Cancelled bool `json:"cancelled"`
	}
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/chat/cancel", nil, http.StatusOK, &cancelled)
	require.False(t, cancelled.Cancelled, "an already-answered turn must not be cancellable")

	// The original message keeps its live status.
	var history []domain.Message
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID+"/chat", nil, http.StatusOK, &history)
	for _, msg := range history {
		if msg.ID == sent.ID {
			require.NotEqual(t, domain.MessageCancelled, msg.Status)
		}
	}
}

func TestAgentGetMessageScopedToOwnMessages(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{
		"name": "Scoped Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Scoped Agent",
	}, http.StatusCreated, &agent)
	var other domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Other Agent",
	}, http.StatusCreated, &other)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	var otherIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+other.ID+pathIdentity, nil, http.StatusCreated, &otherIdentity)
	otherCredential := crWriter.credentialTokens[otherIdentity.CredentialRef]

	var sent domain.Message
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/chat", map[string]any{
		"message": "mine",
	}, http.StatusCreated, &sent)

	// The addressed agent can read it (S-175 cancel polling).
	var fetched domain.Message
	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, pathMyMessagesPrefix+sent.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, sent.ID, fetched.ID)
	require.Equal(t, domain.MessagePending, fetched.Status)

	// Another agent gets 404, never the message.
	doAgentJSONNoBody(t, handler, other.ID, otherCredential, http.MethodGet, pathMyMessagesPrefix+sent.ID, nil, http.StatusNotFound)
}
