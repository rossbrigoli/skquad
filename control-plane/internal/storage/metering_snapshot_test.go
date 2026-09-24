package storage

import (
	"context"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// WP5 (ADR-0010 D8 + Risk 3): the metering row must carry the served
// model and the rate snapshot used at event time.

func seedMeteringSnapshotFixture(t *testing.T, store Store) (*domain.Squad, *domain.Agent) {
	t.Helper()
	ctx := context.Background()
	user, err := store.UpsertUser(ctx, &domain.User{
		OIDCSubject: "meter-snap-user",
		Email:       "meter-snap@example.test",
		Name:        "Meter Snap User",
		Role:        domain.RoleUser,
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:      "meter-snap-squad",
		OwnerID:   user.ID,
		Namespace: "meter-snap-ns",
	})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:        squad.ID,
		Name:           "meter-snap-agent",
		Role:           "coder",
		Permissions:    []byte("[]"),
		IdleTimeoutSec: 300,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return squad, agent
}

func TestMemoryStoreRecordMeteringKeepsRateSnapshot(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	squad, agent := seedMeteringSnapshotFixture(t, store)

	inRate := 3.0
	outRate := 7.0
	cachedRate := 0.3
	writeRate := 3.75
	err := store.RecordMetering(ctx, &domain.MeteringEvent{
		AgentID:              agent.ID,
		SquadID:              squad.ID,
		Model:                "primary-model",
		ModelUsed:            "fallback-model",
		InputTokens:          1000000,
		OutputTokens:         500000,
		Cost:                 6.5,
		Currency:             "USD",
		RateInputPer1M:       &inRate,
		RateCachedInputPer1M: &cachedRate,
		RateCacheWritePer1M:  &writeRate,
		RateOutputPer1M:      &outRate,
		RateSnapshot:         true,
	})
	if err != nil {
		t.Fatalf("record metering: %v", err)
	}

	var stored *domain.MeteringEvent
	for _, ev := range store.metering {
		stored = ev
	}
	if stored == nil {
		t.Fatal("no metering event stored")
	}
	if stored.ModelUsed != "fallback-model" {
		t.Fatalf("stored model_used = %q, want fallback-model", stored.ModelUsed)
	}
	if !stored.RateSnapshot {
		t.Fatal("stored rate_snapshot = false, want true")
	}
	if stored.RateInputPer1M == nil || *stored.RateInputPer1M != 3.0 {
		t.Fatalf("stored rate_input_per_1m = %v, want 3.0", stored.RateInputPer1M)
	}
	if stored.RateOutputPer1M == nil || *stored.RateOutputPer1M != 7.0 {
		t.Fatalf("stored rate_output_per_1m = %v, want 7.0", stored.RateOutputPer1M)
	}
	if stored.RateCachedInputPer1M == nil || *stored.RateCachedInputPer1M != 0.3 {
		t.Fatalf("stored rate_cached_input_per_1m = %v, want 0.3", stored.RateCachedInputPer1M)
	}
	if stored.RateCacheWritePer1M == nil || *stored.RateCacheWritePer1M != 3.75 {
		t.Fatalf("stored rate_cache_write_per_1m = %v, want 3.75", stored.RateCacheWritePer1M)
	}
}

func TestMemoryStoreMeteringSnapshotIsDeepCopied(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	squad, agent := seedMeteringSnapshotFixture(t, store)

	rate := 1.0
	event := &domain.MeteringEvent{
		AgentID:        agent.ID,
		SquadID:        squad.ID,
		Model:          "m",
		RateInputPer1M: &rate,
		RateSnapshot:   true,
	}
	if err := store.RecordMetering(ctx, event); err != nil {
		t.Fatalf("record metering: %v", err)
	}
	// Mutating the caller's event after recording must not rewrite the
	// stored historical snapshot.
	rate = 999.0
	for _, ev := range store.metering {
		if ev.RateInputPer1M != nil && *ev.RateInputPer1M != 1.0 {
			t.Fatalf("stored rate was mutated through the caller's pointer: %v", *ev.RateInputPer1M)
		}
	}
}
