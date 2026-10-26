package httpapi

import (
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// S-298: tri-state decode of the per-agent task-subagent turn budget.
// Absent/empty or null = no override (nil); integer 1..512 = override;
// anything else is a validation error.
func TestParseTaskSubagentMaxTurnsTriState(t *testing.T) {
	for _, raw := range []string{"", "null"} {
		v, err := parseTaskSubagentMaxTurns([]byte(raw))
		if err != nil || v != nil {
			t.Fatalf("raw=%q got (%v, %v), want (nil, nil)", raw, v, err)
		}
	}
	v, err := parseTaskSubagentMaxTurns([]byte("1"))
	if err != nil || v == nil || *v != 1 {
		t.Fatalf("raw=1 got (%v, %v), want (&1, nil)", v, err)
	}
	v, err = parseTaskSubagentMaxTurns([]byte("512"))
	if err != nil || v == nil || *v != 512 {
		t.Fatalf("raw=512 got (%v, %v), want (&512, nil)", v, err)
	}
	for _, raw := range []string{"0", "-3", "513", "100000", "\"128\"", "1.5", "many"} {
		if _, err := parseTaskSubagentMaxTurns([]byte(raw)); err == nil {
			t.Fatalf("raw=%q accepted, want validation error", raw)
		}
	}
}

// S-298: apply semantics — absent leaves the field untouched, null
// clears the override, an integer sets it.
func TestApplyTaskSubagentMaxTurnsUpdate(t *testing.T) {
	agent := &domain.Agent{}
	if err := applyTaskSubagentMaxTurnsUpdate(agent, nil); err != nil {
		t.Fatal(err)
	}
	if agent.TaskSubagentMaxTurns != nil {
		t.Fatal("absent field must leave the override untouched")
	}
	five := 5
	agent.TaskSubagentMaxTurns = &five
	if err := applyTaskSubagentMaxTurnsUpdate(agent, []byte("null")); err != nil {
		t.Fatal(err)
	}
	if agent.TaskSubagentMaxTurns != nil {
		t.Fatal("null must clear the override (inherit platform default)")
	}
	if err := applyTaskSubagentMaxTurnsUpdate(agent, []byte("200")); err != nil {
		t.Fatal(err)
	}
	if agent.TaskSubagentMaxTurns == nil || *agent.TaskSubagentMaxTurns != 200 {
		t.Fatalf("got %v, want &200", agent.TaskSubagentMaxTurns)
	}
	if err := applyTaskSubagentMaxTurnsUpdate(agent, []byte("9999")); err == nil {
		t.Fatal("out-of-range value must be rejected")
	}
}
