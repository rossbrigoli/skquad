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
