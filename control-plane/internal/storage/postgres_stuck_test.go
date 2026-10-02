package storage

// S-197: Postgres coverage for the stuck-scanner store surface —
// ListStaleInProgressTasks (task row + thread activity + heartbeat) and
// HasRecentNotificationForTask (dedupe). Skipped without a test DB, like
// the rest of the Postgres store tests. No t.Parallel(): the sweeps are
// global, matching the parity-test convention.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func backdateTask(t *testing.T, store *PostgresStore, taskID string, age time.Duration) {
	t.Helper()
	_, err := store.pool.Exec(context.Background(),
		`UPDATE tasks SET updated_at = now() - $1::interval WHERE id = $2::uuid`,
		age.String(), taskID)
	require.NoError(t, err)
}

func TestPostgresListStaleInProgressTasks(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	// Stale: in-progress, row older than cutoff, no thread/heartbeat.
	stale, err := store.CreateTask(ctx, &domain.Task{
		BoardID: f.board.ID, SquadID: f.squad.ID, Title: "stale",
		Status: domain.TaskInProgress, AssigneeAgentID: f.agent.ID,
		CreatedByType: "user", CreatedByID: f.user.ID,
	})
	require.NoError(t, err)
	backdateTask(t, store, stale.ID, 48*time.Hour)

	// Fresh in-progress: must not appear.
	fresh, err := store.CreateTask(ctx, &domain.Task{
		BoardID: f.board.ID, SquadID: f.squad.ID, Title: "fresh",
		Status: domain.TaskInProgress, AssigneeAgentID: f.agent.ID,
		CreatedByType: "user", CreatedByID: f.user.ID,
	})
	require.NoError(t, err)

	// Non in-progress and stale: must not appear.
	todo, err := store.CreateTask(ctx, &domain.Task{
		BoardID: f.board.ID, SquadID: f.squad.ID, Title: "todo",
		Status: domain.TaskTodo, CreatedByType: "user", CreatedByID: f.user.ID,
	})
	require.NoError(t, err)
	backdateTask(t, store, todo.ID, 48*time.Hour)

	tasks, err := store.ListStaleInProgressTasks(ctx, cutoff)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, task := range tasks {
		ids[task.ID] = true
	}
	require.True(t, ids[stale.ID], "stale in-progress task must be listed")
	require.False(t, ids[fresh.ID], "fresh task must not be listed")
	require.False(t, ids[todo.ID], "non in-progress task must not be listed")

	// Thread activity (message payload carries task_id) suppresses it.
	_, err = store.pool.Exec(ctx, `
		INSERT INTO messages (from_type, from_id, to_agent_id, squad_id, type, payload, status)
		VALUES ('agent', $1::uuid, $2::uuid, $3::uuid, 'ping',
		        jsonb_build_object('message', 'still working', 'task_id', $4::text), 'delivered')
	`, f.agent.ID, f.agent.ID, f.squad.ID, stale.ID)
	require.NoError(t, err)

	tasks, err = store.ListStaleInProgressTasks(ctx, cutoff)
	require.NoError(t, err)
	for _, task := range tasks {
		require.NotEqual(t, stale.ID, task.ID, "recent thread activity must suppress the task")
	}

	// A fresh execution heartbeat suppresses it too (remove the message first).
	_, err = store.pool.Exec(ctx, `DELETE FROM messages WHERE squad_id = $1::uuid`, f.squad.ID)
	require.NoError(t, err)
	_, err = store.pool.Exec(ctx, `
		INSERT INTO task_executions (task_id, agent_id, worker_id, status, lease_expires_at)
		VALUES ($1::uuid, $2::uuid, 'w1', 'active', now() + interval '1 hour')
	`, stale.ID, f.agent.ID)
	require.NoError(t, err)

	tasks, err = store.ListStaleInProgressTasks(ctx, cutoff)
	require.NoError(t, err)
	for _, task := range tasks {
		require.NotEqual(t, stale.ID, task.ID, "recent heartbeat must suppress the task")
	}
}

func TestPostgresHasRecentNotificationForTask(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	stale, err := store.CreateTask(ctx, &domain.Task{
		BoardID: f.board.ID, SquadID: f.squad.ID, Title: "dedupe me",
		Status: domain.TaskInProgress, AssigneeAgentID: f.agent.ID,
		CreatedByType: "user", CreatedByID: f.user.ID,
	})
	require.NoError(t, err)

	// No prior notification ⇒ false.
	recent, err := store.HasRecentNotificationForTask(ctx, stale.ID, domain.NotificationTaskStuck, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.False(t, recent)

	// Fresh task_stuck row ⇒ true within the window, false outside it.
	_, err = store.CreateNotification(ctx, &domain.Notification{
		UserID: f.user.ID, SquadID: f.squad.ID, TaskID: stale.ID,
		Type: domain.NotificationTaskStuck, Severity: domain.NotificationWarning, Message: "stuck",
	})
	require.NoError(t, err)

	recent, err = store.HasRecentNotificationForTask(ctx, stale.ID, domain.NotificationTaskStuck, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, recent)

	recent, err = store.HasRecentNotificationForTask(ctx, stale.ID, domain.NotificationTaskStuck, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.False(t, recent, "window starting after the row must not match")

	// Other types never dedupe task_stuck.
	recent, err = store.HasRecentNotificationForTask(ctx, stale.ID, domain.NotificationTaskFailed, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.False(t, recent)

	// Empty task id is a cheap false.
	recent, err = store.HasRecentNotificationForTask(ctx, "", domain.NotificationTaskStuck, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.False(t, recent)
}
