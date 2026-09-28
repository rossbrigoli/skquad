// S-175: chat turn cancellation ("stop button").
//
// The chat UI's stop button hits POST /agents/{agentID}/chat/cancel. The
// control plane marks the newest live user chat turn cancelled. The agent
// runtime polls the trigger message status (GET
// /agents/me/messages/{id}) between LLM/tool steps and before posting its
// reply, and abandons the turn when the message is cancelled — so tokens
// stop burning from the next step boundary onward.
//
// Cancellation is best-effort by design: an in-flight LLM call cannot be
// interrupted from outside, and only the single newest unanswered user
// turn is cancellable. Everything older is already answered or dead.

package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// cancelAgentChatTurn handles POST /agents/{agentID}/chat/cancel.
// 200 {"cancelled": true, "message_id": "..."} when a live turn was
// stopped, or 200 {"cancelled": false} when there was nothing to cancel
// (agent already replied, no pending turn, or thread was reset).
func (s *Server) cancelAgentChatTurn(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadAgentForAction(w, r, "talk")
	if !ok {
		return
	}
	cancelled, err := s.store.CancelChatTurn(
		s.pendingUserAuditCtx(r, "chat.cancel", "message", "", target.SquadID, nil),
		target.ID,
	)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"cancelled": false})
			return
		}
		writeStorageError(w, err)
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), target.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateAgentState)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cancelled":  true,
		"message_id": cancelled.ID,
	})
}

// getCurrentAgentMessage handles GET /agents/me/messages/{messageID}.
// The runtime uses it to check whether the turn it is processing was
// cancelled by the user (S-175). Scoped to messages addressed to the
// calling agent — anything else is 404, never 403, so the endpoint leaks
// nothing about other agents' traffic.
func (s *Server) getCurrentAgentMessage(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	msg, err := s.store.GetMessage(r.Context(), chi.URLParam(r, "messageID"))
	if err != nil || msg.ToAgentID != principal.Agent.ID {
		writeError(w, http.StatusNotFound, "not_found", "message not found")
		return
	}
	writeJSON(w, http.StatusOK, msg)
}
