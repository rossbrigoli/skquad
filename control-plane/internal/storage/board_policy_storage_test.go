package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// S-289 (ADR-0016 D8): board_policy document round-trip and the
// per-agent self_approve_allowed override, memory + postgres parity.

func boardPolicyFixture() domain.BoardPolicy {
	cap := 5
	selfApprove := false
	leader := true
	return domain.BoardPolicy{
		ReopenCapPerDay:       &cap,
		SelfApproveDefault:    &selfApprove,
		LeaderApprovalAllowed: &leader,
	}
}

func assertBoardPolicy(t *testing.T, got, want domain.BoardPolicy, label string) {
	t.Helper()
	derefCap := func(p domain.BoardPolicy) any {
		if p.ReopenCapPerDay == nil {
			return "<nil>"
		}
		return *p.ReopenCapPerDay
	}
	derefBool := func(p *bool) any {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	if derefCap(got) != derefCap(want) ||
		derefBool(got.SelfApproveDefault) != derefBool(want.SelfApproveDefault) ||
		derefBool(got.LeaderApprovalAllowed) != derefBool(want.LeaderApprovalAllowed) {
		t.Fatalf("%s: got %+v want %+v", label, got, want)
	}
}

func TestMemoryBoardPolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "policy-squad", OwnerID: "u1", Status: domain.SquadActive})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}

	// Fresh squad: empty document = platform defaults.
	policy, err := store.GetBoardPolicy(ctx, squad.ID)
	if err != nil {
		t.Fatalf("get board policy: %v", err)
	}
	assertBoardPolicy(t, policy, domain.BoardPolicy{}, "fresh squad policy")
	if eff := squad.BoardPolicy.Effective(); eff.ReopenCapPerDay != 3 || !eff.SelfApproveDefault || eff.LeaderApprovalAllowed {
		t.Fatalf("fresh squad effective: %+v", eff)
	}

	// Replace the whole document.
	want := boardPolicyFixture()
	updated, err := store.UpdateBoardPolicy(ctx, squad.ID, want)
	if err != nil {
		t.Fatalf("update board policy: %v", err)
	}
	assertBoardPolicy(t, updated.BoardPolicy, want, "updated squad (returned)")

	got, err := store.GetBoardPolicy(ctx, squad.ID)
	if err != nil {
		t.Fatalf("get board policy after update: %v", err)
	}
	assertBoardPolicy(t, got, want, "GetBoardPolicy after update")

	viaSquad, err := store.GetSquad(ctx, squad.ID)
	if err != nil {
		t.Fatalf("get squad after policy update: %v", err)
	}
	assertBoardPolicy(t, viaSquad.BoardPolicy, want, "GetSquad after policy update")

	// Whole-document replacement: an empty PUT clears all overrides.
	cleared, err := store.UpdateBoardPolicy(ctx, squad.ID, domain.BoardPolicy{})
	if err != nil {
		t.Fatalf("clear board policy: %v", err)
	}
	assertBoardPolicy(t, cleared.BoardPolicy, domain.BoardPolicy{}, "cleared policy")

	// Stored policy is immune to caller mutation (deep clone).
	mutated := boardPolicyFixture()
	if _, err := store.UpdateBoardPolicy(ctx, squad.ID, mutated); err != nil {
		t.Fatalf("re-set policy: %v", err)
	}
	*mutated.ReopenCapPerDay = 99
	after, err := store.GetBoardPolicy(ctx, squad.ID)
	if err != nil {
		t.Fatalf("get policy after caller mutation: %v", err)
	}
	if after.ReopenCapPerDay == nil || *after.ReopenCapPerDay != 5 {
		t.Fatalf("caller mutated the stored policy: %+v", after)
	}

	// Unknown squad -> ErrNotFound on both methods.
	if _, err := store.GetBoardPolicy(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBoardPolicy unknown squad: want ErrNotFound, got %v", err)
	}
	if _, err := store.UpdateBoardPolicy(ctx, "nope", domain.BoardPolicy{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateBoardPolicy unknown squad: want ErrNotFound, got %v", err)
	}
}

func TestMemoryAgentSelfApproveRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "sa-squad", OwnerID: "u1", Status: domain.SquadActive})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "sa-agent", Status: domain.AgentIdle})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if agent.SelfApproveAllowed != nil {
		t.Fatal("fresh agent must have NULL self_approve_allowed")
	}

	for _, want := range []*bool{boolPtr(false), boolPtr(true), nil} {
		agent.SelfApproveAllowed = want
		updated, err := store.UpdateAgent(ctx, agent)
		if err != nil {
			t.Fatalf("update agent: %v", err)
		}
		if (want == nil) != (updated.SelfApproveAllowed == nil) {
			t.Fatalf("round-trip mismatch: want %v got %v", want, updated.SelfApproveAllowed)
		}
		if want != nil && *updated.SelfApproveAllowed != *want {
			t.Fatalf("round-trip value: want %v got %v", *want, *updated.SelfApproveAllowed)
		}
		fetched, err := store.GetAgent(ctx, agent.ID)
		if err != nil {
			t.Fatalf("get agent: %v", err)
		}
		if (want == nil) != (fetched.SelfApproveAllowed == nil) {
			t.Fatalf("persisted round-trip mismatch: want %v got %v", want, fetched.SelfApproveAllowed)
		}
		if want != nil && *fetched.SelfApproveAllowed != *want {
			t.Fatalf("persisted value: want %v got %v", *want, *fetched.SelfApproveAllowed)
		}
	}
}

func boolPtr(v bool) *bool { return &v }

// ---- Postgres parity (skipped without a test database) ----

func TestPostgresBoardPolicyRoundTrip(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	// Fresh squad: empty document.
	policy, err := store.GetBoardPolicy(ctx, f.squad.ID)
	if err != nil {
		t.Fatalf("get board policy: %v", err)
	}
	assertBoardPolicy(t, policy, domain.BoardPolicy{}, "fresh squad policy (pg)")

	want := boardPolicyFixture()
	updated, err := store.UpdateBoardPolicy(ctx, f.squad.ID, want)
	if err != nil {
		t.Fatalf("update board policy: %v", err)
	}
	assertBoardPolicy(t, updated.BoardPolicy, want, "updated squad (pg)")

	got, err := store.GetBoardPolicy(ctx, f.squad.ID)
	if err != nil {
		t.Fatalf("get board policy after update: %v", err)
	}
	assertBoardPolicy(t, got, want, "GetBoardPolicy after update (pg)")

	viaSquad, err := store.GetSquad(ctx, f.squad.ID)
	if err != nil {
		t.Fatalf("get squad after policy update: %v", err)
	}
	assertBoardPolicy(t, viaSquad.BoardPolicy, want, "GetSquad after policy update (pg)")

	// Whole-document replacement clears everything back to '{}' semantics.
	cleared, err := store.UpdateBoardPolicy(ctx, f.squad.ID, domain.BoardPolicy{})
	if err != nil {
		t.Fatalf("clear board policy: %v", err)
	}
	assertBoardPolicy(t, cleared.BoardPolicy, domain.BoardPolicy{}, "cleared policy (pg)")

	// UpdateSquad must preserve the policy too (round-trip through the
	// full-row update).
	if _, err := store.UpdateBoardPolicy(ctx, f.squad.ID, want); err != nil {
		t.Fatalf("re-set policy: %v", err)
	}
	if _, err := store.UpdateSquad(ctx, updated); err != nil {
		t.Fatalf("update squad row: %v", err)
	}
	after, err := store.GetBoardPolicy(ctx, f.squad.ID)
	if err != nil {
		t.Fatalf("get policy after UpdateSquad: %v", err)
	}
	assertBoardPolicy(t, after, want, "policy survives UpdateSquad (pg)")

	// Unknown squad -> ErrNotFound.
	if _, err := store.GetBoardPolicy(ctx, zeroUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBoardPolicy unknown squad: want ErrNotFound, got %v", err)
	}
	if _, err := store.UpdateBoardPolicy(ctx, zeroUUID, domain.BoardPolicy{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateBoardPolicy unknown squad: want ErrNotFound, got %v", err)
	}
}

func TestPostgresAgentSelfApproveRoundTrip(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	f := newPGFixture(t, store)

	if f.agent.SelfApproveAllowed != nil {
		t.Fatal("fresh agent must have NULL self_approve_allowed (pg)")
	}

	for _, want := range []*bool{boolPtr(false), boolPtr(true), nil} {
		f.agent.SelfApproveAllowed = want
		updated, err := store.UpdateAgent(ctx, f.agent)
		if err != nil {
			t.Fatalf("update agent: %v", err)
		}
		if (want == nil) != (updated.SelfApproveAllowed == nil) {
			t.Fatalf("round-trip mismatch: want %v got %v", want, updated.SelfApproveAllowed)
		}
		if want != nil && *updated.SelfApproveAllowed != *want {
			t.Fatalf("round-trip value: want %v got %v", *want, *updated.SelfApproveAllowed)
		}
		fetched, err := store.GetAgent(ctx, f.agent.ID)
		if err != nil {
			t.Fatalf("get agent: %v", err)
		}
		if (want == nil) != (fetched.SelfApproveAllowed == nil) {
			t.Fatalf("persisted round-trip mismatch: want %v got %v", want, fetched.SelfApproveAllowed)
		}
		if want != nil && *fetched.SelfApproveAllowed != *want {
			t.Fatalf("persisted value: want %v got %v", *want, *fetched.SelfApproveAllowed)
		}
		f.agent = fetched
	}

	// CreateAgent carries the override from birth.
	seeded, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:            f.squad.ID,
		Name:               "seeded-agent",
		Status:             domain.AgentIdle,
		SelfApproveAllowed: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("create seeded agent: %v", err)
	}
	if seeded.SelfApproveAllowed == nil || *seeded.SelfApproveAllowed != false {
		t.Fatalf("seeded agent override lost: %v", seeded.SelfApproveAllowed)
	}
}
