package domain

// S-279: policy validation + default-enablement for the file tools
// (read / write / edit / apply_patch). Mirrors the schemas pinned in
// builtin_tools.go: read/write/edit/apply_patch all accept exactly
// {timeoutSeconds, maxBytes}; unknown keys are rejected so a typo can
// never silently widen behaviour.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFileToolsDefaultEnabled(t *testing.T) {
	t.Parallel()
	for _, name := range []string{BuiltinToolRead, BuiltinToolWrite, BuiltinToolEdit, BuiltinToolApplyPatch} {
		if !BuiltinToolDefaultEnabled(name) {
			t.Fatalf("%s should ship enabled by default (migration 0055 seed)", name)
		}
		if !IsBuiltinToolName(name) {
			t.Fatalf("%s missing from BuiltinToolNames", name)
		}
	}
	// Sanity: the original three stay disabled-by-default.
	for _, name := range []string{BuiltinToolExec, BuiltinToolWebFetch, BuiltinToolWebSearch} {
		if BuiltinToolDefaultEnabled(name) {
			t.Fatalf("%s must stay disabled by default (ADR-0012 §1)", name)
		}
	}
}

func TestProcessToolDefaultEnabledAndPolicy(t *testing.T) {
	t.Parallel()
	if !BuiltinToolDefaultEnabled(BuiltinToolProcess) {
		t.Fatal("process should ship enabled by default (migration 0056 seed)")
	}
	if !IsBuiltinToolName(BuiltinToolProcess) {
		t.Fatal("process missing from BuiltinToolNames")
	}
	if errs := ValidateBuiltinPolicy(BuiltinToolProcess, json.RawMessage(`{"maxSessions": 16, "maxLogBytes": 5242880}`)); len(errs) != 0 {
		t.Fatalf("process: valid policy rejected: %v", errs)
	}
	if errs := ValidateBuiltinPolicy(BuiltinToolProcess, json.RawMessage(`{}`)); len(errs) != 0 {
		t.Fatalf("process: empty policy rejected: %v", errs)
	}
	if errs := ValidateBuiltinPolicy(BuiltinToolProcess, json.RawMessage(`{"maxSessions": 0}`)); len(errs) == 0 || !strings.Contains(errs[0], "positive") {
		t.Fatalf("process: maxSessions=0 must be rejected as non-positive, got %v", errs)
	}
	if errs := ValidateBuiltinPolicy(BuiltinToolProcess, json.RawMessage(`{"maxLogBytes": "lots"}`)); len(errs) == 0 || !strings.Contains(errs[0], "integer") {
		t.Fatalf("process: non-integer maxLogBytes must be rejected, got %v", errs)
	}
	if errs := ValidateBuiltinPolicy(BuiltinToolProcess, json.RawMessage(`{"allowedCommands": ["ls"]}`)); len(errs) == 0 || !strings.Contains(errs[0], "allowedCommands") {
		t.Fatalf("process: unknown policy key must be rejected, got %v", errs)
	}
}

func TestFileToolsPolicyValidKeys(t *testing.T) {
	t.Parallel()
	for _, name := range []string{BuiltinToolRead, BuiltinToolWrite, BuiltinToolEdit, BuiltinToolApplyPatch} {
		policy := json.RawMessage(`{"timeoutSeconds": 30, "maxBytes": 262144}`)
		if errs := ValidateBuiltinPolicy(name, policy); len(errs) != 0 {
			t.Fatalf("%s: valid policy rejected: %v", name, errs)
		}
		if errs := ValidateBuiltinPolicy(name, json.RawMessage(`{}`)); len(errs) != 0 {
			t.Fatalf("%s: empty policy rejected: %v", name, errs)
		}
	}
}

func TestFileToolsPolicyRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		policy string
		want   string
	}{
		{BuiltinToolRead, `{"maxLines": 10}`, "maxLines"},
		{BuiltinToolRead, `{"timeoutSeconds": 5, "deniedPatterns": []}`, "deniedPatterns"},
		{BuiltinToolWrite, `{"allowOverwrite": true}`, "allowOverwrite"},
		{BuiltinToolWrite, `{"maxBytes": 100, "allowedPaths": ["*"]}`, "allowedPaths"},
		{BuiltinToolEdit, `{"timeoutSeconds": 0}`, "positive"},
		{BuiltinToolEdit, `{"maxBytes": -1}`, "positive"},
		{BuiltinToolApplyPatch, `{"allowDelete": false}`, "allowDelete"},
		{BuiltinToolApplyPatch, `{"maxBytes": "big"}`, "integer"},
	}
	for _, tc := range cases {
		errs := ValidateBuiltinPolicy(tc.name, json.RawMessage(tc.policy))
		if len(errs) == 0 {
			t.Fatalf("%s %s: expected rejection, got none", tc.name, tc.policy)
		}
		found := false
		for _, e := range errs {
			if strings.Contains(e, tc.want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s %s: expected error containing %q, got %v", tc.name, tc.policy, tc.want, errs)
		}
	}
}

func TestFileToolsUnknownToolRejected(t *testing.T) {
	t.Parallel()
	errs := ValidateBuiltinPolicy("delete_everything", json.RawMessage(`{}`))
	if len(errs) != 1 || !strings.Contains(errs[0], "unknown built-in tool") {
		t.Fatalf("expected unknown tool rejection, got %v", errs)
	}
}

// S-282: the session inspection tools (sessions_list / sessions_history /
// session_status) ship enabled (migration 0058 seed), are known names, and
// carry no policy knobs — only "{}" validates (same posture as
// spawn_subagent), so a typo can never silently change behaviour.
func TestSessionInspectionToolsDefaultEnabled(t *testing.T) {
	t.Parallel()
	for _, name := range []string{BuiltinToolSessionsList, BuiltinToolSessionsHistory, BuiltinToolSessionStatus} {
		if !BuiltinToolDefaultEnabled(name) {
			t.Fatalf("%s should ship enabled by default (migration 0058 seed)", name)
		}
		if !IsBuiltinToolName(name) {
			t.Fatalf("%s missing from BuiltinToolNames", name)
		}
		if errs := ValidateBuiltinPolicy(name, json.RawMessage(`{}`)); len(errs) != 0 {
			t.Fatalf("%s: empty policy rejected: %v", name, errs)
		}
		if errs := ValidateBuiltinPolicy(name, json.RawMessage(`{"timeoutSeconds": 15}`)); len(errs) == 0 || !strings.Contains(errs[0], "unknown policy key") {
			t.Fatalf("%s: policy keys must be rejected (no knobs yet), got %v", name, errs)
		}
	}
}
