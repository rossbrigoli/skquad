package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const (
	auditSquadCreate = "squad.create"
	auditTaskCreate  = "task.create"
)

// S-86: significant mutations must record their audit entry in the same
// transaction — a committed change always has its audit record, and an
// audit-write failure rolls the mutation back.

func TestMemoryPendingAuditCommittedWithMutation(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	entry := NewAuditEntry("user", "u1", auditSquadCreate, "squad", "", "", nil)
	created, err := store.CreateSquad(WithPendingAudit(ctx, entry), &domain.Squad{
		Name:    "audited-squad",
		OwnerID: "u1",
	})
	require.NoError(t, err)

	entries, err := store.ListAudit(ctx, created.ID, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, auditSquadCreate, entries[0].Action)
	// Fill-in: the store knew the new squad id even though the handler did not.
	require.Equal(t, created.ID, entries[0].ResourceID)
	require.Equal(t, created.ID, entries[0].SquadID)
}

func TestMemoryPendingAuditDrainedOnce(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	entry := NewAuditEntry("user", "u1", auditSquadCreate, "squad", "", "", nil)
	ctx = WithPendingAudit(ctx, entry)

	first, err := store.CreateSquad(ctx, &domain.Squad{Name: "squad-a", OwnerID: "u1"})
	require.NoError(t, err)
	// A second mutation on the same ctx must NOT re-record the drained entry.
	second, err := store.CreateSquad(ctx, &domain.Squad{Name: "squad-b", OwnerID: "u1"})
	require.NoError(t, err)

	firstEntries, err := store.ListAudit(ctx, first.ID, 10)
	require.NoError(t, err)
	require.Len(t, firstEntries, 1)

	secondEntries, err := store.ListAudit(ctx, second.ID, 10)
	require.NoError(t, err)
	require.Empty(t, secondEntries)
}

func TestPostgresPendingAuditCommittedWithMutation(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()

	f := newPGFixture(t, store)

	entry := NewAuditEntry("user", f.user.ID, auditTaskCreate, "task", "", f.squad.ID, nil)
	task, err := store.CreateTask(WithPendingAudit(ctx, entry), &domain.Task{
		BoardID:       f.board.ID,
		SquadID:       f.squad.ID,
		Title:         "audited task",
		CreatedByType: "user",
		CreatedByID:   f.user.ID,
	})
	require.NoError(t, err)

	entries, err := store.ListAudit(ctx, f.squad.ID, 50)
	require.NoError(t, err)
	var found bool
	for _, e := range entries {
		if e.Action == auditTaskCreate && e.ResourceID == task.ID {
			found = true
		}
	}
	require.True(t, found, "expected task.create audit entry for the new task")
}

// TestPostgresAuditFailureRollsBackMutation injects an audit-write failure
// (non-uuid resource id violates the audit_log cast) and asserts the state
// mutation is rolled back with it.
func TestPostgresAuditFailureRollsBackMutation(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()

	f := newPGFixture(t, store)

	before, err := store.ListTasks(ctx, f.board.ID, "")
	require.NoError(t, err)

	badEntry := NewAuditEntry("user", f.user.ID, auditTaskCreate, "task", "not-a-uuid", f.squad.ID, nil)
	_, err = store.CreateTask(WithPendingAudit(ctx, badEntry), &domain.Task{
		BoardID:       f.board.ID,
		SquadID:       f.squad.ID,
		Title:         "must not survive a failed audit write",
		CreatedByType: "user",
		CreatedByID:   f.user.ID,
	})
	require.Error(t, err, "audit insert with invalid uuid must fail")

	after, err := store.ListTasks(ctx, f.board.ID, "")
	require.NoError(t, err)
	require.Len(t, after, len(before), "task must have been rolled back with its audit")
}
