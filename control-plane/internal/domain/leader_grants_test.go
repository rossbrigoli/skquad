// S-294 (ADR-0016 D5/D8): leader capability grants on the squad
// board-policy document — default none, granular flags, leader-approval
// resolution requiring BOTH a grant and the enabled knob.
package domain

import "testing"

func TestLeaderGrantDefaultsNone(t *testing.T) {
	t.Parallel()
	p := BoardPolicy{}
	if p.CanPrioritize("a1") || p.CanAssignOthers("a1") || p.IsLeader("a1") {
		t.Fatal("empty policy must grant nothing to anyone")
	}
	if got := p.GrantFor("missing"); got.Any() {
		t.Fatal("GrantFor missing agent must be the zero grant")
	}
}

func TestLeaderGrantGranular(t *testing.T) {
	t.Parallel()
	p := BoardPolicy{LeaderGrants: map[string]LeaderGrant{
		"leader1": {CanPrioritize: true, CanAssignOthers: true},
		"prio":    {CanPrioritize: true},
		"assign":  {CanAssignOthers: true},
		"empty":   {},
	}}
	if !p.CanPrioritize("leader1") || !p.CanAssignOthers("leader1") || !p.IsLeader("leader1") {
		t.Fatal("leader1 must hold both capabilities")
	}
	if !p.CanPrioritize("prio") || p.CanAssignOthers("prio") || !p.IsLeader("prio") {
		t.Fatal("prio must be a prioritize-only leader")
	}
	if p.CanPrioritize("assign") || !p.CanAssignOthers("assign") || !p.IsLeader("assign") {
		t.Fatal("assign must be an assign-only leader")
	}
	if p.IsLeader("empty") {
		t.Fatal("an all-false grant is not a leader")
	}
}

func TestLeaderGrantsValidateRejectsBlankKeys(t *testing.T) {
	t.Parallel()
	if err := (BoardPolicy{LeaderGrants: map[string]LeaderGrant{"  ": {CanPrioritize: true}}}).Validate(); err == nil {
		t.Fatal("blank agent id key must be rejected")
	}
	if err := (BoardPolicy{LeaderGrants: map[string]LeaderGrant{"a1": {CanPrioritize: true}}}).Validate(); err != nil {
		t.Fatalf("valid grant map must validate: %v", err)
	}
}

func TestLeaderMayApprove(t *testing.T) {
	t.Parallel()
	squad := &Squad{BoardPolicy: BoardPolicy{
		LeaderGrants: map[string]LeaderGrant{"boss": {CanPrioritize: true}},
	}}
	leader := &Agent{ID: "boss"}
	plain := &Agent{ID: "worker"}

	// Grant held but leader_approval_allowed unset → platform default false.
	if LeaderMayApprove(leader, squad) {
		t.Fatal("approval must default to strictly human")
	}
	// Knob on: leader may approve, non-leader may not.
	allowed := true
	squad.BoardPolicy.LeaderApprovalAllowed = &allowed
	if !LeaderMayApprove(leader, squad) {
		t.Fatal("granted leader with knob on may approve")
	}
	if LeaderMayApprove(plain, squad) {
		t.Fatal("ungented agent must never leader-approve")
	}
	// Knob back off: denied again.
	squad.BoardPolicy.LeaderApprovalAllowed = boolp(false)
	if LeaderMayApprove(leader, squad) {
		t.Fatal("knob off must deny even a leader")
	}
	if LeaderMayApprove(nil, squad) || LeaderMayApprove(leader, nil) {
		t.Fatal("nil agent/squad must be false")
	}
}
