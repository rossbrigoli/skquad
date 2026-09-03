package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// These tests run the Postgres implementation against a real database so that
// SQL-only semantics — lease fencing, the conditional reaper updates, outbox
// leasing, vector round-trips — are actually exercised. The in-memory store
// cannot validate any of that.
//
// They skip unless a test database is supplied. CI provides
// SKQUAD_TEST_DATABASE_URL pointing at a pgvector/pg16 service container.
//
// Do not add t.Parallel() here: ReapExpiredTaskExecutions and the outbox lease
// are global sweeps, so concurrent tests would race on each other's rows.

func postgresTestStore(t *testing.T) *PostgresStore {
	t.Helper()

	dsn := os.Getenv("SKQUAD_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("SKQUAD_TEST_DATABASE_URL (or DATABASE_URL) is required for Postgres store tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	store, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("new postgres store: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// pgFixture owns a private user + squad + agent + board so tests never observe
// each other's rows, and removes them afterwards.
type pgFixture struct {
	store *PostgresStore
	user  *domain.User
	squad *domain.Squad
	agent *domain.Agent
	board *domain.Board
}

func newPGFixture(t *testing.T, store *PostgresStore) *pgFixture {
	t.Helper()

	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	user, err := store.UpsertUser(ctx, &domain.User{
		OIDCIssuer:    "https://issuer.skquad.test",
		OIDCSubject:   "subject-" + tag,
		Email:         "user-" + tag + "@example.test",
		EmailVerified: true,
		Name:          "PG Test User",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:    "squad-" + tag,
		OwnerID: user.ID,
		Status:  domain.SquadActive,
	})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}

	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: squad.ID,
		Name:    "agent-" + tag,
		Status:  domain.AgentIdle,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	board, err := store.GetBoard(ctx, squad.ID)
	if err != nil {
		t.Fatalf("get board: %v", err)
	}

	f := &pgFixture{store: store, user: user, squad: squad, agent: agent, board: board}
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

func (f *pgFixture) cleanup(t *testing.T) {
	t.Helper()

	store := f.store
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, id := range []string{f.squad.ID, f.agent.ID} {
		if _, err := store.pool.Exec(ctx, `DELETE FROM kubernetes_outbox WHERE aggregate_id = $1`, id); err != nil {
			t.Logf("cleanup outbox %s: %v", id, err)
		}
	}
	// Squads cascade to boards, tasks, agents, task_executions and memory.
	if _, err := store.pool.Exec(ctx, `DELETE FROM squads WHERE id = $1`, f.squad.ID); err != nil {
		t.Logf("cleanup squad %s: %v", f.squad.ID, err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, f.user.ID); err != nil {
		t.Logf("cleanup user %s: %v", f.user.ID, err)
	}
}

func (f *pgFixture) newTask(t *testing.T, store *PostgresStore, title string) *domain.Task {
	t.Helper()

	task, err := store.CreateTask(context.Background(), &domain.Task{
		BoardID:         f.board.ID,
		SquadID:         f.squad.ID,
		Title:           title,
		Status:          domain.TaskTodo,
		AssigneeAgentID: f.agent.ID,
		CreatedByType:   "user",
		CreatedByID:     f.user.ID,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// lapseExecution forces an execution's lease into the past using the database
// clock, which is the clock ClaimNextTask and the reaper compare against.
func lapseExecution(t *testing.T, store *PostgresStore, executionID string) {
	t.Helper()

	if _, err := store.pool.Exec(context.Background(),
		`UPDATE task_executions SET lease_expires_at = now() - interval '2 minutes' WHERE id = $1`,
		executionID); err != nil {
		t.Fatalf("lapse execution lease: %v", err)
	}
}

func TestPostgresStoreTaskExecutionLeaseAndFencing(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	task := f.newTask(t, store, "claim me")

	claimed, err := store.ClaimNextTask(ctx, f.agent.ID, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ExecutionID == "" || claimed.FencingToken == "" {
		t.Fatalf("claim returned no execution/fencing token: %+v", claimed)
	}
	if claimed.Status != domain.TaskInProgress {
		t.Fatalf("claimed task status = %q, want in-progress", claimed.Status)
	}

	// An agent with a live lease must not be handed a second task.
	if _, err := store.ClaimNextTask(ctx, f.agent.ID, "worker-2", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim error = %v, want ErrNotFound", err)
	}

	// Heartbeat with the wrong fencing token is rejected as a conflict.
	if _, err := store.HeartbeatTaskExecution(ctx, f.agent.ID, claimed.ExecutionID, "not-the-token", time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale-token heartbeat error = %v, want ErrConflict", err)
	}

	// A valid heartbeat extends the lease.
	before := claimed.LeaseExpiresAt
	renewed, err := store.HeartbeatTaskExecution(ctx, f.agent.ID, claimed.ExecutionID, claimed.FencingToken, 10*time.Minute)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !renewed.LeaseExpiresAt.After(before) {
		t.Fatalf("lease not extended: before=%s after=%s", before, renewed.LeaseExpiresAt)
	}

	// Completing with a stale token must not touch the task.
	if _, err := store.CompleteTaskExecution(ctx, f.agent.ID, task.ID, claimed.ExecutionID, "not-the-token", domain.TaskDone, "nope"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale-token complete error = %v, want ErrConflict", err)
	}

	completed, err := store.CompleteTaskExecution(ctx, f.agent.ID, task.ID, claimed.ExecutionID, claimed.FencingToken, domain.TaskDone, "all good")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.Status != domain.TaskDone {
		t.Fatalf("completed task status = %q, want done", completed.Status)
	}

	rows, err := store.pool.Query(ctx, `
		SELECT status, coalesce(result_status, ''), result_summary, completed_at IS NOT NULL
		FROM task_executions WHERE id = $1`, claimed.ExecutionID)
	if err != nil {
		t.Fatalf("query execution: %v", err)
	}
	defer rows.Close()
	var (
		status   string
		result   string
		summary  string
		finished bool
		rowCount int
	)
	for rows.Next() {
		rowCount++
		if err := rows.Scan(&status, &result, &summary, &finished); err != nil {
			t.Fatalf("scan execution: %v", err)
		}
	}
	rows.Close()
	if rowCount != 1 {
		t.Fatalf("execution rows = %d, want 1", rowCount)
	}
	if status != string(domain.TaskExecutionCompleted) || result != string(domain.TaskDone) || summary != "all good" || !finished {
		t.Fatalf("execution row = %q/%q/%q finished=%v", status, result, summary, finished)
	}

	// The completed attempt must no longer be reported as active on the board.
	active, err := store.ListBoardTaskExecutions(ctx, f.board.ID)
	if err != nil {
		t.Fatalf("list board executions: %v", err)
	}
	for _, exec := range active {
		if exec.ID == claimed.ExecutionID {
			t.Fatal("completed execution still listed as active")
		}
	}
}

func TestPostgresStoreReapExpiredTaskExecutions(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	task := f.newTask(t, store, "orphan me")
	claimed, err := store.ClaimNextTask(ctx, f.agent.ID, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lapseExecution(t, store, claimed.ExecutionID)

	reaped, err := store.ReapExpiredTaskExecutions(ctx, time.Now())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if reaped < 1 {
		t.Fatalf("reaped = %d, want >= 1", reaped)
	}

	var (
		status   string
		summary  string
		finished bool
	)
	err = store.pool.QueryRow(ctx, `
		SELECT status, result_summary, completed_at IS NOT NULL
		FROM task_executions WHERE id = $1`, claimed.ExecutionID).
		Scan(&status, &summary, &finished)
	if err != nil {
		t.Fatalf("query execution: %v", err)
	}
	if status != string(domain.TaskExecutionExpired) {
		t.Fatalf("execution status = %q, want expired", status)
	}
	if summary != "lease expired without completion" {
		t.Fatalf("execution summary = %q", summary)
	}
	if !finished {
		t.Fatal("expired execution completed_at not set")
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != domain.TaskTodo {
		t.Fatalf("task status = %q, want todo after reap", got.Status)
	}

	// Idempotent for this execution: re-reaping must not resurrect it.
	if _, err := store.ReapExpiredTaskExecutions(ctx, time.Now()); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	err = store.pool.QueryRow(ctx, `SELECT status FROM task_executions WHERE id = $1`, claimed.ExecutionID).Scan(&status)
	if err != nil {
		t.Fatalf("re-query execution: %v", err)
	}
	if status != string(domain.TaskExecutionExpired) {
		t.Fatalf("execution status after second reap = %q, want expired", status)
	}
}

func TestPostgresStoreReapKeepsTaskInProgressWithLiveAttempt(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	task := f.newTask(t, store, "reclaimed task")
	first, err := store.ClaimNextTask(ctx, f.agent.ID, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	lapseExecution(t, store, first.ExecutionID)

	// Lazy reclaim hands the same task to a fresh worker with a live lease.
	second, err := store.ClaimNextTask(ctx, f.agent.ID, "worker-2", time.Minute)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if second.ID != task.ID {
		t.Fatalf("reclaimed task = %q, want %q", second.ID, task.ID)
	}

	if _, err := store.ReapExpiredTaskExecutions(ctx, time.Now()); err != nil {
		t.Fatalf("reap: %v", err)
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != domain.TaskInProgress {
		t.Fatalf("task status = %q, want in-progress while a live attempt remains", got.Status)
	}

	// The fresh attempt must still be completable.
	if _, err := store.CompleteTaskExecution(ctx, f.agent.ID, task.ID, second.ExecutionID, second.FencingToken, domain.TaskDone, "recovered"); err != nil {
		t.Fatalf("complete reclaimed attempt: %v", err)
	}
}

func TestPostgresStoreReapSkipsHeartbeatedAndCompletedExecutions(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	// Heartbeat that pushes the lease past the cutoff wins over the reaper.
	heartbeated := f.newTask(t, store, "heartbeat wins")
	claimed, err := store.ClaimNextTask(ctx, f.agent.ID, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.HeartbeatTaskExecution(ctx, f.agent.ID, claimed.ExecutionID, claimed.FencingToken, 10*time.Minute); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	cutoff := time.Now().Add(2 * time.Minute)
	if _, err := store.ReapExpiredTaskExecutions(ctx, cutoff); err != nil {
		t.Fatalf("reap: %v", err)
	}
	var status string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM task_executions WHERE id = $1`, claimed.ExecutionID).Scan(&status); err != nil {
		t.Fatalf("query heartbeat execution: %v", err)
	}
	if status != string(domain.TaskExecutionActive) {
		t.Fatalf("heartbeated execution status = %q, want active", status)
	}
	if got, err := store.GetTask(ctx, heartbeated.ID); err != nil {
		t.Fatalf("get heartbeated task: %v", err)
	} else if got.Status != domain.TaskInProgress {
		t.Fatalf("heartbeated task status = %q, want in-progress", got.Status)
	}

	// A completed execution is never reaped, even with a cutoff far in the future.
	done, err := store.CompleteTaskExecution(ctx, f.agent.ID, heartbeated.ID, claimed.ExecutionID, claimed.FencingToken, domain.TaskDone, "finished")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := store.ReapExpiredTaskExecutions(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("reap after complete: %v", err)
	}
	if got, err := store.GetTask(ctx, heartbeated.ID); err != nil {
		t.Fatalf("get completed task: %v", err)
	} else if got.Status != done.Status && got.Status != domain.TaskDone {
		t.Fatalf("completed task status = %q, want done", got.Status)
	}
	if err := store.pool.QueryRow(ctx, `SELECT status FROM task_executions WHERE id = $1`, claimed.ExecutionID).Scan(&status); err != nil {
		t.Fatalf("query completed execution: %v", err)
	}
	if status != string(domain.TaskExecutionCompleted) {
		t.Fatalf("completed execution status = %q, want completed", status)
	}
}

func TestPostgresStoreSquadAccessGrants(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	other, err := store.UpsertUser(ctx, &domain.User{
		Email:         fmt.Sprintf("other-%d@example.test", time.Now().UnixNano()),
		EmailVerified: true,
		Name:          "Grant Target",
	})
	if err != nil {
		t.Fatalf("upsert other user: %v", err)
	}

	// Owner always has access; a stranger has none.
	if allowed, err := store.UserMayAccessSquad(ctx, f.user.ID, f.squad.ID, "add_task"); err != nil || !allowed {
		t.Fatalf("owner access = %v (%v), want allowed", allowed, err)
	}
	if allowed, err := store.UserMayAccessSquad(ctx, other.ID, f.squad.ID, "talk"); err != nil || allowed {
		t.Fatalf("stranger access = %v (%v), want denied", allowed, err)
	}

	grant, err := store.CreateGrant(ctx, &domain.AccessGrant{
		SquadID:     f.squad.ID,
		GranteeType: domain.GranteeUser,
		GranteeID:   other.ID,
		Permissions: "talk",
		GrantedBy:   f.user.ID,
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}

	if allowed, err := store.UserMayAccessSquad(ctx, other.ID, f.squad.ID, "talk"); err != nil || !allowed {
		t.Fatalf("granted talk access = %v (%v), want allowed", allowed, err)
	}
	// "ping" is implied by "talk"; "add_task" is not.
	if allowed, err := store.UserMayAccessSquad(ctx, other.ID, f.squad.ID, "ping"); err != nil || !allowed {
		t.Fatalf("implied ping access = %v (%v), want allowed", allowed, err)
	}
	if allowed, err := store.UserMayAccessSquad(ctx, other.ID, f.squad.ID, "add_task"); err != nil || allowed {
		t.Fatalf("ungranted add_task access = %v (%v), want denied", allowed, err)
	}

	if err := store.RevokeGrant(ctx, grant.ID); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}
	if allowed, err := store.UserMayAccessSquad(ctx, other.ID, f.squad.ID, "talk"); err != nil || allowed {
		t.Fatalf("post-revoke access = %v (%v), want denied", allowed, err)
	}
	if err := store.RevokeGrant(ctx, grant.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke error = %v, want ErrNotFound", err)
	}
}

func TestPostgresStoreAgentMemoryTrustAndEmbeddingRoundTrip(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	embedding := make([]float64, 1536)
	embedding[0] = 1

	created, err := store.CreateAgentMemory(ctx, &domain.AgentMemory{
		AgentID:        f.agent.ID,
		SquadID:        f.squad.ID,
		Content:        "pg completion summary",
		Embedding:      embedding,
		EmbeddingModel: "test-model",
	})
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	if created.TrustLevel != "raw_model_output" || created.Provenance != "task_completion" || created.ReviewStatus != "pending_review" {
		t.Fatalf("memory trust defaults = %q/%q/%q", created.TrustLevel, created.Provenance, created.ReviewStatus)
	}
	if created.RawContent != "pg completion summary" {
		t.Fatalf("RawContent = %q", created.RawContent)
	}

	listed, err := store.ListAgentMemory(ctx, f.agent.ID, f.squad.ID, nil, 5)
	if err != nil {
		t.Fatalf("list memory: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("memory count = %d, want 1", len(listed))
	}
	if listed[0].EmbeddingModel != "test-model" {
		t.Fatalf("embedding model = %q", listed[0].EmbeddingModel)
	}
	if len(listed[0].Embedding) != 1536 || listed[0].Embedding[0] != 1 {
		t.Fatalf("embedding round-trip failed (len=%d)", len(listed[0].Embedding))
	}

	// A second, semantically distant memory plus a query embedding must rank the
	// close one first even though the far one is newer.
	far := make([]float64, 1536)
	far[1] = 1
	if _, err := store.CreateAgentMemory(ctx, &domain.AgentMemory{
		AgentID:        f.agent.ID,
		SquadID:        f.squad.ID,
		Content:        "newer but farther",
		Embedding:      far,
		EmbeddingModel: "test-model",
	}); err != nil {
		t.Fatalf("create second memory: %v", err)
	}

	ranked, err := store.ListAgentMemory(ctx, f.agent.ID, f.squad.ID, embedding, 2)
	if err != nil {
		t.Fatalf("semantic list: %v", err)
	}
	if len(ranked) != 2 {
		t.Fatalf("ranked count = %d, want 2", len(ranked))
	}
	if ranked[0].ID != created.ID {
		t.Fatalf("semantic ranking put %q first, want %q", ranked[0].ID, created.ID)
	}
}

func TestPostgresStoreKubernetesOutboxLeaseAndRetry(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	// Squad + agent creation must have enqueued outbox intents.
	pending, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxPending, 50)
	if err != nil {
		t.Fatalf("list pending outbox: %v", err)
	}
	var squadEvent, agentEvent *domain.KubernetesOutboxEvent
	for _, event := range pending {
		switch {
		case event.AggregateID == f.squad.ID && event.Operation == domain.KubernetesOpUpsertSquad:
			squadEvent = event
		case event.AggregateID == f.agent.ID && event.Operation == domain.KubernetesOpUpsertAgent:
			agentEvent = event
		}
	}
	if squadEvent == nil || agentEvent == nil {
		t.Fatalf("outbox intents missing: squad=%v agent=%v", squadEvent, agentEvent)
	}

	// Leasing hands the events out and holds them for the lease duration.
	leased, err := store.LeaseKubernetesOutbox(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("lease outbox: %v", err)
	}
	found := false
	for _, event := range leased {
		if event.ID == squadEvent.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("leased batch did not include the squad intent")
	}

	// A failure defers the next attempt, so an immediate re-lease must skip it.
	if err := store.MarkKubernetesOutboxFailed(ctx, squadEvent.ID, "boom", time.Hour); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	again, err := store.LeaseKubernetesOutbox(ctx, 100, time.Minute)
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	for _, event := range again {
		if event.ID == squadEvent.ID {
			t.Fatal("failed intent was re-leased before its retry delay")
		}
	}

	failed, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxFailed, 50)
	if err != nil {
		t.Fatalf("list failed outbox: %v", err)
	}
	var retries int
	for _, event := range failed {
		if event.ID == squadEvent.ID {
			retries = event.Attempts
			if event.LastError != "boom" {
				t.Fatalf("last error = %q, want boom", event.LastError)
			}
		}
	}
	if retries != 1 {
		t.Fatalf("attempts = %d, want 1", retries)
	}

	if err := store.MarkKubernetesOutboxApplied(ctx, agentEvent.ID); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	applied, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxApplied, 50)
	if err != nil {
		t.Fatalf("list applied outbox: %v", err)
	}
	seen := false
	for _, event := range applied {
		if event.ID == agentEvent.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatal("applied intent missing from applied list")
	}
	if err := store.MarkKubernetesOutboxApplied(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mark applied unknown error = %v, want ErrNotFound", err)
	}
}

func TestPostgresStoreTaskStatusAndListFilters(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	first := f.newTask(t, store, "first todo")
	second := f.newTask(t, store, "second todo")
	if second.Position <= first.Position {
		t.Fatalf("positions not ordered: first=%d second=%d", first.Position, second.Position)
	}

	todos, err := store.ListTasks(ctx, f.board.ID, domain.TaskTodo)
	if err != nil {
		t.Fatalf("list todos: %v", err)
	}
	if len(todos) != 2 {
		t.Fatalf("todo count = %d, want 2", len(todos))
	}

	second.Status = domain.TaskDone
	if _, err := store.UpdateTask(ctx, second); err != nil {
		t.Fatalf("update task: %v", err)
	}
	todos, err = store.ListTasks(ctx, f.board.ID, domain.TaskTodo)
	if err != nil {
		t.Fatalf("list todos after update: %v", err)
	}
	if len(todos) != 1 || todos[0].ID != first.ID {
		t.Fatalf("todos after update = %+v, want only %q", todos, first.ID)
	}

	agentTasks, err := store.ListAgentTasks(ctx, f.agent.ID)
	if err != nil {
		t.Fatalf("list agent tasks: %v", err)
	}
	if len(agentTasks) != 2 {
		t.Fatalf("agent task count = %d, want 2", len(agentTasks))
	}

	if err := store.DeleteTask(ctx, second.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if _, err := store.GetTask(ctx, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted task error = %v, want ErrNotFound", err)
	}
	if err := store.DeleteTask(ctx, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete error = %v, want ErrNotFound", err)
	}
}

func TestPostgresStoreDuplicateSquadNameConflicts(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	if _, err := store.CreateSquad(ctx, &domain.Squad{
		Name:    f.squad.Name,
		OwnerID: f.user.ID,
		Status:  domain.SquadActive,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate squad error = %v, want ErrConflict", err)
	}

	if _, err := store.GetSquad(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown squad error = %v, want ErrNotFound", err)
	}
}
