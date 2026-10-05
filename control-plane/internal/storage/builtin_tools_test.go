package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// BT-2 (ADR-0012): storage semantics for builtin_tools_config.

func TestMemoryStoreSeedsBuiltinTools(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()

	tools, err := store.ListBuiltinTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// S-164 added send_message as a fourth builtin, seeded ENABLED; the
	// original three stay disabled-by-default (ADR-0012 §1). send_inbox
	// (S-193) and notify_owner (platform-prompt awareness) ship ENABLED
	// like send_message — bounded blast radius, own squad owner's inbox.
	// S-232 adds spawn_subagent so the runtime-provided subagent
	// capability is visible (and toggleable) on the Settings Tools page.
	if len(tools) != 8 {
		t.Fatalf("seeded tools = %d, want 8", len(tools))
	}
	wantNames := []string{"exec", "web_fetch", "web_search", "send_message", "send_inbox", "notify_owner", "memory_search", "spawn_subagent"}
	for i, tool := range tools {
		if tool.Name != wantNames[i] {
			t.Fatalf("tool[%d] = %q, want %q", i, tool.Name, wantNames[i])
		}
		wantEnabled := domain.BuiltinToolDefaultEnabled(tool.Name)
		if tool.Enabled != wantEnabled {
			t.Fatalf("tool %s enabled = %v, want %v", tool.Name, tool.Enabled, wantEnabled)
		}
		if string(tool.Policy) != "{}" {
			t.Fatalf("tool %s policy = %s, want {}", tool.Name, tool.Policy)
		}
	}
}

func TestMemoryStoreBuiltinToolUpdateMerge(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()

	enabled := true
	policy := json.RawMessage(`{"timeoutSeconds": 42}`)
	updated, err := store.UpdateBuiltinTool(ctx, "exec", &enabled, policy, "admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || string(updated.Policy) != `{"timeoutSeconds": 42}` || updated.UpdatedBy != "admin-1" {
		t.Fatalf("unexpected update result: %+v", updated)
	}

	// Merge: toggling enabled only must leave the stored policy untouched.
	disabled := false
	merged, err := store.UpdateBuiltinTool(ctx, "exec", &disabled, nil, "admin-2")
	if err != nil {
		t.Fatal(err)
	}
	if merged.Enabled {
		t.Fatal("enabled should be false after merge")
	}
	if string(merged.Policy) != `{"timeoutSeconds": 42}` {
		t.Fatalf("policy changed by merge: %s", merged.Policy)
	}
	if merged.UpdatedBy != "admin-2" {
		t.Fatalf("updated_by = %q, want admin-2", merged.UpdatedBy)
	}

	// Policy-only merge leaves enabled as-is.
	newPolicy := json.RawMessage(`{"timeoutSeconds": 99}`)
	policyOnly, err := store.UpdateBuiltinTool(ctx, "exec", nil, newPolicy, "admin-3")
	if err != nil {
		t.Fatal(err)
	}
	if policyOnly.Enabled {
		t.Fatal("enabled must remain false when only policy is merged")
	}
	if string(policyOnly.Policy) != `{"timeoutSeconds": 99}` {
		t.Fatalf("policy = %s", policyOnly.Policy)
	}
}

func TestMemoryStoreBuiltinToolUnknownName(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	if _, err := store.GetBuiltinTool(context.Background(), "teleport"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	enabled := true
	if _, err := store.UpdateBuiltinTool(context.Background(), "teleport", &enabled, nil, "x"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreBuiltinToolUpdateCarriesPendingAudit(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := WithPendingAudit(context.Background(), NewAuditEntry("user", "admin-9", "builtin_tools.update", "builtin_tools", "web_fetch", "", nil))
	enabled := true
	if _, err := store.UpdateBuiltinTool(ctx, "web_fetch", &enabled, nil, "admin-9"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListAudit(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "builtin_tools.update" && e.ActorID == "admin-9" && e.ResourceID == "web_fetch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending audit entry not recorded with the mutation: %+v", entries)
	}
}
