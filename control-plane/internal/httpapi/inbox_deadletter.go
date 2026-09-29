package httpapi

// S-174: agent inbox observability, owner dead-letter replay, and the
// platform-admin dead-letter screen (replay/prune).
//
// Auth model:
//   - GET /agents/{id}/inbox                     squad owner or platform_admin
//   - POST /agents/{id}/messages/{mid}/replay    squad owner or platform_admin
//   - GET|POST|DELETE /admin/dead-letters[...]   platform_admin only
//
// Replay semantics (identical for owner and admin): dead -> pending,
// attempts reset to 0, expiry refreshed by the default TTL. The
// correlation chain is untouched — the chain budget gates new sends, so
// an operator replay is a fresh delivery attempt, not budget burn.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	auditDeadLetterReplayed = "dead_letter.replayed"
	auditDeadLetterPruned   = "dead_letter.pruned"
)

// loadAgentOwnerOrAdmin loads the {agentID} under the same owner-or-admin
// rule the inbox screens use: squad owner or platform_admin, no grant-based
// access (an inbox is not a shared workspace).
func (s *Server) loadAgentOwnerOrAdmin(w http.ResponseWriter, r *http.Request) (*domain.Agent, bool) {
	agent, err := s.store.GetAgent(r.Context(), chi.URLParam(r, "agentID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if _, ok := s.ensureOwnedOrAdminSquad(w, r, agent.SquadID); !ok {
		return nil, false
	}
	return agent, true
}

// getAgentInbox serves the owner-facing observability panel: pending
// (count + oldest waiting), retrying (attempts/max/next), recently
// delivered, and dead (with terminal_reason). Read-only.
func (s *Server) getAgentInbox(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadAgentOwnerOrAdmin(w, r)
	if !ok {
		return
	}
	limit := boundedIntQuery(r, "limit", 50, 200)
	snapshot, err := s.store.ListAgentInbox(r.Context(), agent.ID, limit)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

// replayAgentDeadMessage lets the agent's owner (or a platform admin)
// put one dead letter back on the queue.
func (s *Server) replayAgentDeadMessage(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.loadAgentOwnerOrAdmin(w, r)
	if !ok {
		return
	}
	messageID := chi.URLParam(r, "messageID")
	msg, err := s.store.GetMessage(r.Context(), messageID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if msg.ToAgentID != agent.ID {
		writeError(w, http.StatusNotFound, "not_found", "message is not addressed to this agent")
		return
	}
	s.replayDeadLetter(w, r, msg, agent.ID)
}

// listAdminDeadLetters is the platform_admin cross-squad dead-letter
// screen: filter by squad/agent/type/reason/time window.
func (s *Server) listAdminDeadLetters(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	filter, ok := s.deadLetterFilter(w, r)
	if !ok {
		return
	}
	letters, err := s.store.ListDeadLetters(r.Context(), filter)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dead_letters": letters})
}

// replayAdminDeadLetter replays any dead letter regardless of squad —
// platform_admin only.
func (s *Server) replayAdminDeadLetter(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	msg, err := s.store.GetMessage(r.Context(), chi.URLParam(r, "messageID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	s.replayDeadLetter(w, r, msg, msg.ToAgentID)
}

// pruneAdminDeadLetter hard-deletes a dead row. Admin-only, dead-only:
// live messages must not vanish through the prune door.
func (s *Server) pruneAdminDeadLetter(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	msg, err := s.store.GetMessage(r.Context(), chi.URLParam(r, "messageID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if msg.Status != domain.MessageDead {
		writeError(w, http.StatusConflict, "not_dead", "only dead messages can be pruned")
		return
	}
	metadata, _ := json.Marshal(map[string]any{
		"message_id":      msg.ID,
		"agent_id":        msg.ToAgentID,
		"message_type":    string(msg.Type),
		"from_type":       msg.FromType,
		"terminal_reason": msg.TerminalReason,
	})
	if err := s.recordUserAuditRequired(r, auditDeadLetterPruned, "message", msg.ID, msg.SquadID, metadata); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to audit dead-letter prune")
		return
	}
	if err := s.store.DeleteMessage(r.Context(), msg.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// replayDeadLetter is the shared replay path for owner and admin replays:
// store transition, mandatory audit, and target wake-up sync.
func (s *Server) replayDeadLetter(w http.ResponseWriter, r *http.Request, msg *domain.Message, agentID string) {
	replayed, err := s.store.ReplayDeadMessage(r.Context(), msg.ID)
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			writeError(w, http.StatusConflict, "not_dead", "only dead messages can be replayed")
			return
		}
		writeStorageError(w, err)
		return
	}
	metadata, _ := json.Marshal(map[string]any{
		"message_id":   msg.ID,
		"agent_id":     agentID,
		"message_type": string(msg.Type),
		"from_type":    msg.FromType,
		"prior_reason": msg.TerminalReason,
		"replayed_at":  time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.recordUserAuditRequired(r, auditDeadLetterReplayed, "message", msg.ID, msg.SquadID, metadata); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to audit dead-letter replay")
		return
	}
	if err := s.syncAgentStatusFromPendingWork(r.Context(), agentID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", msgUpdateTargetAgentState)
		return
	}
	writeJSON(w, http.StatusOK, replayed)
}

// deadLetterFilter parses the admin screen's query parameters, rejecting
// malformed UUIDs and timestamps with a 400 instead of letting Postgres
// surface a syntax error.
func (s *Server) deadLetterFilter(w http.ResponseWriter, r *http.Request) (domain.DeadLetterFilter, bool) {
	q := r.URL.Query()
	filter := domain.DeadLetterFilter{
		SquadID: q.Get("squad"),
		AgentID: q.Get("agent"),
		Type:    q.Get("type"),
		Reason:  q.Get("reason"),
		Limit:   boundedIntQuery(r, "limit", 100, 500),
	}
	if filter.SquadID != "" && !isUUID(filter.SquadID) {
		writeError(w, http.StatusBadRequest, "bad_request", "squad must be a UUID")
		return filter, false
	}
	if filter.AgentID != "" && !isUUID(filter.AgentID) {
		writeError(w, http.StatusBadRequest, "bad_request", "agent must be a UUID")
		return filter, false
	}
	if filter.Type != "" && !domain.MessageType(filter.Type).Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", msgTypeInvalid)
		return filter, false
	}
	if raw := q.Get("since"); raw != "" {
		since, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "since must be RFC3339")
			return filter, false
		}
		filter.Since = &since
	}
	if raw := q.Get("until"); raw != "" {
		until, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "until must be RFC3339")
			return filter, false
		}
		filter.Until = &until
	}
	return filter, true
}
