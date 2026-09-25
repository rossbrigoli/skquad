package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	primaryModelID  = "primary-id"
	fallbackModelID = "fallback-id"
	agentOneID      = "agent-1"
)

func TestProcessOutboxOnceAppliesQueuedSquadAndAgentEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := storage.NewMemoryStore()
	user, err := store.UpsertUser(ctx, &domain.User{Email: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:      "Outbox Squad",
		OwnerID:   user.ID,
		Namespace: "squad-outbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: squad.ID,
		Name:    "Worker",
	})
	if err != nil {
		t.Fatal(err)
	}

	writer := &fakeOutboxWriter{}
	processed, err := ProcessOutboxOnce(ctx, store, writer)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 2 {
		t.Fatalf("processed = %d, want 2", processed)
	}
	want := []string{
		"upsert-squad:" + squad.ID,
		"upsert-agent:" + agent.ID,
	}
	if !equalStrings(writer.ops, want) {
		t.Fatalf("ops = %#v, want %#v", writer.ops, want)
	}
	events, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxApplied, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("applied events = %d, want 2", len(events))
	}
}

func TestProcessOutboxOnceRecordsFailureForRetry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := storage.NewMemoryStore()
	user, err := store.UpsertUser(ctx, &domain.User{Email: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSquad(ctx, &domain.Squad{
		Name:      "Retry Squad",
		OwnerID:   user.ID,
		Namespace: "squad-retry",
	}); err != nil {
		t.Fatal(err)
	}

	writer := &fakeOutboxWriter{err: errors.New("kube unavailable")}
	if _, err := ProcessOutboxOnce(ctx, store, writer); err != nil {
		t.Fatal(err)
	}
	failed, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxFailed, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(failed))
	}
	if failed[0].Attempts != 1 || failed[0].LastError != "kube unavailable" {
		t.Fatalf("failed event = %#v", failed[0])
	}

	if err := store.MarkKubernetesOutboxFailed(ctx, failed[0].ID, failed[0].LastError, -time.Second); err != nil {
		t.Fatal(err)
	}
	writer.err = nil
	if _, err := ProcessOutboxOnce(ctx, store, writer); err != nil {
		t.Fatal(err)
	}
	applied, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxApplied, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied events = %d, want 1", len(applied))
	}
}

type fakeOutboxWriter struct {
	ops []string
	err error
}

func (f *fakeOutboxWriter) UpsertSquad(_ context.Context, squad *domain.Squad) error {
	if f.err != nil {
		return f.err
	}
	f.ops = append(f.ops, "upsert-squad:"+squad.ID)
	return nil
}

func (f *fakeOutboxWriter) DeleteSquad(_ context.Context, squad *domain.Squad) error {
	if f.err != nil {
		return f.err
	}
	f.ops = append(f.ops, "delete-squad:"+squad.ID)
	return nil
}

func (f *fakeOutboxWriter) UpsertAgent(_ context.Context, agent *domain.Agent, _ *domain.AgentIdentity) error {
	if f.err != nil {
		return f.err
	}
	f.ops = append(f.ops, "upsert-agent:"+agent.ID)
	return nil
}

func (f *fakeOutboxWriter) DeleteAgent(_ context.Context, agent *domain.Agent) error {
	if f.err != nil {
		return f.err
	}
	f.ops = append(f.ops, "delete-agent:"+agent.ID)
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// WP5 (ADR-0010 D4): the outbox worker resolves the agent's bound AI
// Model ids into gateway-routable names before writing the CR.
type fakeAIModelResolver struct {
	models map[string]*domain.AIModel
	err    error
}

func (f *fakeAIModelResolver) GetAIModel(_ context.Context, id string) (*domain.AIModel, error) {
	if f.err != nil {
		return nil, f.err
	}
	if m, ok := f.models[id]; ok {
		return m, nil
	}
	return nil, storage.ErrNotFound
}

func TestDeriveBindingModelNamesResolvesPrimaryAndFallback(t *testing.T) {
	t.Parallel()

	resolver := &fakeAIModelResolver{models: map[string]*domain.AIModel{
		primaryModelID:  {ID: primaryModelID, ModelName: "gpt-6-sol"},
		fallbackModelID: {ID: fallbackModelID, ModelName: "halogen/qwen3.8-flash-next"},
	}}
	agent := &domain.Agent{ID: agentOneID, AIModelID: primaryModelID, FallbackAIModelID: fallbackModelID}

	deriveBindingModelNames(context.Background(), resolver, agent)

	if agent.AIModelName != "gpt-6-sol" {
		t.Fatalf("AIModelName = %q, want gpt-6-sol", agent.AIModelName)
	}
	if agent.FallbackAIModelName != "halogen/qwen3.8-flash-next" {
		t.Fatalf("FallbackAIModelName = %q, want halogen/qwen3.8-flash-next", agent.FallbackAIModelName)
	}
}

func TestDeriveBindingModelNamesUnresolvedLeavesNamesEmpty(t *testing.T) {
	t.Parallel()

	resolver := &fakeAIModelResolver{models: map[string]*domain.AIModel{}}
	agent := &domain.Agent{ID: agentOneID, AIModelID: "gone-primary", FallbackAIModelID: "gone-fallback"}

	// Must not error: an unresolvable binding degrades to empty names so
	// the CR writer falls back to the legacy default_model.
	deriveBindingModelNames(context.Background(), resolver, agent)

	if agent.AIModelName != "" || agent.FallbackAIModelName != "" {
		t.Fatalf("unresolved binding must leave names empty, got %q / %q", agent.AIModelName, agent.FallbackAIModelName)
	}
}

func TestDeriveBindingModelNamesNoBindingIsNoop(t *testing.T) {
	t.Parallel()

	resolver := &fakeAIModelResolver{err: errors.New("resolver must not be called")}
	agent := &domain.Agent{ID: agentOneID}

	deriveBindingModelNames(context.Background(), resolver, agent)

	if agent.AIModelName != "" || agent.FallbackAIModelName != "" {
		t.Fatal("unbound agent must not gain model names")
	}
}
