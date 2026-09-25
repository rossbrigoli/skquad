package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// SetAgentPermissions must enqueue an upsert_agent Kubernetes outbox event so
// the Agent CR (workspaceSecrets in particular) re-converges when grants
// change (ADR-0009).

func TestMemorySetAgentPermissionsEnqueuesAgentUpsert(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	user, err := store.UpsertUser(ctx, &domain.User{Email: "perm-outbox@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:      "Perm Outbox Squad",
		OwnerID:   user.ID,
		Namespace: "squad-perm-outbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "Worker"})
	if err != nil {
		t.Fatal(err)
	}

	// Drain the create events.
	events, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxPending, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if err := store.MarkKubernetesOutboxApplied(ctx, e.ID); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.SetAgentPermissions(ctx, agent.ID, []domain.AgentPermission{
		{ResourceType: domain.ResProjectWorkspace, ResourceID: "ws-1", GrantedBy: user.ID},
	}); err != nil {
		t.Fatal(err)
	}

	events, err = store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxPending, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("pending events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Operation != domain.KubernetesOpUpsertAgent {
		t.Fatalf("operation = %q, want %q", ev.Operation, domain.KubernetesOpUpsertAgent)
	}
	var payload domain.KubernetesOutboxPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Agent == nil || payload.Agent.ID != agent.ID {
		t.Fatalf("payload agent = %+v, want agent %s", payload.Agent, agent.ID)
	}

	// Unknown agent must not enqueue anything and must report NotFound.
	if err := store.SetAgentPermissions(ctx, "missing-agent", nil); err != ErrNotFound {
		t.Fatalf("missing agent err = %v, want ErrNotFound", err)
	}
}

func TestPostgresSetAgentPermissionsEnqueuesAgentUpsert(t *testing.T) {
	store := postgresTestStore(t)
	fx := newPGFixture(t, store)
	ctx := context.Background()

	// Drain the fixture-created events for this agent.
	events, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxPending, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.AggregateID == fx.agent.ID && e.Operation == domain.KubernetesOpUpsertAgent {
			if err := store.MarkKubernetesOutboxApplied(ctx, e.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	ws, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type:         domain.ResProjectWorkspace,
		Name:         "parity-repo-" + fx.agent.ID[:8],
		Manifest:     []byte(`{"kind":"git","default_branch":"main"}`),
		AuthRef:      "k8s://squad-ws/ws-parity-token",
		Status:       domain.ResourceActive,
		RegisteredBy: fx.user.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetAgentPermissions(ctx, fx.agent.ID, []domain.AgentPermission{
		{ResourceType: domain.ResProjectWorkspace, ResourceID: ws.ID, GrantedBy: fx.user.ID},
	}); err != nil {
		t.Fatal(err)
	}

	events, err = store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxPending, 500)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.AggregateID == fx.agent.ID && e.Operation == domain.KubernetesOpUpsertAgent {
			found = true
			var payload domain.KubernetesOutboxPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Agent == nil || payload.Agent.ID != fx.agent.ID {
				t.Fatalf("payload agent = %+v, want %s", payload.Agent, fx.agent.ID)
			}
		}
	}
	if !found {
		t.Fatal("expected pending upsert_agent outbox event after SetAgentPermissions")
	}

	// Unknown agent must report NotFound (parity with MemoryStore).
	if err := store.SetAgentPermissions(ctx, "00000000-0000-0000-0000-000000000000", nil); err != ErrNotFound {
		t.Fatalf("missing agent err = %v, want ErrNotFound", err)
	}
}
