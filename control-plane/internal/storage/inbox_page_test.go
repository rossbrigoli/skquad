package storage

// S-239: inbox paging — ListInboxPage returns one newest-first page
// plus the total count of matching messages (paged UI needs both).

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func seedMemoryInbox(t *testing.T, store *MemoryStore, email string, count int) string {
	t.Helper()
	created, err := store.UpsertUser(context.Background(), &domain.User{Email: email})
	if err != nil {
		t.Fatal(err)
	}
	userID := created.ID
	for i := 0; i < count; i++ {
		_, err := store.CreateInboxMessage(context.Background(), &domain.InboxMessage{
			UserID:  userID,
			Kind:    domain.InboxAgentMessage,
			Message: fmt.Sprintf("message %02d", i),
		})
		require.NoError(t, err)
	}
	return userID
}

func TestMemoryStoreListInboxPagePagingAndTotal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	userID := seedMemoryInbox(t, store, "pager@x", 7)

	// Page 1 of 3: newest first.
	page, total, err := store.ListInboxPage(ctx, userID, false, 3, 0)
	require.NoError(t, err)
	require.Equal(t, 7, total)
	require.Len(t, page, 3)
	require.Equal(t, "message 06", page[0].Message)
	require.Equal(t, "message 04", page[2].Message)

	// Middle page.
	page, total, err = store.ListInboxPage(ctx, userID, false, 3, 3)
	require.NoError(t, err)
	require.Equal(t, 7, total)
	require.Equal(t, []string{"message 03", "message 02", "message 01"}, messagesOf(page))

	// Last partial page.
	page, total, err = store.ListInboxPage(ctx, userID, false, 3, 6)
	require.NoError(t, err)
	require.Equal(t, 7, total)
	require.Equal(t, []string{"message 00"}, messagesOf(page))

	// Offset past the end: empty page, total unchanged.
	page, total, err = store.ListInboxPage(ctx, userID, false, 3, 99)
	require.NoError(t, err)
	require.Equal(t, 7, total)
	require.Empty(t, page)
}

func TestMemoryStoreListInboxPageUnreadFilterAndIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	userID := seedMemoryInbox(t, store, "filter@x", 5)
	otherID := seedMemoryInbox(t, store, "other@x", 4)

	// Mark the two newest read → unread total drops to 3.
	all, err := store.ListInboxMessages(ctx, userID, false, 10)
	require.NoError(t, err)
	for _, m := range all[:2] {
		_, err := store.MarkInboxMessageRead(ctx, userID, m.ID)
		require.NoError(t, err)
	}

	page, total, err := store.ListInboxPage(ctx, userID, true, 2, 0)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Len(t, page, 2)
	for _, m := range page {
		require.Nil(t, m.ReadAt)
	}

	// Other user's inbox is untouched by the first user's paging.
	_, total, err = store.ListInboxPage(ctx, otherID, false, 2, 0)
	require.NoError(t, err)
	require.Equal(t, 4, total)
}

func messagesOf(msgs []*domain.InboxMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Message)
	}
	return out
}
