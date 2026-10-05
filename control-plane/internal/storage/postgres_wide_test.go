package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func TestPostgresWideRegistryPermissionsMeteringWakeAndTemplates(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if !hasUser(users, f.user.ID) {
		t.Fatalf("list users did not include fixture user %s", f.user.ID)
	}

	ownedSquads, err := store.ListSquads(ctx, f.user.ID)
	if err != nil {
		t.Fatalf("list owner squads: %v", err)
	}
	if !hasSquad(ownedSquads, f.squad.ID) {
		t.Fatalf("owner squad listing did not include %s", f.squad.ID)
	}
	allSquads, err := store.ListSquads(ctx, "")
	if err != nil {
		t.Fatalf("list all squads: %v", err)
	}
	if !hasSquad(allSquads, f.squad.ID) {
		t.Fatalf("all squad listing did not include %s", f.squad.ID)
	}

	identity, err := store.CreateAgentIdentity(ctx, &domain.AgentIdentity{
		AgentID:        f.agent.ID,
		CredentialRef:  "secret/ref-" + tag,
		CredentialHash: "hash-" + tag,
		VirtualKeyRef:  "vk-" + tag,
		CreatedBy:      f.user.ID,
	})
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if identity.GatewayKeyStatus != domain.GatewayKeyNone {
		t.Fatalf("gateway status = %q, want none", identity.GatewayKeyStatus)
	}
	rotated, err := store.RotateAgentIdentity(ctx, f.agent.ID, "secret/rotated-"+tag, "hash2-"+tag, "")
	if err != nil {
		t.Fatalf("rotate identity: %v", err)
	}
	if rotated.CredentialRef != "secret/rotated-"+tag || rotated.VirtualKeyRef != "" {
		t.Fatalf("rotated identity mismatch: %+v", rotated)
	}
	gateway, err := store.SetAgentIdentityGatewayKey(ctx, f.agent.ID, "token-"+tag, domain.GatewayKeyActive)
	if err != nil {
		t.Fatalf("set gateway key: %v", err)
	}
	if gateway.GatewayKeyStatus != domain.GatewayKeyActive {
		t.Fatalf("gateway status = %q, want active", gateway.GatewayKeyStatus)
	}
	allAgents, err := store.ListAllAgents(ctx)
	if err != nil {
		t.Fatalf("list all agents: %v", err)
	}
	if !hasAgent(allAgents, f.agent.ID) {
		t.Fatalf("all agent listing did not include %s", f.agent.ID)
	}

	grant, err := store.CreateGrant(ctx, &domain.AccessGrant{
		SquadID:     f.squad.ID,
		GranteeType: domain.GranteeAgent,
		GranteeID:   f.agent.ID,
		Permissions: "talk",
		GrantedBy:   f.user.ID,
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	gotGrant, err := store.GetGrant(ctx, grant.ID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if gotGrant.ID != grant.ID {
		t.Fatalf("grant id = %q, want %q", gotGrant.ID, grant.ID)
	}
	grants, err := store.ListGrants(ctx, f.squad.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grant count = %d, want 1", len(grants))
	}
	allowed, err := store.AgentMayMessageSquad(ctx, f.agent.ID, f.squad.ID, "ping")
	if err != nil {
		t.Fatalf("agent may message: %v", err)
	}
	if !allowed {
		t.Fatalf("talk grant should allow ping")
	}

	provider, err := store.CreateAIProvider(ctx, &domain.AIProvider{
		Name:         "wide-provider-" + tag,
		Kind:         "openai",
		BaseURL:      "https://models.example.test",
		APIKeyRef:    "secret/provider-" + tag,
		APIKeyMask:   "sk-..." + tag[len(tag)-4:],
		Status:       domain.ResourceActive,
		RegisteredBy: f.user.ID,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	provider.Name = "wide-provider-updated-" + tag
	provider.APIKeyMask = "sk-...wide"
	updatedProvider, err := store.UpdateAIProvider(ctx, provider)
	if err != nil {
		t.Fatalf("update provider: %v", err)
	}
	if updatedProvider.Name != provider.Name || updatedProvider.APIKeyMask != "sk-...wide" {
		t.Fatalf("updated provider mismatch: %+v", updatedProvider)
	}
	if err := store.DeprecateAIProvider(ctx, provider.ID); err != nil {
		t.Fatalf("deprecate provider: %v", err)
	}
	providers, err := store.ListAIProviders(ctx)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if !hasProvider(providers, provider.ID) {
		t.Fatalf("provider listing did not include %s", provider.ID)
	}

	resource, err := store.CreateResource(ctx, &domain.RegistryResource{
		Type:         domain.ResProjectWorkspace,
		Name:         "wide-workspace-" + tag,
		Description:  "workspace",
		Endpoint:     "https://git.example.test/repo.git",
		AuthRef:      "secret/git-" + tag,
		Manifest:     json.RawMessage(`{"branch":"main"}`),
		Status:       domain.ResourceActive,
		RegisteredBy: f.user.ID,
	})
	if err != nil {
		t.Fatalf("create resource: %v", err)
	}
	gotResource, err := store.GetResource(ctx, domain.ResProjectWorkspace, resource.ID)
	if err != nil {
		t.Fatalf("get resource: %v", err)
	}
	gotResource.Description = "updated workspace"
	gotResource.Manifest = json.RawMessage(`{"branch":"feature"}`)
	if _, err := store.UpdateResource(ctx, gotResource); err != nil {
		t.Fatalf("update resource: %v", err)
	}
	if err := store.DeprecateResource(ctx, domain.ResProjectWorkspace, resource.ID); err != nil {
		t.Fatalf("deprecate resource: %v", err)
	}
	resources, err := store.ListResources(ctx, domain.ResProjectWorkspace)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	if !hasResource(resources, resource.ID) {
		t.Fatalf("resource listing did not include %s", resource.ID)
	}
	if err := store.GrantAgentPermission(ctx, &domain.AgentPermission{
		AgentID:      f.agent.ID,
		ResourceType: domain.ResProjectWorkspace,
		ResourceID:   resource.ID,
		GrantedBy:    f.user.ID,
	}); err != nil {
		t.Fatalf("grant agent permission: %v", err)
	}
	perms, err := store.ListAgentPermissions(ctx, f.agent.ID)
	if err != nil {
		t.Fatalf("list agent permissions: %v", err)
	}
	if len(perms) != 1 {
		t.Fatalf("permission count = %d, want 1", len(perms))
	}
	byResource, err := store.ListPermissionsByResource(ctx, domain.ResProjectWorkspace, resource.ID)
	if err != nil {
		t.Fatalf("list permissions by resource: %v", err)
	}
	if len(byResource) != 1 {
		t.Fatalf("permissions by resource count = %d, want 1", len(byResource))
	}
	hasPerm, err := store.AgentHasPermission(ctx, f.agent.ID, domain.ResProjectWorkspace, resource.ID)
	if err != nil {
		t.Fatalf("agent has permission: %v", err)
	}
	if !hasPerm {
		t.Fatalf("agent should have workspace permission")
	}
	if err := store.RevokeAgentPermission(ctx, f.agent.ID, domain.ResProjectWorkspace, resource.ID); err != nil {
		t.Fatalf("revoke agent permission: %v", err)
	}
	if err := store.SetAgentPermissions(ctx, f.agent.ID, []domain.AgentPermission{{
		ResourceType: domain.ResProjectWorkspace,
		ResourceID:   resource.ID,
		GrantedBy:    f.user.ID,
	}}); err != nil {
		t.Fatalf("set agent permissions: %v", err)
	}

	task := f.newTask(t, store, "wide workspace "+tag)
	linkedTask, err := store.SetTaskWorkspace(ctx, task.ID, resource.ID, "feature/"+tag, "abcdef123456")
	if err != nil {
		t.Fatalf("set task workspace: %v", err)
	}
	if linkedTask.WorkspaceResourceID != resource.ID || linkedTask.WorkspaceCommitSHA != "abcdef123456" {
		t.Fatalf("workspace linkage mismatch: %+v", linkedTask)
	}

	inputRate := 1.25
	outputRate := 5.50
	if err := store.RecordMetering(ctx, &domain.MeteringEvent{
		AgentID:         f.agent.ID,
		SquadID:         f.squad.ID,
		TaskID:          task.ID,
		ProviderID:      provider.ID,
		Model:           testModel,
		InputTokens:     100,
		OutputTokens:    20,
		Cost:            0.42,
		Currency:        "",
		Timestamp:       time.Now().Add(-time.Minute),
		ModelUsed:       testModel,
		RateInputPer1M:  &inputRate,
		RateOutputPer1M: &outputRate,
		RateSnapshot:    true,
	}); err != nil {
		t.Fatalf("record metering: %v", err)
	}
	sum, err := store.SumMetering(ctx, f.squad.ID, f.agent.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("sum metering: %v", err)
	}
	if sum.InputTokens != 100 || sum.OutputTokens != 20 || sum.Currency != "USD" {
		t.Fatalf("metering sum mismatch: %+v", sum)
	}

	if _, err := store.pool.Exec(ctx, `
		UPDATE kubernetes_outbox
		SET status = 'applied', updated_at = now()
		WHERE aggregate_type = 'agent' AND aggregate_id = $1 AND operation = 'upsert_agent'
	`, f.agent.ID); err != nil {
		t.Fatalf("mark fixture outbox applied: %v", err)
	}
	latest, err := store.LatestAppliedAgentUpsert(ctx, f.agent.ID)
	if err != nil {
		t.Fatalf("latest applied agent upsert: %v", err)
	}
	if latest.AggregateID != f.agent.ID {
		t.Fatalf("latest aggregate id = %q, want %q", latest.AggregateID, f.agent.ID)
	}
	now := time.Now().UTC()
	wake := &domain.WakeLatencyEvent{
		AgentID:            f.agent.ID,
		SquadID:            f.squad.ID,
		TaskID:             task.ID,
		WakeRequestedAt:    now.Add(-4 * time.Second),
		CRAppliedAt:        now.Add(-3 * time.Second),
		ContainerStartedAt: now.Add(-2 * time.Second),
		ClaimedAt:          now,
		QueueMs:            1000,
		ScaleupMs:          1000,
		ClaimDelayMs:       1000,
		E2EMs:              4000,
		ColdStart:          true,
	}
	inserted, err := store.RecordWakeLatency(ctx, wake)
	if err != nil {
		t.Fatalf("record wake latency: %v", err)
	}
	if !inserted {
		t.Fatalf("wake latency insert reported duplicate")
	}
	duplicate, err := store.RecordWakeLatency(ctx, wake)
	if err != nil {
		t.Fatalf("record duplicate wake latency: %v", err)
	}
	if duplicate {
		t.Fatalf("duplicate wake latency should not insert")
	}
	wakes, err := store.ListWakeLatency(ctx, f.squad.ID, now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("list wake latency: %v", err)
	}
	if len(wakes) == 0 || wakes[0].AgentID != f.agent.ID {
		t.Fatalf("wake latency listing mismatch: %+v", wakes)
	}

	tpl, err := store.CreatePromptTemplate(ctx, &domain.PromptTemplate{
		Name:        "wide-template-" + tag,
		Description: "desc",
		Content:     "Prompt body",
		AppliesTo:   domain.PromptTemplateAppliesBoth,
		CreatedBy:   f.user.ID,
	})
	if err != nil {
		t.Fatalf("create prompt template: %v", err)
	}
	tpl.Content = "Updated body"
	gotTemplate, err := store.UpdatePromptTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("update prompt template: %v", err)
	}
	if gotTemplate.Content != "Updated body" {
		t.Fatalf("template content = %q, want updated", gotTemplate.Content)
	}
	templates, err := store.ListPromptTemplates(ctx)
	if err != nil {
		t.Fatalf("list prompt templates: %v", err)
	}
	if !hasPromptTemplate(templates, tpl.ID) {
		t.Fatalf("template listing did not include %s", tpl.ID)
	}
	if _, err := store.GetPromptTemplate(ctx, tpl.ID); err != nil {
		t.Fatalf("get prompt template: %v", err)
	}
	if err := store.DeletePromptTemplate(ctx, tpl.ID); err != nil {
		t.Fatalf("delete prompt template: %v", err)
	}
	if err := store.DeleteResource(ctx, domain.ResProjectWorkspace, resource.ID); err != nil {
		t.Fatalf("delete resource: %v", err)
	}
	if err := store.DeleteAIProvider(ctx, provider.ID); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
}

func TestPostgresWideMessagesInboxAndWait(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	hasWork, err := store.WaitForAgentWork(ctx, zeroUUID, time.Millisecond)
	if err != nil {
		t.Fatalf("wait without work: %v", err)
	}
	if hasWork {
		t.Fatalf("unknown agent should not have work")
	}

	task := f.newTask(t, store, "wide wait "+tag)
	hasWork, err = store.WaitForAgentWork(ctx, f.agent.ID, 0)
	if err != nil {
		t.Fatalf("wait with task work: %v", err)
	}
	if !hasWork {
		t.Fatalf("assigned todo task should count as ready work")
	}

	msg, err := store.CreateMessage(ctx, &domain.Message{
		FromType:    "user",
		FromID:      f.user.ID,
		ToAgentID:   f.agent.ID,
		SquadID:     f.squad.ID,
		Type:        domain.MessagePing,
		Payload:     json.RawMessage(`{"body":"hello"}`),
		MaxAttempts: 2,
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	pending, err := store.ListPendingMessages(ctx, f.agent.ID)
	if err != nil {
		t.Fatalf("list pending messages: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != msg.ID {
		t.Fatalf("pending messages = %+v, want %s", pending, msg.ID)
	}
	hasPending, err := store.HasPendingMessages(ctx, f.agent.ID)
	if err != nil {
		t.Fatalf("has pending messages: %v", err)
	}
	if !hasPending {
		t.Fatalf("expected pending messages")
	}

	correlated, err := store.CreateMessage(ctx, &domain.Message{
		FromType:      "agent",
		FromID:        f.agent.ID,
		ToAgentID:     f.agent.ID,
		SquadID:       f.squad.ID,
		Type:          domain.MessageReply,
		Payload:       json.RawMessage(`{"reply":"ok"}`),
		CorrelationID: msg.ID,
	})
	if err != nil {
		t.Fatalf("create correlated message: %v", err)
	}
	count, err := store.CountMessagesByCorrelation(ctx, msg.ID)
	if err != nil {
		t.Fatalf("count messages by correlation: %v", err)
	}
	if count != 1 {
		t.Fatalf("correlation count = %d, want 1", count)
	}
	if _, err := store.CountMessagesByCorrelation(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty correlation error = %v, want ErrInvalidInput", err)
	}

	acked, err := store.AckMessage(ctx, f.agent.ID, msg.ID)
	if err != nil {
		t.Fatalf("ack message: %v", err)
	}
	if acked.Status != domain.MessageDelivered || acked.DeliveredAt.IsZero() {
		t.Fatalf("acked message mismatch: %+v", acked)
	}
	if _, err := store.GetMessage(ctx, msg.ID); err != nil {
		t.Fatalf("get message: %v", err)
	}
	updated, err := store.UpdateMessagePayload(ctx, correlated.ID, json.RawMessage(`{"reply":"updated"}`), domain.MessageDelivered)
	if err != nil {
		t.Fatalf("update message payload: %v", err)
	}
	if updated.Status != domain.MessageDelivered {
		t.Fatalf("updated message status = %q, want delivered", updated.Status)
	}

	retryMsg, err := store.CreateMessage(ctx, &domain.Message{
		FromType:    "user",
		FromID:      f.user.ID,
		ToAgentID:   f.agent.ID,
		SquadID:     f.squad.ID,
		Type:        domain.MessagePing,
		Payload:     json.RawMessage(`{"body":"retry"}`),
		MaxAttempts: 2,
	})
	if err != nil {
		t.Fatalf("create retry message: %v", err)
	}
	retrying, err := store.FailMessage(ctx, f.agent.ID, retryMsg.ID, "try again")
	if err != nil {
		t.Fatalf("fail retry message: %v", err)
	}
	if retrying.Status != domain.MessagePending || retrying.Attempts != 1 {
		t.Fatalf("retrying message mismatch: %+v", retrying)
	}
	deadMsg, err := store.CreateMessage(ctx, &domain.Message{
		FromType:    "user",
		FromID:      f.user.ID,
		ToAgentID:   f.agent.ID,
		SquadID:     f.squad.ID,
		Type:        domain.MessagePing,
		Payload:     json.RawMessage(`{"body":"dead"}`),
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("create dead message: %v", err)
	}
	dead, err := store.FailMessage(ctx, f.agent.ID, deadMsg.ID, "permanent failure")
	if err != nil {
		t.Fatalf("fail dead message: %v", err)
	}
	if dead.Status != domain.MessageDead || dead.TerminalReason == "" {
		t.Fatalf("dead message mismatch: %+v", dead)
	}

	inbox, err := store.ListAgentInbox(ctx, f.agent.ID, 10)
	if err != nil {
		t.Fatalf("list agent inbox: %v", err)
	}
	if inbox.DeliveredCount < 2 || inbox.RetryingCount != 1 || inbox.DeadCount != 1 {
		t.Fatalf("inbox counts mismatch: %+v", inbox)
	}
	deadLetters, err := store.ListDeadLetters(ctx, domain.DeadLetterFilter{
		SquadID: f.squad.ID,
		AgentID: f.agent.ID,
		Reason:  "permanent",
		Limit:   5,
	})
	if err != nil {
		t.Fatalf("list dead letters: %v", err)
	}
	if len(deadLetters) != 1 || deadLetters[0].ID != dead.ID {
		t.Fatalf("dead letters = %+v, want %s", deadLetters, dead.ID)
	}
	replayed, err := store.ReplayDeadMessage(ctx, dead.ID)
	if err != nil {
		t.Fatalf("replay dead message: %v", err)
	}
	if replayed.Status != domain.MessagePending || replayed.Attempts != 0 {
		t.Fatalf("replayed message mismatch: %+v", replayed)
	}
	if err := store.DeleteMessage(ctx, replayed.ID); err != nil {
		t.Fatalf("delete message: %v", err)
	}

	archived, resetAt, err := store.ResetAgentChat(ctx, f.agent.ID, f.squad.ID, "transcript", json.RawMessage(`{"source":"test"}`), nil, "test-embed-model")
	if err != nil {
		t.Fatalf("reset agent chat: %v", err)
	}
	if archived == 0 || resetAt.IsZero() {
		t.Fatalf("reset archived=%d resetAt=%v, want archived messages", archived, resetAt)
	}
	history, err := store.ListAgentMessageHistory(ctx, f.agent.ID)
	if err != nil {
		t.Fatalf("list history after reset: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("history after reset = %d, want 0", len(history))
	}

	cancelMe, err := store.CreateMessage(ctx, &domain.Message{
		FromType:  "user",
		FromID:    f.user.ID,
		ToAgentID: f.agent.ID,
		SquadID:   f.squad.ID,
		Type:      domain.MessagePing,
		Payload:   json.RawMessage(`{"body":"cancel"}`),
	})
	if err != nil {
		t.Fatalf("create cancellable message: %v", err)
	}
	cancelled, err := store.CancelChatTurn(ctx, f.agent.ID)
	if err != nil {
		t.Fatalf("cancel chat turn: %v", err)
	}
	if cancelled.ID != cancelMe.ID || cancelled.Status != domain.MessageCancelled {
		t.Fatalf("cancelled message mismatch: %+v", cancelled)
	}

	asker, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: f.squad.ID,
		Name:    "asker-" + tag,
		Status:  domain.AgentIdle,
	})
	if err != nil {
		t.Fatalf("create asker agent: %v", err)
	}
	consult, err := store.CreateMessage(ctx, &domain.Message{
		FromType:  "agent",
		FromID:    asker.ID,
		ToAgentID: f.agent.ID,
		SquadID:   f.squad.ID,
		Type:      domain.MessageConsult,
		Payload:   json.RawMessage(`{"question":"still there?"}`),
	})
	if err != nil {
		t.Fatalf("create consult message: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE messages SET timeout_at = now() - interval '1 second' WHERE id = $1`, consult.ID); err != nil {
		t.Fatalf("set consult timeout: %v", err)
	}
	posted, err := store.SweepConsultTimeouts(ctx, time.Now())
	if err != nil {
		t.Fatalf("sweep consult timeouts: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted timeout replies = %d, want 1", posted)
	}

	gotTask, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task after message test: %v", err)
	}
	if gotTask.ID != task.ID {
		t.Fatalf("task id = %q, want %q", gotTask.ID, task.ID)
	}
}

func TestPostgresWideDeletePaths(t *testing.T) {
	store := postgresTestStore(t)
	f := newPGFixture(t, store)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: f.squad.ID,
		Name:    "delete-me-" + tag,
		Status:  domain.AgentIdle,
	})
	if err != nil {
		t.Fatalf("create delete agent: %v", err)
	}
	if err := store.DeleteAgent(ctx, agent.ID); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	if _, err := store.GetAgent(ctx, agent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted agent error = %v, want ErrNotFound", err)
	}

	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:    "delete-squad-" + tag,
		OwnerID: f.user.ID,
		Status:  domain.SquadActive,
	})
	if err != nil {
		t.Fatalf("create delete squad: %v", err)
	}
	if _, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID: squad.ID,
		Name:    "delete-squad-agent-" + tag,
		Status:  domain.AgentIdle,
	}); err != nil {
		t.Fatalf("create delete squad agent: %v", err)
	}
	if err := store.DeleteSquad(ctx, squad.ID); err != nil {
		t.Fatalf("delete squad: %v", err)
	}
	if _, err := store.GetSquad(ctx, squad.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted squad error = %v, want ErrNotFound", err)
	}
}

func hasUser(users []*domain.User, id string) bool {
	for _, u := range users {
		if u.ID == id {
			return true
		}
	}
	return false
}

func hasSquad(squads []*domain.Squad, id string) bool {
	for _, s := range squads {
		if s.ID == id {
			return true
		}
	}
	return false
}

func hasAgent(agents []*domain.Agent, id string) bool {
	for _, a := range agents {
		if a.ID == id {
			return true
		}
	}
	return false
}

func hasProvider(providers []*domain.AIProvider, id string) bool {
	for _, p := range providers {
		if p.ID == id {
			return true
		}
	}
	return false
}

func hasResource(resources []*domain.RegistryResource, id string) bool {
	for _, r := range resources {
		if r.ID == id {
			return true
		}
	}
	return false
}

func hasPromptTemplate(templates []*domain.PromptTemplate, id string) bool {
	for _, tpl := range templates {
		if tpl.ID == id {
			return true
		}
	}
	return false
}
