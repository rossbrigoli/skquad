// S-162 tests: chat reset archives the transcript and excludes prior
// messages from history; restart deletes agent pods via the restarter.

package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

// fakeRestarter records restart calls.
type fakeRestarter struct {
	deleted int
	err     error
	calls   []string
}

func (f *fakeRestarter) RestartAgentPods(_ context.Context, agentID string) (int, error) {
	f.calls = append(f.calls, agentID)
	return f.deleted, f.err
}

// seedChat creates a squad + agent and sends three user chat messages,
// returning the agent id and squad id.
func seedChat(t *testing.T, handler http.Handler) (string, string) {
	t.Helper()
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "Reset Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Chatty", "role": "worker", "system_prompt": "hi",
	}, http.StatusCreated, &agent)
	for i := 0; i < 3; i++ {
		var msg domain.Message
		doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/chat", map[string]any{
			"message": "hello there",
		}, http.StatusCreated, &msg)
	}
	return agent.ID, squad.ID
}

func TestChatResetArchivesAndExcludes(t *testing.T) {
	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	agentID, squadID := seedChat(t, handler)

	// Before reset: 3 messages visible.
	var before []map[string]any
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agentID+"/chat", nil, http.StatusOK, &before)
	require.Len(t, before, 3)

	// Reset.
	var res map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+"/chat/reset", nil, http.StatusOK, &res)
	require.Equal(t, float64(3), res["archived"])
	require.NotEmpty(t, res["reset_at"])

	// After reset: chat is empty.
	var after []map[string]any
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agentID+"/chat", nil, http.StatusOK, &after)
	require.Empty(t, after)

	// Transcript archived into agent memory with chat_reset provenance.
	mem, err := store.ListAgentMemory(context.Background(), agentID, squadID, nil, 10)
	require.NoError(t, err)
	require.Len(t, mem, 1)
	require.Equal(t, "chat_reset", mem[0].Provenance)
	require.Contains(t, mem[0].Content, "hello there")
	require.Contains(t, mem[0].Content, "user:")
}

func TestChatResetTwiceArchivesNothing(t *testing.T) {
	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	agentID, _ := seedChat(t, handler)

	var res map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+"/chat/reset", nil, http.StatusOK, &res)
	require.Equal(t, float64(3), res["archived"])

	// Second reset: nothing new since the boundary.
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+"/chat/reset", nil, http.StatusOK, &res)
	require.Equal(t, float64(0), res["archived"])
}

func TestChatResetThenNewMessagesVisible(t *testing.T) {
	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	agentID, _ := seedChat(t, handler)
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+"/chat/reset", nil, http.StatusOK, &map[string]any{})

	// A fresh message after the boundary shows up.
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+"/chat", map[string]any{"message": "post-reset"}, http.StatusCreated, &map[string]any{})
	var after []map[string]any
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agentID+"/chat", nil, http.StatusOK, &after)
	require.Len(t, after, 1)
}

func TestRestartAgentCallsRestarter(t *testing.T) {
	store := storage.NewMemoryStore()
	seedHandler := New(testConfig(), store)
	agentID, _ := seedChat(t, seedHandler)

	fr := &fakeRestarter{deleted: 1}
	handler := NewWithPodRestarter(testConfig(), store, fr)
	var res map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+"/restart", nil, http.StatusAccepted, &res)
	require.Equal(t, true, res["restarting"])
	require.Equal(t, float64(1), res["pods"])
	require.Equal(t, []string{agentID}, fr.calls)
}

func TestRestartAgentUnavailable(t *testing.T) {
	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	agentID, _ := seedChat(t, handler)
	rec := doRawAuth(t, handler, "", http.MethodPost, pathAgentsPrefix+agentID+"/restart")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
