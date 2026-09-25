package kube

import (
	"context"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

type capturingOutboxWriter struct {
	agents []*domain.Agent
}

func (w *capturingOutboxWriter) UpsertSquad(context.Context, *domain.Squad) error { return nil }
func (w *capturingOutboxWriter) DeleteSquad(context.Context, *domain.Squad) error { return nil }
func (w *capturingOutboxWriter) DeleteAgent(context.Context, *domain.Agent) error { return nil }
func (w *capturingOutboxWriter) UpsertAgent(_ context.Context, agent *domain.Agent, _ *domain.AgentIdentity) error {
	w.agents = append(w.agents, agent)
	return nil
}

func workspaceResource(name, manifest, authRef string, status domain.ResourceStatus) *domain.RegistryResource {
	return &domain.RegistryResource{
		Type:     domain.ResProjectWorkspace,
		Name:     name,
		Manifest: []byte(manifest),
		AuthRef:  authRef,
		Status:   status,
	}
}

func TestProcessOutboxOnceDerivesWorkspaceSecrets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := storage.NewMemoryStore()
	user, err := store.UpsertUser(ctx, &domain.User{Email: "ws-worker@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name: "WS Squad", OwnerID: user.ID, Namespace: "squad-ws",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "Coder"})
	if err != nil {
		t.Fatal(err)
	}

	gitWS, err := store.CreateResource(ctx, workspaceResource(
		"repo-app", `{"kind":"git","default_branch":"main"}`,
		"k8s://squad-ws/ws-repo-app-token", domain.ResourceActive,
	))
	if err != nil {
		t.Fatal(err)
	}
	deprecatedWS, err := store.CreateResource(ctx, workspaceResource(
		"repo-old", `{"kind":"git","default_branch":"main"}`,
		"k8s://squad-ws/ws-repo-old-token", domain.ResourceDeprecated,
	))
	if err != nil {
		t.Fatal(err)
	}
	nonGitWS, err := store.CreateResource(ctx, workspaceResource(
		"bucket", `{"kind":"bucket"}`,
		"k8s://squad-ws/ws-bucket-token", domain.ResourceActive,
	))
	if err != nil {
		t.Fatal(err)
	}
	badRefWS, err := store.CreateResource(ctx, workspaceResource(
		"repo-badauth", `{"kind":"git","default_branch":"main"}`,
		"gateway://whatever/x", domain.ResourceActive,
	))
	if err != nil {
		t.Fatal(err)
	}

	// Drain create-time events.
	writer := &capturingOutboxWriter{}
	if _, err := ProcessOutboxOnce(ctx, store, writer); err != nil {
		t.Fatal(err)
	}

	// Grant: 1 good + deprecated + non-git + bad auth ref + stale (missing resource).
	if err := store.SetAgentPermissions(ctx, agent.ID, []domain.AgentPermission{
		{ResourceType: domain.ResProjectWorkspace, ResourceID: gitWS.ID, GrantedBy: user.ID},
		{ResourceType: domain.ResProjectWorkspace, ResourceID: deprecatedWS.ID, GrantedBy: user.ID},
		{ResourceType: domain.ResProjectWorkspace, ResourceID: nonGitWS.ID, GrantedBy: user.ID},
		{ResourceType: domain.ResProjectWorkspace, ResourceID: badRefWS.ID, GrantedBy: user.ID},
		{ResourceType: domain.ResProjectWorkspace, ResourceID: "deleted-resource-id", GrantedBy: user.ID},
		{ResourceType: domain.ResLLMProvider, ResourceID: gitWS.ID, GrantedBy: user.ID},
	}); err != nil {
		t.Fatal(err)
	}

	writer = &capturingOutboxWriter{}
	processed, err := ProcessOutboxOnce(ctx, store, writer)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 {
		t.Fatalf("processed = %d, want 1 (SetAgentPermissions must enqueue agent upsert)", processed)
	}
	if len(writer.agents) != 1 {
		t.Fatalf("writer agents = %d, want 1", len(writer.agents))
	}
	got := writer.agents[0].WorkspaceSecrets
	want := []domain.WorkspaceSecret{{ResourceID: gitWS.ID, SecretName: "ws-repo-app-token"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("workspace secrets = %+v, want %+v", got, want)
	}
}

func TestProcessOutboxOnceAgentWithoutGrantsGetsEmptySecrets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := storage.NewMemoryStore()
	user, err := store.UpsertUser(ctx, &domain.User{Email: "ws-none@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name: "NoWS Squad", OwnerID: user.ID, Namespace: "squad-nows",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "NoWS"})
	if err != nil {
		t.Fatal(err)
	}
	// Drain create-time events before the permission change.
	if _, err := ProcessOutboxOnce(ctx, store, &capturingOutboxWriter{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentPermissions(ctx, agent.ID, nil); err != nil {
		t.Fatal(err)
	}

	writer := &capturingOutboxWriter{}
	if _, err := ProcessOutboxOnce(ctx, store, writer); err != nil {
		t.Fatal(err)
	}
	if len(writer.agents) != 1 {
		t.Fatalf("writer agents = %d, want 1", len(writer.agents))
	}
	if len(writer.agents[0].WorkspaceSecrets) != 0 {
		t.Fatalf("workspace secrets = %+v, want empty", writer.agents[0].WorkspaceSecrets)
	}
}
