package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-209 retest regression at the HTTP layer: the bell's GET /notifications
// must NOT emit a read_at key for unread alerts. Previously time.Time +
// omitempty serialized "read_at":"0001-01-01T00:00:00Z", which the web
// client read as "read", zeroing the unread count and hiding the badge and
// "Mark all read". ReadAt is now *time.Time so nil is omitted.
func TestListNotificationsOmitsUnreadReadAt(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "s209b-notif"}, http.StatusCreated, &squad)
	var owner domain.User
	doJSON(t, handler, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &owner)

	ctx := context.Background()
	unread, err := store.CreateNotification(ctx, &domain.Notification{
		UserID:   owner.ID,
		SquadID:  squad.ID,
		Type:     domain.NotificationTaskFailed,
		Severity: domain.NotificationError,
		Message:  "task failed",
	})
	require.NoError(t, err)
	read, err := store.CreateNotification(ctx, &domain.Notification{
		UserID:   owner.ID,
		SquadID:  squad.ID,
		Type:     domain.NotificationTaskFailed,
		Severity: domain.NotificationError,
		Message:  "already acked",
	})
	require.NoError(t, err)
	_, err = store.MarkNotificationRead(ctx, owner.ID, read.ID)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var items []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 2)
	byID := map[string]map[string]any{}
	for _, it := range items {
		byID[it["id"].(string)] = it
	}

	_, unreadHasReadAt := byID[unread.ID]["read_at"]
	require.False(t, unreadHasReadAt, "unread notification must omit read_at; body=%s", rec.Body.String())

	readVal, readHasReadAt := byID[read.ID]["read_at"]
	require.True(t, readHasReadAt, "read notification must carry read_at; body=%s", rec.Body.String())
	require.NotEmpty(t, readVal, "read notification read_at must be a real timestamp")
}

// Same contract for the owner inbox listing.
func TestListInboxOmitsUnreadReadAt(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "s209b-inbox"}, http.StatusCreated, &squad)
	var owner domain.User
	doJSON(t, handler, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &owner)

	ctx := context.Background()
	unread, err := store.CreateInboxMessage(ctx, &domain.InboxMessage{
		UserID:  owner.ID,
		SquadID: squad.ID,
		Kind:    domain.InboxTaskCompleted,
		Message: "unread inbox",
	})
	require.NoError(t, err)
	read, err := store.CreateInboxMessage(ctx, &domain.InboxMessage{
		UserID:  owner.ID,
		SquadID: squad.ID,
		Kind:    domain.InboxTaskCompleted,
		Message: "read inbox",
	})
	require.NoError(t, err)
	_, err = store.MarkInboxMessageRead(ctx, owner.ID, read.ID)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/inbox", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var items []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 2)
	byID := map[string]map[string]any{}
	for _, it := range items {
		byID[it["id"].(string)] = it
	}

	_, unreadHasReadAt := byID[unread.ID]["read_at"]
	require.False(t, unreadHasReadAt, "unread inbox message must omit read_at; body=%s", rec.Body.String())

	readVal, readHasReadAt := byID[read.ID]["read_at"]
	require.True(t, readHasReadAt, "read inbox message must carry read_at; body=%s", rec.Body.String())
	require.NotEmpty(t, readVal)
}
