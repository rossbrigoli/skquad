package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func TestInboxTaskCompletionNotificationAndRead(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "inbox-squad")

	var owner domain.User
	doJSON(t, handler, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &owner)

	var empty []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &empty)
	require.Empty(t, empty)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Finish me",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        string(domain.TaskInReview),
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &inbox)
	require.Len(t, inbox, 1)
	require.Equal(t, domain.InboxTaskCompleted, inbox[0].Kind)
	require.Equal(t, owner.ID, inbox[0].UserID)
	require.Equal(t, agent.ID, inbox[0].FromAgentID)
	require.Equal(t, squad.ID, inbox[0].SquadID)
	require.Equal(t, task.ID, inbox[0].TaskID)
	require.Contains(t, inbox[0].Message, "Finish me")
	require.True(t, !inbox[0].IsRead())

	var unread []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, "/api/v1/inbox?unread=true", nil, http.StatusOK, &unread)
	require.Len(t, unread, 1)

	var read domain.InboxMessage
	doJSON(t, handler, http.MethodPost, "/api/v1/inbox/"+inbox[0].ID+"/read", nil, http.StatusOK, &read)
	require.True(t, read.IsRead())

	doJSON(t, handler, http.MethodGet, "/api/v1/inbox?unread=true", nil, http.StatusOK, &unread)
	require.Empty(t, unread)

	// Marking read again is idempotent.
	doJSON(t, handler, http.MethodPost, "/api/v1/inbox/"+inbox[0].ID+"/read", nil, http.StatusOK, &read)
	require.True(t, read.IsRead())
}

func TestInboxBlockedTaskRequestsAction(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "inbox-block")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Blocked work",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/block", map[string]any{
		"summary":       "need credentials",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &inbox)
	require.Len(t, inbox, 1)
	require.Equal(t, domain.InboxActionRequired, inbox[0].Kind)
	require.Contains(t, inbox[0].Message, "need credentials")
}

func TestInboxAgentNotifyOwner(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "inbox-notify")

	var owner domain.User
	doJSON(t, handler, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &owner)

	var created domain.InboxMessage
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/notify-owner", map[string]any{
		"message": "approve deploying to prod?",
	}, http.StatusCreated, &created)
	require.Equal(t, domain.InboxActionRequired, created.Kind)
	require.Equal(t, owner.ID, created.UserID)
	require.Equal(t, agent.ID, created.FromAgentID)
	require.Equal(t, squad.ID, created.SquadID)

	rec := doAgentRequest(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/notify-owner", map[string]any{"message": "   "})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &inbox)
	require.Len(t, inbox, 1)
}
