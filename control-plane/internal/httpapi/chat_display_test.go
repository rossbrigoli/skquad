// S-272: the agent chat list must carry the sender's resolved display
// name (from_display) so the web UI renders the ACTUAL sender's avatar
// and name per message — not the logged-in viewer's.

package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func TestListAgentChatMessagesDecoratesFromDisplay(t *testing.T) {
	t.Parallel()
	handler := New(testConfig(), storage.NewMemoryStore())

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{
		"name": "Display Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Display Agent",
	}, http.StatusCreated, &agent)

	doJSONNoBody(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+"/chat",
		map[string]any{"message": "hello there"}, http.StatusCreated)

	var history []domain.Message
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID+"/chat", nil, http.StatusOK, &history)
	require.Len(t, history, 1)
	// Dev-mode sender is "Dev Admin" → first-token display "Dev"
	// (S-235 convention). The wire field must be present, not a GUID.
	require.Equal(t, "Dev", history[0].FromDisplay)
	require.NotEmpty(t, history[0].FromID)
}
