package kube

import (
	"context"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-183: effective idle timeout precedence — per-agent override (>0)
// wins, then the platform setting, then the 15-minute fallback.
func TestDeriveEffectiveIdleTimeoutPrecedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storage.NewMemoryStore()

	if got := deriveEffectiveIdleTimeout(ctx, store, 60); got != 60 {
		t.Fatalf("agent override: got %d, want 60", got)
	}
	if err := store.SetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds, "1200", "admin"); err != nil {
		t.Fatal(err)
	}
	if got := deriveEffectiveIdleTimeout(ctx, store, 0); got != 1200 {
		t.Fatalf("platform setting: got %d, want 1200", got)
	}
	if err := store.SetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds, "0", "admin"); err != nil {
		t.Fatal(err)
	}
	if got := deriveEffectiveIdleTimeout(ctx, store, 0); got != defaultIdleScaleToZeroSeconds {
		t.Fatalf("zero setting: got %d, want fallback %d", got, defaultIdleScaleToZeroSeconds)
	}
	if err := store.SetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds, "not-a-number", "admin"); err != nil {
		t.Fatal(err)
	}
	if got := deriveEffectiveIdleTimeout(ctx, store, 0); got != defaultIdleScaleToZeroSeconds {
		t.Fatalf("garbage setting: got %d, want fallback %d", got, defaultIdleScaleToZeroSeconds)
	}
}

// noSettingsStore satisfies the outbox store contract without platform
// settings — exercises the "reader not available" fallback branch.
type noSettingsStore struct {
	storage.KubernetesOutboxStore
}

func TestDeriveEffectiveIdleTimeoutWithoutSettingsReader(t *testing.T) {
	t.Parallel()
	store := noSettingsStore{KubernetesOutboxStore: storage.NewMemoryStore()}
	if got := deriveEffectiveIdleTimeout(context.Background(), store, 0); got != defaultIdleScaleToZeroSeconds {
		t.Fatalf("got %d, want fallback %d", got, defaultIdleScaleToZeroSeconds)
	}
}

// capturingAgentWriter records the agent payload handed to UpsertAgent so
// tests can assert the CR-visible idle timeout.
type capturingAgentWriter struct {
	fakeOutboxWriter
	lastAgentIdleSeconds int
}

func (f *capturingAgentWriter) UpsertAgent(ctx context.Context, agent *domain.Agent, identity *domain.AgentIdentity) error {
	f.lastAgentIdleSeconds = agent.IdleTimeoutSec
	return f.fakeOutboxWriter.UpsertAgent(ctx, agent, identity)
}

// The outbox must write the EFFECTIVE idle timeout into the agent CR:
// an agent with no override picks up the platform setting at apply time.
func TestProcessOutboxOnceAppliesEffectiveIdleTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	user, err := store.UpsertUser(ctx, &domain.User{Email: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "Idle Squad", OwnerID: user.ID, Namespace: "squad-idle"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds, "1200", "admin"); err != nil {
		t.Fatal(err)
	}
	// Agent with no per-agent override (0 = follow platform).
	if _, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "Follower"}); err != nil {
		t.Fatal(err)
	}
	// Agent with an explicit override.
	if _, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "Override", IdleTimeoutSec: 45}); err != nil {
		t.Fatal(err)
	}

	writer := &capturingAgentWriter{}
	if _, err := ProcessOutboxOnce(ctx, store, writer); err != nil {
		t.Fatal(err)
	}
	// Last agent processed is "Override" (insertion order) — its explicit
	// 45s must survive.
	if writer.lastAgentIdleSeconds != 45 {
		t.Fatalf("override agent idle = %d, want 45", writer.lastAgentIdleSeconds)
	}

	// Re-run with only the follower: create a fresh store pair.
	store2 := storage.NewMemoryStore()
	user2, err := store2.UpsertUser(ctx, &domain.User{Email: "o2@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad2, err := store2.CreateSquad(ctx, &domain.Squad{Name: "Idle Squad 2", OwnerID: user2.ID, Namespace: "squad-idle-2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.SetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds, "1200", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := store2.CreateAgent(ctx, &domain.Agent{SquadID: squad2.ID, Name: "Follower"}); err != nil {
		t.Fatal(err)
	}
	writer2 := &capturingAgentWriter{}
	if _, err := ProcessOutboxOnce(ctx, store2, writer2); err != nil {
		t.Fatal(err)
	}
	if writer2.lastAgentIdleSeconds != 1200 {
		t.Fatalf("follower agent idle = %d, want platform setting 1200", writer2.lastAgentIdleSeconds)
	}
}

// EnqueueAllAgentUpserts (the settings-change fan-out) must emit one
// upsert_agent event per agent.
func TestEnqueueAllAgentUpsertsFansOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	user, err := store.UpsertUser(ctx, &domain.User{Email: "fan@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "Fan Squad", OwnerID: user.ID, Namespace: "squad-fan"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"A", "B", "C"} {
		if _, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	// Drain the creation events (lease + mark applied) so only the
	// fan-out's events remain pending.
	creation, err := store.LeaseKubernetesOutbox(ctx, 50, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range creation {
		if err := store.MarkKubernetesOutboxApplied(ctx, ev.ID); err != nil {
			t.Fatal(err)
		}
	}
	count, err := store.EnqueueAllAgentUpserts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("fan-out count = %d, want 3", count)
	}
	pending, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxPending, 50)
	if err != nil {
		t.Fatal(err)
	}
	agents := 0
	for _, ev := range pending {
		if ev.Operation == domain.KubernetesOpUpsertAgent {
			agents++
		}
	}
	if agents != 3 {
		t.Fatalf("pending agent upserts = %d, want 3", agents)
	}
}
