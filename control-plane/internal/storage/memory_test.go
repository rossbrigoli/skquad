package storage

import (
	"context"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func TestMemoryStoreDefaultsTrustLabelsAndRawContent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	squad := mustCreateMemoryTestSquad(t, ctx, store)
	agent := mustCreateMemoryTestAgent(t, ctx, store, squad.ID)

	created, err := store.CreateAgentMemory(ctx, &domain.AgentMemory{
		AgentID: agent.ID,
		SquadID: squad.ID,
		Content: "raw completion summary",
	})
	if err != nil {
		t.Fatal(err)
	}

	if created.RawContent != "raw completion summary" {
		t.Fatalf("RawContent = %q", created.RawContent)
	}
	if created.TrustLevel != "raw_model_output" {
		t.Fatalf("TrustLevel = %q", created.TrustLevel)
	}
	if created.Provenance != "task_completion" {
		t.Fatalf("Provenance = %q", created.Provenance)
	}
	if created.ReviewStatus != "pending_review" {
		t.Fatalf("ReviewStatus = %q", created.ReviewStatus)
	}
}

func TestMemoryStoreRanksByEmbeddingWhenQueryProvided(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	squad := mustCreateMemoryTestSquad(t, ctx, store)
	agent := mustCreateMemoryTestAgent(t, ctx, store, squad.ID)

	olderCloser, err := store.CreateAgentMemory(ctx, &domain.AgentMemory{
		AgentID:   agent.ID,
		SquadID:   squad.ID,
		Content:   "closest semantic memory",
		Embedding: []float64{1, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	newerFarther, err := store.CreateAgentMemory(ctx, &domain.AgentMemory{
		AgentID:   agent.ID,
		SquadID:   squad.ID,
		Content:   "newer but farther memory",
		Embedding: []float64{0, 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	recent, err := store.ListAgentMemory(ctx, agent.ID, squad.ID, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if recent[0].ID != newerFarther.ID {
		t.Fatalf("recent first ID = %q, want newer memory %q", recent[0].ID, newerFarther.ID)
	}

	semantic, err := store.ListAgentMemory(ctx, agent.ID, squad.ID, []float64{1, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if semantic[0].ID != olderCloser.ID {
		t.Fatalf("semantic first ID = %q, want closest memory %q", semantic[0].ID, olderCloser.ID)
	}
}

func mustCreateMemoryTestSquad(t *testing.T, ctx context.Context, store *MemoryStore) *domain.Squad {
	t.Helper()
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:    "memory-test",
		OwnerID: "owner-1",
		Status:  domain.SquadActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return squad
}

func mustCreateMemoryTestAgent(t *testing.T, ctx context.Context, store *MemoryStore, squadID string) *domain.Agent {
	t.Helper()
	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: squadID,
		Name:    "memory-agent",
		Status:  domain.AgentIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func mustCreateMemoryTestTask(t *testing.T, ctx context.Context, store *MemoryStore, squad *domain.Squad, agent *domain.Agent) *domain.Task {
	t.Helper()
	task, err := store.CreateTask(ctx, &domain.Task{
		BoardID:         store.boardsBySquad[squad.ID],
		SquadID:         squad.ID,
		Title:           "reap test task",
		Status:          domain.TaskTodo,
		AssigneeAgentID: agent.ID,
		CreatedByType:   "user",
		CreatedByID:     "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestMemoryStoreReapExpiredTaskExecutions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	squad := mustCreateMemoryTestSquad(t, ctx, store)
	agent := mustCreateMemoryTestAgent(t, ctx, store, squad.ID)
	task := mustCreateMemoryTestTask(t, ctx, store, squad, agent)

	claimed, err := store.ClaimNextTask(ctx, agent.ID, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ExecutionID == "" {
		t.Fatal("claim returned no execution")
	}

	// A cutoff before the lease expiry reaps nothing: the lease is still live.
	if n, err := store.ReapExpiredTaskExecutions(ctx, time.Now()); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("reaped = %d, want 0 before lease expiry", n)
	}

	// A cutoff past the lease expiry expires the execution and re-queues the task.
	if n, err := store.ReapExpiredTaskExecutions(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}

	exec := store.taskExecs[claimed.ExecutionID]
	if exec.Status != domain.TaskExecutionExpired {
		t.Fatalf("execution status = %q, want expired", exec.Status)
	}
	if exec.ResultSummary != "lease expired without completion" {
		t.Fatalf("execution summary = %q", exec.ResultSummary)
	}
	if exec.CompletedAt.IsZero() {
		t.Fatal("execution completed_at not set")
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskTodo {
		t.Fatalf("task status = %q, want todo", got.Status)
	}

	// Idempotent: a second reap with the same cutoff reaps nothing.
	if n, err := store.ReapExpiredTaskExecutions(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("second reap = %d, want 0", n)
	}
}

func TestMemoryStoreReapSkipsHeartbeatedExecution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	squad := mustCreateMemoryTestSquad(t, ctx, store)
	agent := mustCreateMemoryTestAgent(t, ctx, store, squad.ID)
	mustCreateMemoryTestTask(t, ctx, store, squad, agent)

	claimed, err := store.ClaimNextTask(ctx, agent.ID, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// A heartbeat that extends the lease past the cutoff wins: the same
	// cutoff that would reap the original 1-minute lease (see the main reap
	// test) leaves the heartbeated attempt active.
	if _, err := store.HeartbeatTaskExecution(ctx, agent.ID, claimed.ExecutionID, claimed.FencingToken, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ReapExpiredTaskExecutions(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("reaped = %d, want 0 after heartbeat", n)
	}
	if exec := store.taskExecs[claimed.ExecutionID]; exec.Status != domain.TaskExecutionActive {
		t.Fatalf("execution status = %q, want active", exec.Status)
	}
}

func TestMemoryStoreReapKeepsTaskInProgressWithLiveAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	squad := mustCreateMemoryTestSquad(t, ctx, store)
	agent := mustCreateMemoryTestAgent(t, ctx, store, squad.ID)
	task := mustCreateMemoryTestTask(t, ctx, store, squad, agent)

	claimed, err := store.ClaimNextTask(ctx, agent.ID, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the first lease lapsing, then a lazy reclaim creating a
	// fresh live attempt on the same task.
	store.mu.Lock()
	store.taskExecs[claimed.ExecutionID].LeaseExpiresAt = time.Now().Add(-time.Minute)
	store.mu.Unlock()
	reclaimed, err := store.ClaimNextTask(ctx, agent.ID, "worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.ID != task.ID {
		t.Fatalf("reclaimed task = %q, want %q", reclaimed.ID, task.ID)
	}

	// Reap the lapsed attempt: the fresh lease must keep the task in-progress.
	if n, err := store.ReapExpiredTaskExecutions(ctx, time.Now()); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskInProgress {
		t.Fatalf("task status = %q, want in-progress (live attempt remains)", got.Status)
	}
}

func TestMemoryStoreReapSkipsCompletedExecution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	squad := mustCreateMemoryTestSquad(t, ctx, store)
	agent := mustCreateMemoryTestAgent(t, ctx, store, squad.ID)
	task := mustCreateMemoryTestTask(t, ctx, store, squad, agent)

	claimed, err := store.ClaimNextTask(ctx, agent.ID, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTaskExecution(ctx, agent.ID, task.ID, claimed.ExecutionID, claimed.FencingToken, domain.TaskDone, "done"); err != nil {
		t.Fatal(err)
	}

	if n, err := store.ReapExpiredTaskExecutions(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("reaped = %d, want 0 for completed execution", n)
	}
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskDone {
		t.Fatalf("task status = %q, want done", got.Status)
	}
}
