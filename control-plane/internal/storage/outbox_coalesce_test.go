package storage

import (
	"context"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// TestPostgresSetTaskExecutionPromptSHA regression-tests the WP5 run-audit
// start path: the UPDATE previously referenced $5 with only four bound
// arguments (and never referenced $3), so every task.start carrying a
// prompt_sha failed with "could not determine data type of parameter $3"
// on Postgres. The memory-store-only run-audit test never caught it.
func TestPostgresSetTaskExecutionPromptSHA(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	task := f.newTask(t, store, "prompt sha regression")
	claimed, err := store.ClaimNextTask(ctx, f.agent.ID, testWorkerID, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	exec, err := store.SetTaskExecutionPromptSHA(ctx, f.agent.ID, task.ID, sha)
	if err != nil {
		t.Fatalf("SetTaskExecutionPromptSHA: %v", err)
	}
	if exec.PromptSHA != sha {
		t.Fatalf("prompt sha = %q, want %q", exec.PromptSHA, sha)
	}
	if exec.ID != claimed.ExecutionID {
		t.Fatalf("updated execution %q, claimed %q", exec.ID, claimed.ExecutionID)
	}
}

// TestPostgresOutboxCoalescesSupersededEvents pins the enqueue-time
// coalescing contract: every CR payload is full-state idempotent, so a
// newer event for the same aggregate must supersede pending/failed ones
// instead of stacking a backlog behind the worker's fixed batch rate.
func TestPostgresOutboxCoalescesSupersededEvents(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := store.SetAgentStatus(ctx, f.agent.ID, domain.AgentBusy); err != nil {
			t.Fatalf("set agent status %d: %v", i, err)
		}
	}

	var pending int
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM kubernetes_outbox
		WHERE aggregate_type = $1 AND aggregate_id = $2 AND status IN ('pending','failed')
	`, domain.KubernetesAggregateAgent, f.agent.ID).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending outbox events = %d, want 1 (superseded events must coalesce at enqueue)", pending)
	}

	var superseded int
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM kubernetes_outbox
		WHERE aggregate_type = $1 AND aggregate_id = $2
		  AND status = 'applied' AND last_error LIKE 'coalesced:%'
	`, domain.KubernetesAggregateAgent, f.agent.ID).Scan(&superseded); err != nil {
		t.Fatalf("count coalesced: %v", err)
	}
	if superseded != 5 {
		// 5 SetAgentStatus calls each supersede the prior pending event:
		// the fixture's creation upsert plus the 4 intermediate ones.
		t.Fatalf("coalesced events = %d, want 5", superseded)
	}
}
