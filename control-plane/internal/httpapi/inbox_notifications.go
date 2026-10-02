package httpapi

// S-193 epic: inbox & notifications.
//
// Two human-facing surfaces with deliberately separate concerns:
//
//   - Inbox (inbox_messages): email-like messages addressed to a human.
//     Never auto-removed; only explicit user delete. Agents deliver here
//     via the send_inbox builtin (kind=agent_message) on top of the
//     system-emitted task_completed / action_required kinds.
//   - Notifications (notifications): transient "something went wrong"
//     alerts for the top-bar bell — task failed, task stuck, agent died,
//     task blocked needing user input. An inbox message never creates a
//     notification and vice-versa.
//
// Auth model:
//   - GET    /inbox                       own inbox; ?user_id= ⇒ platform_admin only
//   - DELETE /inbox/{id}                  recipient or platform_admin
//   - GET    /notifications             own alerts; ?user_id= ⇒ platform_admin only
//   - POST   /notifications/{id}/read   recipient only
//   - POST   /notifications/read-all    recipient only
//   - POST   /agents/me/inbox           agent-cred; routes to squad.OwnerID

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// formatTaskRef renders the human-facing task reference ("T-1234").
// TaskNumber is immutable per squad (S-184); the short-id fallback only
// guards against zero-value structs in test fixtures.
func formatTaskRef(task *domain.Task) string {
	if task == nil {
		return "task"
	}
	if task.TaskNumber > 0 {
		return fmt.Sprintf("T-%d", task.TaskNumber)
	}
	if len(task.ID) >= 8 {
		return "T-" + task.ID[:8]
	}
	return "T-" + task.ID
}

// inboxRecipient resolves the target user for a listing request that may
// carry the admin ?user_id= filter. Returns (userID, ok). When the param
// is absent the caller's own id is used; when present, platform_admin is
// mandatory (403 written otherwise).
func (s *Server) inboxRecipient(w http.ResponseWriter, r *http.Request) (string, bool) {
	u := currentUser(r.Context())
	target := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if target == "" {
		return u.ID, true
	}
	if !s.requirePlatformAdmin(w, r) {
		return "", false
	}
	return target, true
}

// listInbox serves the email-like inbox listing. With ?user_id= a
// platform admin views another user's inbox (the admin Filter control);
// the response shape is unchanged (bare array) for compatibility.
func (s *Server) listInbox(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.inboxRecipient(w, r)
	if !ok {
		return
	}
	unreadOnly := r.URL.Query().Get("unread") == "true"
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	messages, err := s.store.ListInboxMessages(r.Context(), userID, unreadOnly, limit)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

// deleteInboxMessage implements the only removal path an inbox message
// ever has: an explicit delete by its recipient (or a platform admin
// viewing via the filter). There is no auto-prune.
func (s *Server) deleteInboxMessage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	id := chi.URLParam(r, "messageID")
	msg, err := s.store.GetInboxMessage(r.Context(), id)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	scope := u.ID
	if msg.UserID != u.ID {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
		scope = "" // admin delete: unrestricted
	}
	// S-198: inbox deletes used to be a silent 204. Record who deleted
	// which message before removing it; if the audit cannot be written,
	// the delete fails too (same required-audit pattern as dead-letter
	// prune). The 204 response semantics are unchanged.
	metadata, _ := json.Marshal(map[string]any{
		"message_kind":      string(msg.Kind),
		"recipient_user_id": msg.UserID,
		"from_agent_id":     msg.FromAgentID,
		"task_id":           msg.TaskID,
	})
	if err := s.recordUserAuditRequired(r, auditInboxDeleted, "inbox_message", msg.ID, msg.SquadID, metadata); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to audit inbox delete")
		return
	}
	if err := s.store.DeleteInboxMessage(r.Context(), id, scope); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listNotifications serves the bell dropdown listing.
func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.inboxRecipient(w, r)
	if !ok {
		return
	}
	unreadOnly := r.URL.Query().Get("unread") == "true"
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	items, err := s.store.ListNotifications(r.Context(), userID, unreadOnly, limit)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) markNotificationRead(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	updated, err := s.store.MarkNotificationRead(r.Context(), u.ID, chi.URLParam(r, "notificationID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) markAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	count, err := s.store.MarkAllNotificationsRead(r.Context(), u.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"marked_read": count})
}

// sendInboxFromAgent is the send_inbox builtin's endpoint: an agent
// delivers human-requested content to its squad owner's inbox. The
// agent→human addressing is squad-based: agents never name a user — the
// control plane routes to squad.OwnerID. task_id is optional and must
// belong to the agent's squad so the UI can render an internal link.
func (s *Server) sendInboxFromAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
		Subject string `json:"subject"`
		TaskID  string `json:"task_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	message := trimRunes(strings.TrimSpace(req.Message), maxInboxMessageChars)
	if message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	principal := currentAgent(r.Context())
	squad, err := s.store.GetSquad(r.Context(), principal.Agent.SquadID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if squad.OwnerID == "" {
		writeError(w, http.StatusNotFound, "not_found", "squad owner not found for notification")
		return
	}
	taskID := strings.TrimSpace(req.TaskID)
	if taskID != "" {
		task, err := s.store.GetTask(r.Context(), taskID)
		if err != nil || task.SquadID != principal.Agent.SquadID {
			writeError(w, http.StatusNotFound, "not_found", "task not found in your squad")
			return
		}
	}
	subject := trimRunes(strings.TrimSpace(req.Subject), 200)
	created, err := s.store.CreateInboxMessage(s.pendingAgentAuditCtx(r, principal.Agent.ID, "inbox.send_inbox", "inbox_message", "", principal.Agent.SquadID, nil), &domain.InboxMessage{
		SquadID:     principal.Agent.SquadID,
		UserID:      squad.OwnerID,
		FromAgentID: principal.Agent.ID,
		TaskID:      taskID,
		Kind:        domain.InboxAgentMessage,
		Message:     message,
		Subject:     subject,
		Body:        message,
	})
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "squad owner not found for notification")
		return
	}
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// getNotificationPreferences serves the S-199 per-user mute list.
// Response shape is stable: {"muted_types": [...]} — an empty list
// means every notification type is enabled (the default).
func (s *Server) getNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	prefs, err := s.store.GetNotificationPreferences(r.Context(), u.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// putNotificationPreferences replaces the caller's mute list. Every
// entry must be a known notification type (400 otherwise); duplicates
// are collapsed. Only human (OIDC) users reach this route — it lives in
// the authenticate group, same as the rest of /notifications.
func (s *Server) putNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MutedTypes []string `json:"muted_types"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	seen := map[domain.NotificationType]bool{}
	muted := make([]domain.NotificationType, 0, len(req.MutedTypes))
	for raw := range req.MutedTypes {
		t := domain.NotificationType(strings.TrimSpace(req.MutedTypes[raw]))
		if !domain.IsKnownNotificationType(t) {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("unknown notification type %q (known: task_failed, task_stuck, agent_died, task_blocked)", string(t)))
			return
		}
		if !seen[t] {
			seen[t] = true
			muted = append(muted, t)
		}
	}
	u := currentUser(r.Context())
	prefs, err := s.store.SetNotificationPreferences(r.Context(), u.ID, muted)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// notificationMuted reports whether the recipient has muted this
// notification type (S-199). Fail-open: a preferences lookup error
// delivers the alert rather than silently dropping it — muting is a
// user convenience, never an alert-suppression failure mode.
func notificationMuted(ctx context.Context, store Store, userID string, kind domain.NotificationType) bool {
	prefs, err := store.GetNotificationPreferences(ctx, userID)
	if err != nil {
		slog.Warn("notification preferences lookup failed; delivering anyway",
			"error", err, "type", string(kind), "user", userID)
		return false
	}
	return prefs.IsMuted(kind)
}

// emitNotification resolves the squad owner and files one notification.
// Best-effort: every failure path is silent (or logged) so the underlying
// task flow never breaks on alerting. S-199: the recipient's mute list
// is checked first — a muted type is skipped (logged, no error).
func (s *Server) emitNotification(ctx context.Context, squadID string, taskID string, agentID string, kind domain.NotificationType, severity domain.NotificationSeverity, message string) {
	squad, err := s.store.GetSquad(ctx, squadID)
	if err != nil || squad.OwnerID == "" {
		return
	}
	if notificationMuted(ctx, s.store, squad.OwnerID, kind) {
		slog.Info("notification skipped: type muted by user preference",
			"type", string(kind), "user", squad.OwnerID, "squad", squadID)
		return
	}
	if _, err := s.store.CreateNotification(ctx, &domain.Notification{
		UserID:   squad.OwnerID,
		SquadID:  squadID,
		TaskID:   taskID,
		AgentID:  agentID,
		Type:     kind,
		Severity: severity,
		Message:  trimRunes(strings.TrimSpace(message), maxInboxMessageChars),
	}); err != nil && !errors.Is(err, storage.ErrNotFound) {
		slog.Warn("emit notification", "error", err, "type", string(kind), "squad", squadID)
	}
}

// notifyTaskFailedOnAgentError (S-193) fires the task_failed bell alert
// when a heartbeat transitions an agent into error while it holds an
// in-progress task. Transition-gated so a sticky-error agent beating
// every 30s cannot spam the owner.
func (s *Server) notifyTaskFailedOnAgentError(ctx context.Context, agent *domain.Agent, previousStatus domain.AgentStatus) {
	if previousStatus == domain.AgentError {
		return
	}
	tasks, err := s.store.ListAgentTasks(ctx, agent.ID)
	if err != nil {
		return
	}
	var inProgress *domain.Task
	for _, t := range tasks {
		if t.Status == domain.TaskInProgress {
			inProgress = t
			break
		}
	}
	if inProgress == nil {
		return
	}
	s.emitNotification(ctx, inProgress.SquadID, inProgress.ID, agent.ID,
		domain.NotificationTaskFailed, domain.NotificationError,
		fmt.Sprintf("Task %s failed: agent %s reported an error.", formatTaskRef(inProgress), agent.Name))
}
