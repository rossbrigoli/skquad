package httpapi

// S-235: actor display-name resolution for the task page thread and the
// status timeline. Names are resolved SERVER-SIDE at read time so the web
// layer renders a plain `from_display` / `actor_display` string instead of
// pasting GUIDs. Read-time resolution means an agent renamed after an event
// shows its CURRENT name, and per-event actors stay correct across task
// reassignments (the actor id on each row is authoritative, not the
// task's current assignee).

import (
	"context"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// actorRef is one (kind, id) actor reference collected from result rows.
type actorRef struct {
	kind string // "user" | "agent"
	id   string
}

// actorDisplayName picks the display string for a resolved actor.
// Agents: their name verbatim. Users: the FIRST token of the profile name
// (OIDC display name), falling back to the email local-part when the
// profile name is empty. Anything else: "" (callers keep their fallback).
func actorDisplayName(kind, name, email string) string {
	switch kind {
	case "agent":
		return strings.TrimSpace(name)
	case "user":
		if fields := strings.Fields(name); len(fields) > 0 {
			return fields[0]
		}
		if email != "" {
			if local := strings.SplitN(email, "@", 2)[0]; local != "" {
				return local
			}
		}
	}
	return ""
}

// resolveActorDisplays batch-resolves actor references to display names.
// Unresolvable ids (deleted users/agents, system actors) are simply absent
// from the map; callers fall back to whatever they rendered before.
func (s *Server) resolveActorDisplays(ctx context.Context, refs []actorRef) map[string]string {
	display := make(map[string]string, len(refs))
	for _, ref := range refs {
		key := ref.kind + ":" + ref.id
		if ref.kind != "user" && ref.kind != "agent" || ref.id == "" {
			continue
		}
		if _, done := display[key]; done {
			continue
		}
		switch ref.kind {
		case "agent":
			agent, err := s.store.GetAgent(ctx, ref.id)
			if err != nil || agent == nil {
				display[key] = ""
				continue
			}
			display[key] = actorDisplayName("agent", agent.Name, "")
		case "user":
			user, err := s.store.GetUser(ctx, ref.id)
			if err != nil || user == nil {
				display[key] = ""
				continue
			}
			display[key] = actorDisplayName("user", user.Name, user.Email)
		}
	}
	return display
}

// actorDisplay looks a previously resolved display name up; "" when absent.
func actorDisplay(display map[string]string, kind, id string) string {
	return display[kind+":"+id]
}

// decorateMessages fills FromDisplay on messages from the resolved map.
func decorateMessages(messages []*domain.Message, display map[string]string) {
	for _, msg := range messages {
		msg.FromDisplay = actorDisplay(display, msg.FromType, msg.FromID)
	}
}

// decorateAuditEntries fills ActorDisplay on audit entries from the
// resolved map.
func decorateAuditEntries(entries []*domain.AuditEntry, display map[string]string) {
	for _, entry := range entries {
		entry.ActorDisplay = actorDisplay(display, entry.ActorType, entry.ActorID)
	}
}
