// Slash-command registry endpoint tests: the composer discovers reset /
// restart / wake through GET /agents/{id}/commands, and the endpoint
// enforces the same "talk" permission as the chat surface.

package httpapi

import (
	"net/http"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestListAgentChatCommands(t *testing.T) {
	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	agentID, _ := seedChat(t, handler)

	var res struct {
		AgentID  string `json:"agent_id"`
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Destructive bool   `json:"destructive"`
		} `json:"commands"`
	}
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agentID+"/commands", nil, http.StatusOK, &res)
	require.Equal(t, agentID, res.AgentID)

	names := make([]string, 0, len(res.Commands))
	for _, c := range res.Commands {
		require.NotEmpty(t, c.Description, "command %q must carry a description for the UI", c.Name)
		names = append(names, c.Name)
	}
	require.Equal(t, []string{"reset", "restart", "wake"}, names)

	// Destructive flags must match the confirm-gating contract the UI relies on.
	require.True(t, res.Commands[0].Destructive) // reset
	require.True(t, res.Commands[1].Destructive) // restart
	require.False(t, res.Commands[2].Destructive) // wake
}

func TestListAgentChatCommandsUnknownAgent(t *testing.T) {
	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+"nope"+"/commands", nil, http.StatusNotFound, &map[string]any{})
}
