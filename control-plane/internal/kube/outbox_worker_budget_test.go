package kube

// S-203 WP3: the CR mirror zeroes the idle timeout for agents whose
// squad owner is budget-blocked, so the operator scales the pod to
// zero the moment desiredActive goes false (end of turn).

import (
	"context"
	"errors"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/metrics"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

type captureAgentWriter struct {
	agents []*domain.Agent
}

func (w *captureAgentWriter) UpsertSquad(context.Context, *domain.Squad) error { return nil }
func (w *captureAgentWriter) DeleteSquad(context.Context, *domain.Squad) error { return nil }
func (w *captureAgentWriter) DeleteAgent(context.Context, *domain.Agent) error { return nil }
func (w *captureAgentWriter) UpsertAgent(_ context.Context, agent *domain.Agent, _ *domain.AgentIdentity) error {
	cp := *agent
	w.agents = append(w.agents, &cp)
	return nil
}

func TestUpsertAgentFromOutboxZeroIdleTimeoutWhenOwnerBlocked(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	user, err := store.UpsertUser(ctx, &domain.User{Email: "blocked-owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "Budget Squad", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "Worker"})
	if err != nil {
		t.Fatal(err)
	}

	// Unblocked: normal platform idle timeout (memory store seeds 900).
	writer := &captureAgentWriter{}
	base := *agent
	if err := upsertAgentFromOutbox(ctx, store, writer, &domain.KubernetesOutboxPayload{Agent: &base}); err != nil {
		t.Fatal(err)
	}
	if len(writer.agents) != 1 || writer.agents[0].IdleTimeoutSec != 900 {
		t.Fatalf("unblocked idle timeout = %+v, want 900", writer.agents)
	}

	// Owner blocked (current period): the mirror zeroes the timeout.
	if _, err := store.SetBudgetBlock(ctx, user.ID, currentBudgetPeriod(), domain.BudgetSourceUser, true); err != nil {
		t.Fatal(err)
	}
	blocked := *agent
	if err := upsertAgentFromOutbox(ctx, store, writer, &domain.KubernetesOutboxPayload{Agent: &blocked}); err != nil {
		t.Fatal(err)
	}
	if writer.agents[1].IdleTimeoutSec != 0 {
		t.Fatalf("blocked idle timeout = %d, want 0", writer.agents[1].IdleTimeoutSec)
	}

	// Unblock (resume): the mirror restores the normal timeout.
	if _, err := store.SetBudgetBlock(ctx, user.ID, currentBudgetPeriod(), domain.BudgetSourceUser, false); err != nil {
		t.Fatal(err)
	}
	resumed := *agent
	if err := upsertAgentFromOutbox(ctx, store, writer, &domain.KubernetesOutboxPayload{Agent: &resumed}); err != nil {
		t.Fatal(err)
	}
	if writer.agents[2].IdleTimeoutSec != 900 {
		t.Fatalf("resumed idle timeout = %d, want 900", writer.agents[2].IdleTimeoutSec)
	}
}

// currentBudgetPeriod mirrors the httpapi helper (same UTC "2006-01"
// key) so the kube-side check and the enforcement writes agree.
func currentBudgetPeriod() string {
	return time.Now().UTC().Format("2006-01")
}

// failingBudgetChecker forces IsAgentBudgetBlocked to error, simulating a
// storage outage at CR-sync time (S-319 / plan WI-2, T3).
type failingBudgetChecker struct {
	*storage.MemoryStore
}

func (f *failingBudgetChecker) IsAgentBudgetBlocked(_ context.Context, _, _ string) (bool, error) {
	return false, errors.New("forced block check failure (test)")
}

// T3 (CR-sync half): a failed block check must fail CLOSED — the CR gets
// IdleTimeoutSec=0 exactly like a confirmed block. The old fail-open posture
// ("keep normal idle timeout") is a money-control leak. Safe because the
// operator never scales down a pod whose desiredActive is true (asserted
// operator-side in TestDesiredReplicasNeverInterruptsActiveAgent).
func TestUpsertAgentFromOutboxZeroIdleTimeoutWhenBlockCheckFails(t *testing.T) {
	ctx := context.Background()
	base := storage.NewMemoryStore()
	store := &failingBudgetChecker{MemoryStore: base}
	user, err := store.UpsertUser(ctx, &domain.User{Email: "checkfail-owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "CheckFail Squad", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "Worker"})
	if err != nil {
		t.Fatal(err)
	}

	writer := &captureAgentWriter{}
	p := *agent
	if err := upsertAgentFromOutbox(ctx, store, writer, &domain.KubernetesOutboxPayload{Agent: &p}); err != nil {
		t.Fatal(err)
	}
	if len(writer.agents) != 1 {
		t.Fatalf("expected 1 captured agent, got %d", len(writer.agents))
	}
	if writer.agents[0].IdleTimeoutSec != 0 {
		t.Fatalf("check-error idle timeout = %d, want 0 (fail closed)", writer.agents[0].IdleTimeoutSec)
	}
}

// S-321 / WI-9: the CR-sync fail-closed site must also increment
// budget_check_errors_total{site="crsync"} — fail-closed without the
// counter is fail-mysteriously. Delta assertion (process-global counter).
func TestUpsertAgentFromOutboxBlockCheckFailureCounted(t *testing.T) {
	ctx := context.Background()
	base := storage.NewMemoryStore()
	store := &failingBudgetChecker{MemoryStore: base}
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "CrsyncMetric Squad", OwnerID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "Worker"})
	if err != nil {
		t.Fatal(err)
	}

	read := func() float64 {
		c, err := metrics.BudgetCheckErrors.GetMetricWithLabelValues("crsync")
		if err != nil {
			t.Fatal(err)
		}
		var dm dto.Metric
		if err := c.Write(&dm); err != nil {
			t.Fatal(err)
		}
		return dm.GetCounter().GetValue()
	}

	before := read()
	writer := &captureAgentWriter{}
	p := *agent
	if err := upsertAgentFromOutbox(ctx, store, writer, &domain.KubernetesOutboxPayload{Agent: &p}); err != nil {
		t.Fatal(err)
	}
	after := read()
	if after-before != 1 {
		t.Fatalf("crsync check-error delta = %v, want 1", after-before)
	}
}
