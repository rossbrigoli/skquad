package storage

// S-198: Postgres coverage for the retention store surface. Same
// semantics as the MemoryStore tests (retention_test.go): old READ
// notifications purged, unread and young survive; audit purge scoped to
// the sweep's own action. Skipped without a test DB, per the Postgres
// test convention. No t.Parallel(): global sweeps.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func backdateNotification(t *testing.T, store *PostgresStore, id string, age time.Duration) {
	t.Helper()
	_, err := store.pool.Exec(context.Background(),
		`UPDATE notifications SET created_at = now() - $1::interval WHERE id = $2::uuid`,
		age.String(), id)
	require.NoError(t, err)
}

func TestPostgresDeleteReadNotificationsBefore(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	mk := func(msg string) *domain.Notification {
		n, err := store.CreateNotification(ctx, &domain.Notification{
			UserID:   f.user.ID,
			SquadID:  f.squad.ID,
			Type:     domain.NotificationTaskFailed,
			Severity: domain.NotificationError,
			Message:  msg,
		})
		require.NoError(t, err)
		return n
	}

	oldRead := mk("old read")
	_, err := store.MarkNotificationRead(ctx, f.user.ID, oldRead.ID)
	require.NoError(t, err)
	backdateNotification(t, store, oldRead.ID, 95*24*time.Hour)

	youngRead := mk("young read")
	_, err = store.MarkNotificationRead(ctx, f.user.ID, youngRead.ID)
	require.NoError(t, err)

	oldUnread := mk("old unread")
	backdateNotification(t, store, oldUnread.ID, 95*24*time.Hour)

	purged, err := store.DeleteReadNotificationsBefore(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, 1, purged)

	list, err := store.ListNotifications(ctx, f.user.ID, false, 100)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range list {
		ids[n.ID] = true
	}
	require.False(t, ids[oldRead.ID], "old read notification must be purged")
	require.True(t, ids[youngRead.ID], "young read notification must survive")
	require.True(t, ids[oldUnread.ID], "unread notifications are never purged, however old (S-193)")
}

func TestPostgresDeleteAuditByActionBefore(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	oldDel := &domain.AuditEntry{ActorType: "user", ActorID: f.user.ID, Action: "inbox.deleted",
		ResourceType: "inbox_message", ResourceID: uuid.NewString(), SquadID: f.squad.ID, Timestamp: cutoff.Add(-5 * 24 * time.Hour)}
	freshDel := &domain.AuditEntry{ActorType: "user", ActorID: f.user.ID, Action: "inbox.deleted",
		ResourceType: "inbox_message", ResourceID: uuid.NewString(), SquadID: f.squad.ID, Timestamp: time.Now().UTC()}
	oldOther := &domain.AuditEntry{ActorType: "user", ActorID: f.user.ID, Action: "builtin_tools.update",
		ResourceType: "builtin_tool", SquadID: f.squad.ID, Timestamp: cutoff.Add(-5 * 24 * time.Hour)}
	for _, e := range []*domain.AuditEntry{oldDel, freshDel, oldOther} {
		require.NoError(t, store.RecordAudit(ctx, e))
	}

	purged, err := store.DeleteAuditByActionBefore(ctx, "inbox.deleted", cutoff)
	require.NoError(t, err)
	require.Equal(t, 1, purged)

	entries, err := store.ListAudit(ctx, f.squad.ID, 100)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.ElementsMatch(t,
		[]string{"inbox.deleted", "builtin_tools.update"},
		actionNames(entries),
		"fresh inbox.deleted and the unrelated old action must survive")
	for _, e := range entries {
		if e.Action == "inbox.deleted" {
			require.Equal(t, freshDel.ResourceID, e.ResourceID, "only the fresh inbox.deleted row survives")
		}
	}
}

func actionNames(entries []*domain.AuditEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Action)
	}
	return out
}
