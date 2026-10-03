package storage

// S-198: MemoryStore coverage for the retention store surface —
// DeleteReadNotificationsBefore (old read purged; unread and young read
// survive) and DeleteAuditByActionBefore (only the sweep-owned action is
// purged). Postgres parity lives in postgres_retention_test.go.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func retentionFixture(t *testing.T, store *MemoryStore) (*domain.User, *domain.Squad) {
	t.Helper()
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	user, err := store.UpsertUser(ctx, &domain.User{
		OIDCIssuer:    "https://issuer.skquad.test",
		OIDCSubject:   "retention-" + tag,
		Email:         "retention-" + tag + "@example.test",
		EmailVerified: true,
		Name:          "Retention User",
	})
	require.NoError(t, err)
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:    "retention-" + tag,
		OwnerID: user.ID,
		Status:  domain.SquadActive,
	})
	require.NoError(t, err)
	return user, squad
}

func TestMemoryDeleteReadNotificationsBefore(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	user, squad := retentionFixture(t, store)

	mk := func(msg string) *domain.Notification {
		n, err := store.CreateNotification(ctx, &domain.Notification{
			UserID:   user.ID,
			SquadID:  squad.ID,
			Type:     domain.NotificationTaskFailed,
			Severity: domain.NotificationError,
			Message:  msg,
		})
		require.NoError(t, err)
		return n
	}

	oldRead := mk("old read")
	oldReadAt := time.Now().UTC().Add(-95 * 24 * time.Hour)
	store.notifications[oldRead.ID].ReadAt = &oldReadAt
	store.notifications[oldRead.ID].CreatedAt = time.Now().UTC().Add(-95 * 24 * time.Hour)

	youngRead := mk("young read")
	_, err := store.MarkNotificationRead(ctx, user.ID, youngRead.ID)
	require.NoError(t, err)

	oldUnread := mk("old unread")
	store.notifications[oldUnread.ID].CreatedAt = time.Now().UTC().Add(-95 * 24 * time.Hour)

	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
	purged, err := store.DeleteReadNotificationsBefore(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, 1, purged)

	list, err := store.ListNotifications(ctx, user.ID, false, 100)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range list {
		ids[n.ID] = true
	}
	require.False(t, ids[oldRead.ID], "old read notification must be purged")
	require.True(t, ids[youngRead.ID], "young read notification must survive")
	require.True(t, ids[oldUnread.ID], "unread notifications are never purged, however old (S-193)")

	// Idempotent: a second sweep purges nothing.
	purged, err = store.DeleteReadNotificationsBefore(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, 0, purged)
}

func TestMemoryDeleteAuditByActionBefore(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	user, squad := retentionFixture(t, store)
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	oldDel := &domain.AuditEntry{ActorType: "user", ActorID: user.ID, Action: "inbox.deleted",
		ResourceType: "inbox_message", ResourceID: "old-del-res", SquadID: squad.ID, Timestamp: cutoff.Add(-5 * 24 * time.Hour)}
	freshDel := &domain.AuditEntry{ActorType: "user", ActorID: user.ID, Action: "inbox.deleted",
		ResourceType: "inbox_message", ResourceID: "fresh-del-res", SquadID: squad.ID, Timestamp: time.Now().UTC()}
	oldOther := &domain.AuditEntry{ActorType: "user", ActorID: user.ID, Action: "builtin_tools.update",
		ResourceType: "builtin_tool", SquadID: squad.ID, Timestamp: cutoff.Add(-5 * 24 * time.Hour)}
	for _, e := range []*domain.AuditEntry{oldDel, freshDel, oldOther} {
		require.NoError(t, store.RecordAudit(ctx, e))
	}

	purged, err := store.DeleteAuditByActionBefore(ctx, "inbox.deleted", cutoff)
	require.NoError(t, err)
	require.Equal(t, 1, purged)

	entries, err := store.ListAudit(ctx, "", 100)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.ElementsMatch(t, []string{"inbox.deleted", "builtin_tools.update"}, auditActionNames(entries))
	for _, e := range entries {
		if e.Action == "inbox.deleted" {
			require.Equal(t, "fresh-del-res", e.ResourceID, "only the fresh inbox.deleted row survives")
		}
	}
	// Unrelated actions survive regardless of age.
	var keptOther bool
	for _, e := range entries {
		if e.Action == "builtin_tools.update" {
			keptOther = true
		}
	}
	require.True(t, keptOther, "unrelated audit actions must never be purged by the sweep")
}

func auditActionNames(entries []*domain.AuditEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Action)
	}
	return out
}
