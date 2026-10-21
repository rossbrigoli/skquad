package httpapi

// S-281 tests: the scheduled-job scheduler (fire-due-once under
// concurrency, one-shot disable-after-fire, failure cap) and the
// agent-scoped REST surface (CRUD, authz, maxActiveJobs, validation).

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// cronStoreSetup builds a memory store with a squad + agent + board and
// one scheduled job owned by that agent.
func cronStoreSetup(t *testing.T) (*storage.MemoryStore, *domain.Squad, *domain.Agent) {
	t.Helper()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "cron-squad"})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "ticker", Status: domain.AgentIdle})
	require.NoError(t, err)
	return store, squad, agent
}

func newTestScheduledJob(agent *domain.Agent, kind domain.ScheduleKind, spec json.RawMessage, nextRun time.Time) *domain.ScheduledJob {
	return &domain.ScheduledJob{
		AgentID:       agent.ID,
		SquadID:       agent.SquadID,
		Message:       "run the nightly report",
		ScheduleKind:  kind,
		ScheduleSpec:  spec,
		Enabled:       true,
		NextRunAt:     nextRun,
		CreatedByType: "agent",
		CreatedByID:   agent.ID,
	}
}

func TestScheduledJobTickFiresDueJobOnce(t *testing.T) {
	store, _, agent := cronStoreSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	spec, err := domain.BuildScheduleSpec(domain.ScheduleEvery, "", 3600, "", "")
	require.NoError(t, err)
	_, err = store.CreateScheduledJob(ctx, newTestScheduledJob(agent, domain.ScheduleEvery, spec, now.Add(-time.Minute)))
	require.NoError(t, err)

	fired, err := TickScheduledJobsOnce(ctx, store, now)
	require.NoError(t, err)
	require.Equal(t, 1, fired)

	// The canonical wake path ran: a task materialized on the board,
	// assigned to the agent, and the agent mirrored busy.
	tasks, err := store.ListAgentTasks(ctx, agent.ID)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, agent.ID, tasks[0].AssigneeAgentID)
	require.Contains(t, tasks[0].Description, "run the nightly report")
	require.Contains(t, tasks[0].Description, "Scheduled job")
	agentNow, err := store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, domain.AgentBusy, agentNow.Status)

	// The job is re-armed for the next hour, marked fired, fail count clean.
	jobs, err := store.ListScheduledJobs(ctx, agent.ID)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.True(t, jobs[0].Enabled)
	require.Equal(t, "fired", jobs[0].LastRunStatus)
	require.Equal(t, 0, jobs[0].FailCount)
	require.True(t, jobs[0].NextRunAt.After(now))

	// A second tick at the same instant fires nothing (not due yet).
	fired, err = TickScheduledJobsOnce(ctx, store, now)
	require.NoError(t, err)
	require.Equal(t, 0, fired)
}

func TestScheduledJobConcurrentTicksFireOnce(t *testing.T) {
	// SKIP LOCKED semantics in the memory store: the claim is atomic
	// under the write lock, so N concurrent ticks deliver the due job to
	// exactly one of them.
	store, _, agent := cronStoreSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	spec, err := domain.BuildScheduleSpec(domain.ScheduleEvery, "", 3600, "", "")
	require.NoError(t, err)
	_, err = store.CreateScheduledJob(ctx, newTestScheduledJob(agent, domain.ScheduleEvery, spec, now.Add(-time.Minute)))
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			n, err := TickScheduledJobsOnce(ctx, store, now)
			require.NoError(t, err)
			results[idx] = n
		}(i)
	}
	wg.Wait()

	total := 0
	for _, n := range results {
		total += n
	}
	require.Equal(t, 1, total, "concurrent ticks must fire the due job exactly once")

	tasks, err := store.ListAgentTasks(ctx, agent.ID)
	require.NoError(t, err)
	require.Len(t, tasks, 1, "exactly one task materialized")
}

func TestScheduledJobOneShotDisabledAfterFire(t *testing.T) {
	store, _, agent := cronStoreSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(5 * time.Minute).Format(time.RFC3339)
	spec, err := domain.BuildScheduleSpec(domain.ScheduleAt, at, 0, "", "")
	require.NoError(t, err)
	// Force it due: create with next_run in the past by overriding.
	job, err := store.CreateScheduledJob(ctx, newTestScheduledJob(agent, domain.ScheduleAt, spec, now.Add(-time.Second)))
	require.NoError(t, err)

	fired, err := TickScheduledJobsOnce(ctx, store, now)
	require.Equal(t, 1, fired)
	require.NoError(t, err)

	got, err := store.GetScheduledJob(ctx, job.ID)
	require.NoError(t, err)
	require.False(t, got.Enabled, "one-shot 'at' job must be disabled after firing")
	require.Equal(t, "fired", got.LastRunStatus)
	require.Contains(t, got.DisabledReason, "one-shot")

	// Ticking again (even past the old next_run) fires nothing.
	fired, err = TickScheduledJobsOnce(ctx, store, now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, fired)
}

func TestScheduledJobFailureCapDisables(t *testing.T) {
	store := storage.NewMemoryStore()
	ctx := context.Background()
	// A job whose agent does not exist: every fire fails at delivery.
	ghost := &domain.Agent{ID: "11111111-1111-4111-8111-111111111111", SquadID: "22222222-2222-4222-8222-222222222222"}
	spec, err := domain.BuildScheduleSpec(domain.ScheduleEvery, "", 60, "", "")
	require.NoError(t, err)
	job, err := store.CreateScheduledJob(ctx, newTestScheduledJob(ghost, domain.ScheduleEvery, spec, time.Now().UTC().Add(-time.Minute)))
	require.NoError(t, err)

	now := time.Now().UTC()
	for i := 0; i < maxJobFailures; i++ {
		// fired counts SUCCESSES; this job only ever gets claimed,
		// attempted and failed — assert the attempt via fail_count.
		_, err := TickScheduledJobsOnce(ctx, store, now.Add(time.Duration(i)*time.Hour))
		require.NoError(t, err)
		got, err := store.GetScheduledJob(ctx, job.ID)
		require.NoError(t, err)
		require.Equal(t, i+1, got.FailCount, "tick %d should have attempted the job", i)
	}

	got, err := store.GetScheduledJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, maxJobFailures, got.FailCount)
	require.False(t, got.Enabled, "job must auto-disable after %d consecutive failures", maxJobFailures)
	require.Contains(t, got.DisabledReason, "auto-disabled")

	// Disabled: never claimed again.
	fired, err := TickScheduledJobsOnce(ctx, store, now.Add(24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, fired)
}

func TestScheduledJobFailureResetsOnSuccess(t *testing.T) {
	store, _, agent := cronStoreSetup(t)
	ctx := context.Background()
	spec, err := domain.BuildScheduleSpec(domain.ScheduleEvery, "", 60, "", "")
	require.NoError(t, err)
	job, err := store.CreateScheduledJob(ctx, newTestScheduledJob(agent, domain.ScheduleEvery, spec, time.Now().UTC().Add(-time.Minute)))
	require.NoError(t, err)

	// One failure via direct store call (simulated delivery error).
	require.NoError(t, store.FailScheduledJobRun(ctx, job.ID, time.Now().UTC(), maxJobFailures, "boom"))
	got, err := store.GetScheduledJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.FailCount)
	require.True(t, got.Enabled)

	// A successful fire resets the counter.
	fired, err := TickScheduledJobsOnce(ctx, store, time.Now().UTC())
	require.Equal(t, 1, fired)
	require.NoError(t, err)
	got, err = store.GetScheduledJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, 0, got.FailCount)
	require.Equal(t, "fired", got.LastRunStatus)
}

// --- REST API tests --------------------------------------------------------

func cronAPISetup(t *testing.T, squadName string) (http.Handler, *storage.MemoryStore, domain.Squad, domain.Agent, string) {
	t.Helper()
	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": squadName}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "ticker"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)
	return handler, store, squad, agent, credential
}

const pathCronJobs = "/api/v1/agents/me/scheduled-jobs"

func TestCronAPIJobLifecycle(t *testing.T) {
	handler, _, _, agent, credential := cronAPISetup(t, "cron-api")
	agentID := agent.ID

	var created domain.ScheduledJob
	doAgentJSON(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"title":   "nightly report",
		"message": "generate the nightly usage report",
		"schedule": map[string]any{
			"kind":             "every",
			"interval_seconds": 3600,
		},
	}, http.StatusCreated, &created)
	require.NotEmpty(t, created.ID)
	require.True(t, created.Enabled)
	require.Equal(t, domain.ScheduleEvery, created.ScheduleKind)
	require.True(t, created.NextRunAt.After(time.Now().UTC()))

	var listed struct {
		Jobs []domain.ScheduledJob `json:"jobs"`
	}
	doAgentJSON(t, handler, agentID, credential, http.MethodGet, pathCronJobs, nil, http.StatusOK, &listed)
	require.Len(t, listed.Jobs, 1)

	var fetched domain.ScheduledJob
	doAgentJSON(t, handler, agentID, credential, http.MethodGet, pathCronJobs+"/"+created.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, created.ID, fetched.ID)

	// Pause: disabled, next_run_at preserved.
	var paused domain.ScheduledJob
	doAgentJSON(t, handler, agentID, credential, http.MethodPost, pathCronJobs+"/"+created.ID+"/pause", nil, http.StatusOK, &paused)
	require.False(t, paused.Enabled)

	// Resume: re-enabled with next run recomputed into the future.
	var resumed domain.ScheduledJob
	doAgentJSON(t, handler, agentID, credential, http.MethodPost, pathCronJobs+"/"+created.ID+"/resume", nil, http.StatusOK, &resumed)
	require.True(t, resumed.Enabled)
	require.True(t, resumed.NextRunAt.After(time.Now().UTC()))

	// Cancel.
	rec := doAgentRequest(t, handler, agentID, credential, http.MethodDelete, pathCronJobs+"/"+created.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	var missing map[string]map[string]string
	doAgentJSON(t, handler, agentID, credential, http.MethodGet, pathCronJobs+"/"+created.ID, nil, http.StatusNotFound, &missing)
	require.Equal(t, "not_found", missing["error"]["code"])
}

func TestCronAPIValidation(t *testing.T) {
	handler, _, _, agent, credential := cronAPISetup(t, "cron-valid")
	agentID := agent.ID

	// Missing message.
	rec := doAgentRequest(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"schedule": map[string]any{"kind": "every", "interval_seconds": 3600},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Unknown kind.
	rec = doAgentRequest(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"message":  "hi",
		"schedule": map[string]any{"kind": "moonphase"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Bad cron expression.
	rec = doAgentRequest(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"message":  "hi",
		"schedule": map[string]any{"kind": "cron", "expression": "0 99 * * *"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 'at' in the past.
	rec = doAgentRequest(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"message":  "hi",
		"schedule": map[string]any{"kind": "at", "at": "2020-01-01T00:00:00Z"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Interval below the floor.
	rec = doAgentRequest(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"message":  "hi",
		"schedule": map[string]any{"kind": "every", "interval_seconds": 5},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCronAPIMaxActiveJobs(t *testing.T) {
	handler, store, _, agent, credential := cronAPISetup(t, "cron-cap")
	agentID := agent.ID
	ctx := context.Background()

	// Tighten the cap to 2.
	policy := `{"maxActiveJobs": 2}`
	_, err := store.UpdateBuiltinTool(ctx, domain.BuiltinToolCron, nil, json.RawMessage(policy), "admin")
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		var job domain.ScheduledJob
		doAgentJSON(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
			"message":  "tick",
			"schedule": map[string]any{"kind": "every", "interval_seconds": 3600},
		}, http.StatusCreated, &job)
	}

	rec := doAgentRequest(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"message":  "one too many",
		"schedule": map[string]any{"kind": "every", "interval_seconds": 3600},
	})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "too_many_active_jobs")

	// Pause one -> a new create succeeds.
	jobs, err := store.ListScheduledJobs(ctx, agent.ID)
	require.NoError(t, err)
	_, err = store.SetScheduledJobEnabled(ctx, jobs[0].ID, false, time.Time{})
	require.NoError(t, err)

	var job domain.ScheduledJob
	doAgentJSON(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"message":  "fits again",
		"schedule": map[string]any{"kind": "every", "interval_seconds": 3600},
	}, http.StatusCreated, &job)
}

func TestCronAPIAuthzCrossAgentDenied(t *testing.T) {
	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "cron-authz2"}, http.StatusCreated, &squad)

	mkAgent := func(name string) (domain.Agent, string) {
		var a domain.Agent
		doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": name}, http.StatusCreated, &a)
		var id domain.AgentIdentity
		doJSON(t, handler, http.MethodPost, pathAgentsPrefix+a.ID+pathIdentity, nil, http.StatusCreated, &id)
		tok := crWriter.credentialTokens[id.CredentialRef]
		require.NotEmpty(t, tok)
		return a, tok
	}
	ownerAgent, ownerCred := mkAgent("owner-agent")
	other, otherCred := mkAgent("other-agent")

	var created domain.ScheduledJob
	doAgentJSON(t, handler, ownerAgent.ID, ownerCred, http.MethodPost, pathCronJobs, map[string]any{
		"message":  "mine",
		"schedule": map[string]any{"kind": "every", "interval_seconds": 3600},
	}, http.StatusCreated, &created)

	// The other agent cannot see, cancel, pause or resume it.
	rec := doAgentRequest(t, handler, other.ID, otherCred, http.MethodGet, pathCronJobs+"/"+created.ID, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = doAgentRequest(t, handler, other.ID, otherCred, http.MethodDelete, pathCronJobs+"/"+created.ID, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = doAgentRequest(t, handler, other.ID, otherCred, http.MethodPost, pathCronJobs+"/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = doAgentRequest(t, handler, other.ID, otherCred, http.MethodPost, pathCronJobs+"/"+created.ID+"/resume", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Listing as the other agent shows nothing (jobs are agent-scoped).
	var listed struct {
		Jobs []domain.ScheduledJob `json:"jobs"`
	}
	doAgentJSON(t, handler, other.ID, otherCred, http.MethodGet, pathCronJobs, nil, http.StatusOK, &listed)
	require.Len(t, listed.Jobs, 0)
	_ = other

	// The owner still manages their own job.
	var paused domain.ScheduledJob
	doAgentJSON(t, handler, ownerAgent.ID, ownerCred, http.MethodPost, pathCronJobs+"/"+created.ID+"/pause", nil, http.StatusOK, &paused)
	require.False(t, paused.Enabled)
}

func TestCronAPIDisabledToolRejectsCreate(t *testing.T) {
	handler, store, _, agent, credential := cronAPISetup(t, "cron-disabled")
	agentID := agent.ID
	ctx := context.Background()
	off := false
	_, err := store.UpdateBuiltinTool(ctx, domain.BuiltinToolCron, &off, nil, "admin")
	require.NoError(t, err)

	rec := doAgentRequest(t, handler, agentID, credential, http.MethodPost, pathCronJobs, map[string]any{
		"message":  "nope",
		"schedule": map[string]any{"kind": "every", "interval_seconds": 3600},
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "cron_disabled")
}
