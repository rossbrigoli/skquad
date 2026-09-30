package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// S-183: platform settings parity — memory and Postgres expose the same
// get/set semantics and the migration-0026 seed.

func TestPlatformSettingsMemoryParity(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	// Seeded default (mirrors migration 0026).
	raw, found, err := store.GetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds)
	if err != nil || !found || raw != "900" {
		t.Fatalf("seed: got %q found=%v err=%v, want \"900\" true nil", raw, found, err)
	}

	// Missing key is not an error.
	if _, found, err := store.GetPlatformSetting(ctx, "nope"); found || err != nil {
		t.Fatalf("missing key: found=%v err=%v, want false nil", found, err)
	}

	// Upsert overwrites.
	if err := store.SetPlatformSetting(ctx, "parity.key", "v1", "tester"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPlatformSetting(ctx, "parity.key", "v2", "tester2"); err != nil {
		t.Fatal(err)
	}
	raw, found, err = store.GetPlatformSetting(ctx, "parity.key")
	if err != nil || !found || raw != "v2" {
		t.Fatalf("upsert: got %q found=%v err=%v, want \"v2\" true nil", raw, found, err)
	}
}

func TestPlatformSettingsPostgresParity(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()

	// Migration 0026 seed must be present.
	raw, found, err := store.GetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if !found || raw != "900" {
		t.Fatalf("seed: got %q found=%v, want \"900\" true", raw, found)
	}

	// Missing key is not an error.
	if _, found, err := store.GetPlatformSetting(ctx, "missing.key."+t.Name()); found || err != nil {
		t.Fatalf("missing key: found=%v err=%v, want false nil", found, err)
	}

	// Upsert overwrites (unique on key).
	key := fmt.Sprintf("test.parity.%d", time.Now().UnixNano())
	if err := store.SetPlatformSetting(ctx, key, "v1", "tester"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPlatformSetting(ctx, key, "v2", "tester2"); err != nil {
		t.Fatal(err)
	}
	raw, found, err = store.GetPlatformSetting(ctx, key)
	if err != nil || !found || raw != "v2" {
		t.Fatalf("upsert: got %q found=%v err=%v, want \"v2\" true nil", raw, found, err)
	}
}

func TestEnqueueAllAgentUpsertsPostgres(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	count, err := store.EnqueueAllAgentUpserts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count < 1 {
		t.Fatalf("fan-out count = %d, want >= 1 (fixture agent)", count)
	}
	// The fixture agent must have a pending upsert_agent event.
	pending, err := store.ListKubernetesOutbox(ctx, domain.KubernetesOutboxPending, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range pending {
		if ev.AggregateID == f.agent.ID && ev.Operation == domain.KubernetesOpUpsertAgent {
			return
		}
	}
	t.Fatalf("no pending upsert_agent event for fixture agent %s", f.agent.ID)
}
