package storage

// S-195 regression: SumMeteringDaily must run against the real Postgres
// schema. The original implementation joined `llm_providers`, but the
// actual table is `providers` — the memory-store-only tests never caught
// it and the live dashboard failed with "unexpected storage error".
// These tests exercise the exact SQL path via SKQUAD_TEST_DATABASE_URL.

import (
	"context"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func TestPostgresSumMeteringDailyJoinsRealSchema(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	provider, err := store.CreateLLMProvider(ctx, &domain.LLMProvider{
		Name:         "metering-prov-" + time.Now().UTC().Format("150405.000000"),
		Kind:         "openai",
		BaseURL:      "https://provider.skquad.test",
		APIKeyRef:    "secret/ref",
		Status:       domain.ResourceActive,
		RegisteredBy: f.user.ID,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM providers WHERE id = $1`, provider.ID)
	})

	y, m, d := time.Now().UTC().Date()
	day1 := time.Date(y, m, d-2, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(y, m, d-1, 3, 0, 0, 0, time.UTC)
	day1Str := day1.Format("2006-01-02")
	day2Str := day2.Format("2006-01-02")

	events := []*domain.MeteringEvent{
		{
			AgentID: f.agent.ID, SquadID: f.squad.ID, ProviderID: provider.ID,
			Model: "model-a", InputTokens: 100, OutputTokens: 10, Cost: 0.5,
			Currency: "USD", Timestamp: day1, ModelUsed: "model-a",
		},
		{
			// Same day as day1: must bucket together with it.
			AgentID: f.agent.ID, SquadID: f.squad.ID, ProviderID: provider.ID,
			Model: "model-a", InputTokens: 50, OutputTokens: 5, Cost: 0.25,
			Currency: "USD", Timestamp: day1.Add(2 * time.Hour), ModelUsed: "model-a",
		},
		{
			// No provider: LEFT JOIN must degrade to empty name, not drop the row.
			AgentID: f.agent.ID, SquadID: f.squad.ID, ProviderID: "",
			Model: "model-b", InputTokens: 7, OutputTokens: 3, Cost: 0.125,
			Currency: "USD", Timestamp: day2, ModelUsed: "model-b",
		},
	}
	for _, ev := range events {
		if err := store.RecordMetering(ctx, ev); err != nil {
			t.Fatalf("record metering: %v", err)
		}
	}

	rows, err := store.SumMeteringDaily(ctx, day1.Add(-24*time.Hour), []string{f.squad.ID})
	if err != nil {
		t.Fatalf("SumMeteringDaily: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}

	byDay := map[string]domain.MeteringDailyRow{}
	for _, r := range rows {
		byDay[r.Day] = r
	}

	r1, ok := byDay[day1Str]
	if !ok {
		t.Fatalf("missing bucket for %s (got %v)", day1Str, rows)
	}
	if r1.InputTokens != 150 || r1.OutputTokens != 15 {
		t.Fatalf("day1 tokens = %d/%d, want 150/15", r1.InputTokens, r1.OutputTokens)
	}
	if r1.Cost != 0.75 {
		t.Fatalf("day1 cost = %v, want 0.75", r1.Cost)
	}
	if r1.SquadName != f.squad.Name || r1.AgentName != f.agent.Name {
		t.Fatalf("day1 names = %q/%q, want %q/%q", r1.SquadName, r1.AgentName, f.squad.Name, f.agent.Name)
	}
	if r1.ProviderName != provider.Name {
		t.Fatalf("day1 provider = %q, want %q (join broken?)", r1.ProviderName, provider.Name)
	}
	if r1.Currency != "USD" {
		t.Fatalf("day1 currency = %q, want USD", r1.Currency)
	}

	r2, ok := byDay[day2Str]
	if !ok {
		t.Fatalf("missing bucket for %s (got %v)", day2Str, rows)
	}
	if r2.ProviderName != "" {
		t.Fatalf("day2 provider = %q, want empty for NULL provider_id", r2.ProviderName)
	}
	if r2.Model != "model-b" || r2.InputTokens != 7 || r2.OutputTokens != 3 {
		t.Fatalf("day2 row = %+v", r2)
	}
}

func TestPostgresSumMeteringDailySquadAllowlist(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()

	if err := store.RecordMetering(ctx, &domain.MeteringEvent{
		AgentID: f.agent.ID, SquadID: f.squad.ID, Model: "m",
		InputTokens: 1, OutputTokens: 1, Cost: 1, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record metering: %v", err)
	}

	// Empty allowlist means "all squads" (auth layer passes explicit lists).
	all, err := store.SumMeteringDaily(ctx, time.Time{}, nil)
	if err != nil {
		t.Fatalf("SumMeteringDaily all: %v", err)
	}
	found := false
	for _, r := range all {
		if r.SquadID == f.squad.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture squad missing from unfiltered aggregation")
	}

	// A non-matching allowlist must return nothing for our fixture.
	none, err := store.SumMeteringDaily(ctx, time.Time{}, []string{zeroUUID})
	if err != nil {
		t.Fatalf("SumMeteringDaily filtered: %v", err)
	}
	for _, r := range none {
		if r.SquadID == f.squad.ID {
			t.Fatal("fixture squad leaked through non-matching allowlist")
		}
	}
}
