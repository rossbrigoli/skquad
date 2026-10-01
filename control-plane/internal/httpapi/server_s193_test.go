package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// S-193 epic: inbox & notifications endpoint coverage.

func TestS193SendInboxFromAgent(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s193-send")

	var owner domain.User
	doJSON(t, handler, http.MethodGet, pathAuthMe, nil, http.StatusOK, &owner)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Inbox me the report",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var created domain.InboxMessage
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/inbox", map[string]any{
		"message": "Here is the report you asked for: all green.",
		"subject": "Report",
		"task_id": task.ID,
	}, http.StatusCreated, &created)
	require.Equal(t, domain.InboxAgentMessage, created.Kind)
	require.Equal(t, owner.ID, created.UserID)
	require.Equal(t, squad.ID, created.SquadID)
	require.Equal(t, agent.ID, created.FromAgentID)
	require.Equal(t, task.ID, created.TaskID)
	require.Contains(t, created.Message, "all green")

	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, pathInbox, nil, http.StatusOK, &inbox)
	require.Len(t, inbox, 1)
	require.Equal(t, domain.InboxAgentMessage, inbox[0].Kind)

	// Empty message is rejected.
	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/inbox",
		map[string]any{"message": "   "}, http.StatusBadRequest)

	// A task outside the agent's squad is not linkable.
	var otherSquad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "s193-other"}, http.StatusCreated, &otherSquad)
	var foreignTask domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+otherSquad.ID+pathBoardTasks, map[string]any{"title": "foreign"}, http.StatusCreated, &foreignTask)
	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/inbox",
		map[string]any{"message": "link hijack", "task_id": foreignTask.ID}, http.StatusNotFound)
}

func TestS193InboxDeleteIsExplicitOnly(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s193-delete")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{"title": "msg carrier"}, http.StatusCreated, &task)
	var created domain.InboxMessage
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/inbox",
		map[string]any{"message": "temporary ping", "task_id": task.ID}, http.StatusCreated, &created)

	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, pathInbox, nil, http.StatusOK, &inbox)
	require.Len(t, inbox, 1)

	// Explicit delete by the recipient (the dev admin owns the squad here).
	doJSONNoBody(t, handler, http.MethodDelete, pathInbox+"/"+created.ID, nil, http.StatusNoContent)
	doJSON(t, handler, http.MethodGet, pathInbox, nil, http.StatusOK, &inbox)
	require.Empty(t, inbox)

	// Deleting again 404s — the row is gone, and nothing else removed it.
	doJSONNoBody(t, handler, http.MethodDelete, pathInbox+"/"+created.ID, nil, http.StatusNotFound)
}

func s193OIDCHandler(t *testing.T) (http.Handler, *storage.MemoryStore) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.IssuerURL = testIssuer
	cfg.Audience = "skquad"
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authAdmin: {Issuer: testIssuer, Subject: "admin-sub", Email: adminEmail, EmailVerified: true, Name: "Admin", Groups: []string{platformAdminGroup}},
		authAlice: {Issuer: testIssuer, Subject: "alice-sub", Email: aliceEmail, EmailVerified: true, Name: "Alice"},
	})
	return handler, store
}

func TestS193InboxAdminFilter(t *testing.T) {
	handler, store := s193OIDCHandler(t)
	ctx := context.Background()

	var alice domain.User
	doJSONAuth(t, handler, authAlice, http.MethodGet, pathAuthMe, nil, http.StatusOK, &alice)

	// Seed an inbox message for Alice directly (no agent plumbing needed).
	var created domain.Squad
	doJSONAuth(t, handler, authAdmin, http.MethodPost, pathSquads, map[string]any{"name": "s193-filter"}, http.StatusCreated, &created)
	squadID := created.ID
	_, err := store.CreateInboxMessage(ctx, &domain.InboxMessage{
		SquadID: squadID, UserID: alice.ID, Kind: domain.InboxAgentMessage, Message: "for alice only",
	})
	require.NoError(t, err)

	// Admin can view Alice's inbox via the filter.
	var asAdmin []domain.InboxMessage
	doJSONAuth(t, handler, authAdmin, http.MethodGet, pathInbox+"?user_id="+alice.ID, nil, http.StatusOK, &asAdmin)
	require.Len(t, asAdmin, 1)
	require.Equal(t, "for alice only", asAdmin[0].Message)

	// Alice sees her own by default and cannot peek via the filter.
	var asAlice []domain.InboxMessage
	doJSONAuth(t, handler, authAlice, http.MethodGet, pathInbox, nil, http.StatusOK, &asAlice)
	require.Len(t, asAlice, 1)
	doJSONAuth(t, handler, authAlice, http.MethodGet, pathInbox+"?user_id=admin", nil, http.StatusForbidden, &map[string]any{})

	// Notifications honour the same admin filter gate.
	_, err = store.CreateNotification(ctx, &domain.Notification{
		UserID: alice.ID, SquadID: squadID, Type: domain.NotificationTaskBlocked, Message: "alice task blocked",
	})
	require.NoError(t, err)
	var notesAdmin []domain.Notification
	doJSONAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/notifications?user_id="+alice.ID, nil, http.StatusOK, &notesAdmin)
	require.Len(t, notesAdmin, 1)
	doJSONAuth(t, handler, authAlice, http.MethodGet, "/api/v1/notifications?user_id=notalice", nil, http.StatusForbidden, &map[string]any{})
}

func TestS193BlockedTaskCreatesNotification(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s193-block")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Blocked work",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksClaim, nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/block", map[string]any{
		"summary":       "need credentials decision",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	var notes []domain.Notification
	doJSON(t, handler, http.MethodGet, "/api/v1/notifications", nil, http.StatusOK, &notes)
	require.Len(t, notes, 1)
	require.Equal(t, domain.NotificationTaskBlocked, notes[0].Type)
	require.Equal(t, domain.NotificationWarning, notes[0].Severity)
	require.Equal(t, task.ID, notes[0].TaskID)
	require.Contains(t, notes[0].Message, "requires attention")
	require.Contains(t, notes[0].Message, "T-1")
	require.False(t, notes[0].IsRead())

	// Unread filter + mark read + read-all.
	var unread []domain.Notification
	doJSON(t, handler, http.MethodGet, "/api/v1/notifications?unread=true", nil, http.StatusOK, &unread)
	require.Len(t, unread, 1)
	var read domain.Notification
	doJSON(t, handler, http.MethodPost, "/api/v1/notifications/"+notes[0].ID+"/read", nil, http.StatusOK, &read)
	require.True(t, read.IsRead())
	doJSON(t, handler, http.MethodGet, "/api/v1/notifications?unread=true", nil, http.StatusOK, &unread)
	require.Empty(t, unread)

	// A second blocked task then read-all clears everything.
	var task2 domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Second block",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task2)
	var claimed2 domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksClaim, nil, http.StatusOK, &claimed2)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task2.ID+"/block", map[string]any{
		"execution_id":  claimed2.ExecutionID,
		"fencing_token": claimed2.FencingToken,
	}, http.StatusOK, &domain.Task{})
	var marked map[string]int
	doJSON(t, handler, http.MethodPost, "/api/v1/notifications/read-all", nil, http.StatusOK, &marked)
	require.Equal(t, 1, marked["marked_read"])
	doJSON(t, handler, http.MethodGet, "/api/v1/notifications?unread=true", nil, http.StatusOK, &unread)
	require.Empty(t, unread)
}

func TestS193HeartbeatErrorTransitionFilesTaskFailed(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s193-fail")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Doomed work",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksClaim, nil, http.StatusOK, &claimed)

	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyHeartbeat, map[string]any{
		"status": string(domain.AgentError),
	}, http.StatusOK, &domain.Agent{})

	var notes []domain.Notification
	doJSON(t, handler, http.MethodGet, "/api/v1/notifications", nil, http.StatusOK, &notes)
	require.Len(t, notes, 1)
	require.Equal(t, domain.NotificationTaskFailed, notes[0].Type)
	require.Equal(t, domain.NotificationError, notes[0].Severity)
	require.Contains(t, notes[0].Message, "T-1")
	require.Contains(t, notes[0].Message, "reported an error")

	// Transition-gated: a second error heartbeat adds nothing.
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyHeartbeat, map[string]any{
		"status": string(domain.AgentError),
	}, http.StatusOK, &domain.Agent{})
	doJSON(t, handler, http.MethodGet, "/api/v1/notifications", nil, http.StatusOK, &notes)
	require.Len(t, notes, 1)
}

func TestS193ReaperFilesAgentDiedNotification(t *testing.T) {
	cfg := testConfig()
	store := storage.NewMemoryStore()
	handler := New(cfg, store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "s193-reaper"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "doomed-agent"}, http.StatusCreated, &agent)
	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Interrupted work",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	// Claim directly through the store, then reap with a cutoff past the lease.
	claimed, err := store.ClaimNextTask(context.Background(), agent.ID, "worker-1", time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, claimed.ExecutionID)
	reaped, err := store.ReapExpiredTaskExecutions(context.Background(), time.Now().Add(10*time.Minute))
	require.NoError(t, err)
	require.Len(t, reaped, 1)

	notifyReapedExecution(context.Background(), store, reaped[0])

	var notes []domain.Notification
	doJSON(t, handler, http.MethodGet, "/api/v1/notifications", nil, http.StatusOK, &notes)
	require.Len(t, notes, 1)
	require.Equal(t, domain.NotificationAgentDied, notes[0].Type)
	require.Equal(t, domain.NotificationError, notes[0].Severity)
	require.Contains(t, notes[0].Message, "died mid-task")
	require.Contains(t, notes[0].Message, "T-1")
	require.Equal(t, task.ID, notes[0].TaskID)
	require.Equal(t, agent.ID, notes[0].AgentID)
}
