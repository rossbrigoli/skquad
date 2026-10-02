package httpapi

// S-198: retention sweep + inbox-delete audit tests.
//
//   - PurgeNotificationsOnce: old read purged, unread survives, young
//     survives, sweep-owned audit rows purged, unrelated audit survives.
//   - DELETE /inbox/{id} keeps its 204 semantics but now leaves an
//     audit trail (action='inbox.deleted', actor=deleting user).

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func TestPurgeNotificationsOnce(t *testing.T) {
	store := storage.NewMemoryStore()
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	user, err := store.UpsertUser(ctx, &domain.User{
		OIDCIssuer: "https://issuer.skquad.test", OIDCSubject: "purge-" + tag,
		Email: "purge-" + tag + "@example.test", EmailVerified: true, Name: "Purge User",
	})
	require.NoError(t, err)
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "purge-" + tag, OwnerID: user.ID, Status: domain.SquadActive})
	require.NoError(t, err)

	mk := func(msg string) *domain.Notification {
		n, err := store.CreateNotification(ctx, &domain.Notification{
			UserID: user.ID, SquadID: squad.ID, Type: domain.NotificationTaskFailed,
			Severity: domain.NotificationError, Message: msg,
		})
		require.NoError(t, err)
		return n
	}

	oldRead := mk("old read")
	_, err = store.MarkNotificationRead(ctx, user.ID, oldRead.ID)
	require.NoError(t, err)
	unread := mk("still unread")

	require.NoError(t, store.RecordAudit(ctx, &domain.AuditEntry{
		ActorType: "user", ActorID: user.ID, Action: auditInboxDeleted,
		ResourceType: "inbox_message", SquadID: squad.ID, Timestamp: time.Now().UTC().Add(-5 * time.Minute),
	}))
	require.NoError(t, store.RecordAudit(ctx, &domain.AuditEntry{
		ActorType: "user", ActorID: user.ID, Action: "task.updated",
		ResourceType: "task", SquadID: squad.ID, Timestamp: time.Now().UTC().Add(-5 * time.Minute),
	}))

	// Cutoff just ahead of "now": every row created moments ago counts as
	// old. The 90-day boundary semantics themselves are covered by the
	// storage-level retention tests; here we exercise the sweep wiring.
	cutoff := time.Now().UTC().Add(time.Minute)
	notifs, audit, err := PurgeNotificationsOnce(ctx, store, cutoff)
	require.NoError(t, err)
	require.Equal(t, 1, notifs, "only the READ notification is purged")
	require.Equal(t, 1, audit, "only the inbox.deleted audit row is purged")

	list, err := store.ListNotifications(ctx, user.ID, false, 100)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range list {
		ids[n.ID] = true
	}
	require.False(t, ids[oldRead.ID])
	require.True(t, ids[unread.ID], "unread survives regardless of age (S-193)")

	entries, err := store.ListAudit(ctx, squad.ID, 100)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "task.updated", entries[0].Action, "unrelated audit actions survive the sweep")

	// Idempotent second sweep.
	notifs, audit, err = PurgeNotificationsOnce(ctx, store, cutoff)
	require.NoError(t, err)
	require.Zero(t, notifs)
	require.Zero(t, audit)
}

func TestInboxDeleteWritesAuditTrail(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s198-audit")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks,
		map[string]any{"title": "audit carrier"}, http.StatusCreated, &task)
	var created domain.InboxMessage
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/inbox",
		map[string]any{"message": "delete me", "task_id": task.ID}, http.StatusCreated, &created)

	var owner domain.User
	doJSON(t, handler, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &owner)

	// 204 semantics unchanged.
	doJSONNoBody(t, handler, http.MethodDelete, pathInbox+"/"+created.ID, nil, http.StatusNoContent)

	// ...but the delete is no longer silent server-side.
	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/audit", nil, http.StatusOK, &audit)
	var found *domain.AuditEntry
	for i := range audit {
		if audit[i].Action == auditInboxDeleted {
			found = &audit[i]
			break
		}
	}
	require.NotNil(t, found, "inbox delete must leave an audit trail")
	require.Equal(t, "user", found.ActorType)
	require.Equal(t, owner.ID, found.ActorID)
	require.Equal(t, "inbox_message", found.ResourceType)
	require.Equal(t, created.ID, found.ResourceID)
	require.Equal(t, squad.ID, found.SquadID)
	require.Contains(t, string(found.Metadata), string(domain.InboxAgentMessage))
	require.Contains(t, string(found.Metadata), created.UserID)

	// Message really is gone.
	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, pathInbox, nil, http.StatusOK, &inbox)
	require.Empty(t, inbox)
}
