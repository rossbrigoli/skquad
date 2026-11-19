// Slash-command registry for the agent chat composer.
//
// The web composer must suggest chat slash commands ("/reset", "/restart",
// "/wake") as the user types, without hardcoding the list in the frontend.
// This registry is the single source of truth: adding a new chat command
// here (alongside its action route) makes it discoverable through
// GET /agents/{agentID}/commands with no frontend change required.

package httpapi

import (
	"net/http"
)

// slashChatCommand describes one composer-level slash command.
type slashChatCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Destructive marks commands that mutate state beyond the chat itself
	// (archiving history, evicting pods). The UI flags these and routes
	// them through a confirmation dialog before executing.
	Destructive bool `json:"destructive"`
}

// agentChatCommands lists the chat slash commands. Keep in sync with the
// action routes registered in server.go (chat/reset, restart, wake).
func agentChatCommands() []slashChatCommand {
	return []slashChatCommand{
		{
			Name:        "reset",
			Description: "Reset the chat thread — earlier messages are archived to the agent's memory",
			Destructive: true,
		},
		{
			Name:        "restart",
			Description: "Restart the agent — its pod is evicted and resumes on the next message",
			Destructive: true,
		},
		{
			Name:        "wake",
			Description: "Wake the agent now instead of waiting for it to pick up the message",
			Destructive: false,
		},
	}
}

// listAgentChatCommands serves the slash-command registry to the composer.
// Authorization mirrors the chat surface itself: the caller must be able to
// "talk" to the agent.
func (s *Server) listAgentChatCommands(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadAgentForAction(w, r, "talk")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": target.ID,
		"commands": agentChatCommands(),
	})
}
