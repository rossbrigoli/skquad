package storage

// S-203 WP1: Postgres budget tests (gated on SKQUAD_TEST_DATABASE_URL
// like the rest of the Postgres suite). Exercises the real SQL: upsert,
// ensure-default from platform_settings, clamp-on-lower, clear.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func TestPostgresBudgetLifecycle(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	// Unset budget ⇒ ErrNotFound.
	_, err := store.GetUserBudget(ctx, f.user.ID)
	require.ErrorIs(t, err, ErrNotFound)

	// Set + read back.
	require.NoError(t, store.SetUserBudget(ctx, f.user.ID, 42.25, "tester"))
	b, err := store.GetUserBudget(ctx, f.user.ID)
	require.NoError(t, err)
	require.Equal(t, 42.25, b.MonthlyBudgetUSD)
	require.Equal(t, "tester", b.UpdatedBy)

	// Upsert overwrites.
	require.NoError(t, store.SetUserBudget(ctx, f.user.ID, 11.5, "tester2"))
	b, err = store.GetUserBudget(ctx, f.user.ID)
	require.NoError(t, err)
	require.Equal(t, 11.5, b.MonthlyBudgetUSD)
	require.Equal(t, "tester2", b.UpdatedBy)

	// EnsureUserDefaultBudget: no default set ⇒ no-op.
	inserted, err := store.EnsureUserDefaultBudget(ctx, f.user.ID)
	require.NoError(t, err)
	require.False(t, inserted, "existing row must not be touched")

	// Set a platform default; a fresh user gets it on ensure.
	require.NoError(t, store.SetPlatformSetting(ctx, domain.PlatformSettingBudgetDefaultUSD, "25.75", "admin"))
	user2, err := store.UpsertUser(ctx, &domain.User{
		OIDCIssuer: "https://issuer.skquad.test", OIDCSubject: "budget-user-2",
		Email: "budget2@example.test", EmailVerified: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user2.ID)
	})
	inserted, err = store.EnsureUserDefaultBudget(ctx, user2.ID)
	require.NoError(t, err)
	require.True(t, inserted)
	b, err = store.GetUserBudget(ctx, user2.ID)
	require.NoError(t, err)
	require.InDelta(t, 25.75, b.MonthlyBudgetUSD, 1e-9)
	require.Equal(t, "system:default", b.UpdatedBy)

	// Idempotent: second ensure does nothing.
	inserted, err = store.EnsureUserDefaultBudget(ctx, user2.ID)
	require.NoError(t, err)
	require.False(t, inserted)

	// Clamp: f.user 11.5 below, user2 25.75 above max 20.
	clamped, err := store.ClampUserBudgetsToMax(ctx, 20, "admin")
	require.NoError(t, err)
	require.GreaterOrEqual(t, clamped, 1)
	b, err = store.GetUserBudget(ctx, user2.ID)
	require.NoError(t, err)
	require.InDelta(t, 20.0, b.MonthlyBudgetUSD, 1e-9)
	b, err = store.GetUserBudget(ctx, f.user.ID)
	require.NoError(t, err)
	require.InDelta(t, 11.5, b.MonthlyBudgetUSD, 1e-9, "below-max untouched")

	// Platform budget view.
	pb, err := store.GetPlatformBudgets(ctx)
	require.NoError(t, err)
	require.NotNil(t, pb.DefaultMonthlyUSD)
	require.InDelta(t, 25.75, *pb.DefaultMonthlyUSD, 1e-9)
	require.Nil(t, pb.MaxUSD)

	// Set + clear platform max through the budget helper.
	require.NoError(t, store.SetPlatformBudgetSetting(ctx, domain.PlatformSettingBudgetMaxUSD, ptr(99.5), "admin"))
	pb, err = store.GetPlatformBudgets(ctx)
	require.NoError(t, err)
	require.NotNil(t, pb.MaxUSD)
	require.InDelta(t, 99.5, *pb.MaxUSD, 1e-9)
	require.NoError(t, store.SetPlatformBudgetSetting(ctx, domain.PlatformSettingBudgetMaxUSD, nil, "admin"))
	pb, err = store.GetPlatformBudgets(ctx)
	require.NoError(t, err)
	require.Nil(t, pb.MaxUSD)

	// Clear user budget.
	ok, err := store.ClearUserBudget(ctx, f.user.ID)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.ClearUserBudget(ctx, f.user.ID)
	require.NoError(t, err)
	require.False(t, ok)
}

func ptr[T any](v T) *T { return &v }

// TestPostgresUserLevelBudgetInboxAndNotification (S-203 WP4) is the
// live-bug regression: budget inbox messages and bell notifications are
// user-level (empty squad scope). The inbox INSERT RETURNING and the
// notification INSERT RETURNING must coalesce the NULL squad_id or the
// row scan fails with "cannot scan NULL into *string" — and the widened
// CHECK constraints (0040/0041) must admit the budget kinds/types.
func TestPostgresUserLevelBudgetInboxAndNotification(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	msg, err := store.CreateInboxMessage(ctx, &domain.InboxMessage{
		UserID:  f.user.ID,
		Kind:    domain.InboxBudgetWarning,
		Message: "Budget warning: 80% of your monthly budget used",
		Subject: "Budget warning: 80% of your monthly budget used",
		Body:    "You have used 80% of your monthly budget.",
	})
	require.NoError(t, err, "user-level budget inbox insert must work")
	require.Empty(t, msg.SquadID, "squad scope stays empty after coalesce scan")
	require.Equal(t, domain.InboxBudgetWarning, msg.Kind)

	notif, err := store.CreateNotification(ctx, &domain.Notification{
		UserID:   f.user.ID,
		Type:     domain.NotificationBudgetStopped,
		Severity: domain.NotificationError,
		Message:  "Monthly budget reached — your agents are stopped",
	})
	require.NoError(t, err, "user-level budget notification insert must work")
	require.Empty(t, notif.SquadID)
	require.Equal(t, domain.NotificationBudgetStopped, notif.Type)

	list, err := store.ListNotifications(ctx, f.user.ID, false, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Empty(t, list[0].SquadID)
}
