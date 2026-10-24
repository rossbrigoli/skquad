package domain

import "testing"

func intp(v int) *int    { return &v }
func boolp(v bool) *bool { return &v }

func TestBoardPolicyValidate(t *testing.T) {
	t.Parallel()

	if err := (BoardPolicy{}).Validate(); err != nil {
		t.Fatalf("empty policy must validate: %v", err)
	}
	if err := (BoardPolicy{ReopenCapPerDay: intp(0)}).Validate(); err != nil {
		t.Fatalf("cap 0 (reopens disabled) must validate: %v", err)
	}
	if err := (BoardPolicy{ReopenCapPerDay: intp(7)}).Validate(); err != nil {
		t.Fatalf("cap 7 must validate: %v", err)
	}
	if err := (BoardPolicy{ReopenCapPerDay: intp(-1)}).Validate(); err == nil {
		t.Fatal("negative cap must not validate")
	}
}

func TestBoardPolicyEffective(t *testing.T) {
	t.Parallel()

	// Empty document = platform defaults everywhere.
	eff := BoardPolicy{}.Effective()
	if eff.ReopenCapPerDay != 3 { // PlatformReopenCapPerDay
		t.Fatalf("default cap: got %d", eff.ReopenCapPerDay)
	}
	if !eff.SelfApproveDefault {
		t.Fatal("default self_approve must be true")
	}
	if eff.LeaderApprovalAllowed {
		t.Fatal("default leader_approval must be false")
	}

	// Explicit false must survive (pointer semantics).
	eff = BoardPolicy{SelfApproveDefault: boolp(false), LeaderApprovalAllowed: boolp(true), ReopenCapPerDay: intp(0)}.Effective()
	if eff.SelfApproveDefault {
		t.Fatal("explicit false self_approve_default must be preserved")
	}
	if !eff.LeaderApprovalAllowed {
		t.Fatal("explicit true leader_approval_allowed must be preserved")
	}
	if eff.ReopenCapPerDay != 0 {
		t.Fatalf("explicit cap 0 must be preserved, got %d", eff.ReopenCapPerDay)
	}
}

func TestEffectiveSelfApprovePrecedence(t *testing.T) {
	t.Parallel()

	// Table: agent override ?? squad default ?? platform true.
	cases := []struct {
		name  string
		agent *Agent
		squad *Squad
		want  bool
	}{
		{"no agent no squad -> platform true", nil, nil, true},
		{"agent nil override, squad nil default -> platform true", &Agent{}, &Squad{}, true},
		{"agent override false beats squad true", &Agent{SelfApproveAllowed: boolp(false)}, &Squad{BoardPolicy: BoardPolicy{SelfApproveDefault: boolp(true)}}, false},
		{"agent override true beats squad false", &Agent{SelfApproveAllowed: boolp(true)}, &Squad{BoardPolicy: BoardPolicy{SelfApproveDefault: boolp(false)}}, true},
		{"squad false applies when agent unset", &Agent{}, &Squad{BoardPolicy: BoardPolicy{SelfApproveDefault: boolp(false)}}, false},
		{"squad true applies when agent unset", &Agent{}, &Squad{BoardPolicy: BoardPolicy{SelfApproveDefault: boolp(true)}}, true},
		{"nil agent, squad false", nil, &Squad{BoardPolicy: BoardPolicy{SelfApproveDefault: boolp(false)}}, false},
		{"agent false, nil squad", &Agent{SelfApproveAllowed: boolp(false)}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveSelfApprove(tc.agent, tc.squad); got != tc.want {
				t.Fatalf("EffectiveSelfApprove: got %v want %v", got, tc.want)
			}
		})
	}
}

func TestEffectiveReopenCapPrecedence(t *testing.T) {
	t.Parallel()

	if got := EffectiveReopenCap(nil); got != 3 {
		t.Fatalf("nil squad -> platform 3, got %d", got)
	}
	if got := EffectiveReopenCap(&Squad{}); got != 3 {
		t.Fatalf("unset squad cap -> platform 3, got %d", got)
	}
	if got := EffectiveReopenCap(&Squad{BoardPolicy: BoardPolicy{ReopenCapPerDay: intp(0)}}); got != 0 {
		t.Fatalf("squad cap 0 must apply, got %d", got)
	}
	if got := EffectiveReopenCap(&Squad{BoardPolicy: BoardPolicy{ReopenCapPerDay: intp(10)}}); got != 10 {
		t.Fatalf("squad cap 10 must apply, got %d", got)
	}
}

func TestEffectiveLeaderApprovalPrecedence(t *testing.T) {
	t.Parallel()

	if got := EffectiveLeaderApproval(nil); got != false {
		t.Fatalf("nil squad -> platform false, got %v", got)
	}
	if got := EffectiveLeaderApproval(&Squad{}); got != false {
		t.Fatalf("unset -> platform false, got %v", got)
	}
	if got := EffectiveLeaderApproval(&Squad{BoardPolicy: BoardPolicy{LeaderApprovalAllowed: boolp(true)}}); got != true {
		t.Fatalf("squad true must apply, got %v", got)
	}
	if got := EffectiveLeaderApproval(&Squad{BoardPolicy: BoardPolicy{LeaderApprovalAllowed: boolp(false)}}); got != false {
		t.Fatalf("squad explicit false must apply, got %v", got)
	}
}
