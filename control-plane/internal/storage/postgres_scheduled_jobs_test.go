package storage

// BUG df5b2277 regression: ClaimDueScheduledJobs must actually fire on
// Postgres. The outer UPDATE ... FROM picked RETURNING used the bare
// scheduledJobColumns list; `id` exists on both scheduled_jobs and the
// picked CTE, so every cron tick died with
// `column reference "id" is ambiguous` and no scheduled job ever ran.
// These tests run the real claim SQL against the test database.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func pgDueJob(t *testing.T, store *PostgresStore, agentID, squadID, createdBy, title string, nextRun time.Time) *domain.ScheduledJob {
	t.Helper()
	job, err := store.CreateScheduledJob(context.Background(), &domain.ScheduledJob{
		AgentID:       agentID,
		SquadID:       squadID,
		Title:         title,
		Message:       "scheduled turn for " + title,
		ScheduleKind:  domain.ScheduleEvery,
		ScheduleSpec:  []byte(`{"interval_seconds": 60}`),
		Enabled:       true,
		NextRunAt:     nextRun,
		CreatedByType: "user",
		CreatedByID:   createdBy,
	})
	require.NoError(t, err)
	return job
}

func TestPostgresClaimDueScheduledJobsFires(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	now := time.Now().UTC()
	due := pgDueJob(t, store, f.agent.ID, f.squad.ID, f.user.ID, "due-job", now.Add(-time.Minute))
	notDue := pgDueJob(t, store, f.agent.ID, f.squad.ID, f.user.ID, "future-job", now.Add(time.Hour))

	// The claim itself is the regression: pre-fix this returned a PG
	// ambiguity error ("column reference \"id\" is ambiguous").
	claimed, err := store.ClaimDueScheduledJobs(ctx, now, 10, 5*time.Minute)
	require.NoError(t, err, "claim must not error on Postgres")

	ids := make(map[string]bool, len(claimed))
	for _, j := range claimed {
		ids[j.ID] = true
	}
	require.True(t, ids[due.ID], "due job must be claimed")
	require.False(t, ids[notDue.ID], "future job must not be claimed")

	// The claimed row must come back fully populated (RETURNING shape).
	var got *domain.ScheduledJob
	for _, j := range claimed {
		if j.ID == due.ID {
			got = j
		}
	}
	require.NotNil(t, got)
	require.Equal(t, due.Title, got.Title)
	require.Equal(t, "running", got.LastRunStatus)
	require.WithinDuration(t, now, got.LastRunAt, time.Minute)
	// Safety net pushed next_run_at forward so a dead worker gets a retry.
	require.True(t, got.NextRunAt.After(now), "next_run_at must be pushed past now by the safety net")

	// Second immediate claim: the row is no longer due (safety net), so
	// it must not be handed out twice.
	again, err := store.ClaimDueScheduledJobs(ctx, now, 10, 5*time.Minute)
	require.NoError(t, err)
	for _, j := range again {
		require.NotEqual(t, due.ID, j.ID, "job must not be re-claimed before its safety-net deadline")
	}

	// After the safety-net window elapses, the same job is claimable
	// again (at-least-once semantics).
	later := now.Add(6 * time.Minute)
	retried, err := store.ClaimDueScheduledJobs(ctx, later, 10, 5*time.Minute)
	require.NoError(t, err)
	found := false
	for _, j := range retried {
		if j.ID == due.ID {
			found = true
		}
	}
	require.True(t, found, "job must be re-claimable after the safety-net window")
}

func TestPostgresClaimDueScheduledJobsConcurrentNoDoubleFire(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	now := time.Now().UTC()
	job := pgDueJob(t, store, f.agent.ID, f.squad.ID, f.user.ID, "contended-job", now.Add(-time.Second))

	// Two replicas claim simultaneously; SKIP LOCKED means exactly one wins.
	var wg sync.WaitGroup
	results := make([][]*domain.ScheduledJob, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.ClaimDueScheduledJobs(ctx, now, 10, 5*time.Minute)
		}(i)
	}
	wg.Wait()
	for i := range errs {
		require.NoError(t, errs[i])
	}

	claimedOnce := 0
	for i := range results {
		for _, j := range results[i] {
			if j.ID == job.ID {
				claimedOnce++
			}
		}
	}
	require.Equal(t, 1, claimedOnce, "contended job must be claimed by exactly one worker")
}
