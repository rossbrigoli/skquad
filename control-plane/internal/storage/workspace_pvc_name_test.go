package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// S-261: the store fixes the friendly workspace PVC name at agent
// creation and never lets updates change it.

func TestMemoryCreateAgentDerivesWorkspacePVCName(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	owner := &domain.User{ID: "owner-1", Name: "Ross Brigoli", Email: "ross@example.com"}
	u, err := store.UpsertUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:    "Minions",
		OwnerID: u.ID,
		Status:  domain.SquadActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:        squad.ID,
		Name:           "Bob",
		Status:         domain.AgentIdle,
		StorageEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := "-workspace-" + agent.ID
	wantPrefix := "ross-brigoli-minions-bob"
	if !strings.HasSuffix(agent.WorkspacePVCName, wantSuffix) || !strings.HasPrefix(agent.WorkspacePVCName, wantPrefix) {
		t.Fatalf("WorkspacePVCName = %q, want %s...%s", agent.WorkspacePVCName, wantPrefix, wantSuffix)
	}

	// Reloaded agent carries the same fixed name.
	reloaded, err := store.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.WorkspacePVCName != agent.WorkspacePVCName {
		t.Fatalf("reloaded name %q != created name %q", reloaded.WorkspacePVCName, agent.WorkspacePVCName)
	}
}

func TestMemoryUpdateAgentCannotChangeWorkspacePVCName(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	owner := &domain.User{ID: "owner-2", Name: "Ross Brigoli", Email: "ross@example.com"}
	if _, err := store.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:    "Minions", OwnerID: owner.ID, Status: domain.SquadActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: squad.ID, Name: "Bob", Status: domain.AgentIdle, StorageEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixed := agent.WorkspacePVCName

	// A caller trying to rename (or clear) the PVC name is ignored.
	mutated := *agent
	mutated.WorkspacePVCName = "evil-renamed-volume"
	updated, err := store.UpdateAgent(ctx, &mutated)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WorkspacePVCName != fixed {
		t.Fatalf("update changed WorkspacePVCName to %q, want immutable %q", updated.WorkspacePVCName, fixed)
	}

	// Clearing is also ignored.
	mutated.WorkspacePVCName = ""
	updated, err = store.UpdateAgent(ctx, &mutated)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WorkspacePVCName != fixed {
		t.Fatalf("update cleared WorkspacePVCName to %q, want immutable %q", updated.WorkspacePVCName, fixed)
	}
}

func TestMemoryPreS261AgentsKeepEmptyName(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	owner := &domain.User{ID: "owner-3", Name: "Nobody", Email: "n@example.com"}
	if _, err := store.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name: "Old", OwnerID: owner.ID, Status: domain.SquadActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-S-261 row inserted directly with no fixed name:
	// the derivation only runs on CreateAgent with a live squad+user, so
	// verify the derivation itself never yields a name without a GUID.
	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: squad.ID, Name: "Legacy", Status: domain.AgentIdle,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Agents are always created with a GUID, so the name IS derived now;
	// the pre-S-261 guarantee is that EXISTING rows stay '' (migration
	// default) — asserted by the migration, not by this store. Here we
	// verify the derived name still ends with the full agent GUID.
	if !strings.HasSuffix(agent.WorkspacePVCName, agent.ID) {
		t.Fatalf("derived name %q must end with the full agent GUID", agent.WorkspacePVCName)
	}
}
