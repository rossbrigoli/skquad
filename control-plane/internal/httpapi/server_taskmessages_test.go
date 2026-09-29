package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func createBoardTask(t *testing.T, h http.Handler, squadID, title string) domain.Task {
	t.Helper()
	var task domain.Task
	doJSON(t, h, http.MethodPost, "/api/v1/squads/"+squadID+"/board/tasks", map[string]any{"title": title}, http.StatusCreated, &task)
	return task
}

func assignTask(t *testing.T, h http.Handler, taskID, agentID string) domain.Task {
	t.Helper()
	var task domain.Task
	doJSON(t, h, http.MethodPatch, pathTasksPrefix+taskID, map[string]any{"assignee_agent_id": agentID}, http.StatusOK, &task)
	return task
}

func TestTaskMessagesComposerAndThread(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	task := createBoardTask(t, h, f.squadID, "write the report")
	assignTask(t, h, task.ID, f.workerID)

	// Composer posts a user message to the assignee with task_id forced in.
	var sent domain.Message
	doJSON(t, h, http.MethodPost, pathTasksPrefix+task.ID+pathMessages, map[string]any{
		"message": "focus on the summary first",
	}, http.StatusCreated, &sent)
	require.Equal(t, f.workerID, sent.ToAgentID)
	require.Equal(t, f.squadID, sent.SquadID)
	require.Equal(t, "user", sent.FromType)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(sent.Payload, &payload))
	require.Equal(t, task.ID, payload["task_id"])
	require.Equal(t, "focus on the summary first", payload["message"])

	// Thread lists the task-scoped messages: the S-181 assign lifecycle
	// event first, then the composer message.
	var thread []domain.Message
	doJSON(t, h, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	require.Len(t, thread, 2)
	require.Equal(t, "delivered", string(thread[0].Status)) // lifecycle events are thread-only
	require.Contains(t, string(thread[0].Payload), "Task assigned to you")
	require.Equal(t, sent.ID, thread[1].ID)

	// A second task on the same assignee must not see this thread — only
	// its own lifecycle event.
	other := createBoardTask(t, h, f.squadID, "unrelated work")
	assignTask(t, h, other.ID, f.workerID)
	var otherThread []domain.Message
	doJSON(t, h, http.MethodGet, pathTasksPrefix+other.ID+pathMessages, nil, http.StatusOK, &otherThread)
	require.Len(t, otherThread, 1)
	require.Contains(t, string(otherThread[0].Payload), "unrelated work")

	// Empty message rejected.
	doJSONNoBody(t, h, http.MethodPost, pathTasksPrefix+task.ID+pathMessages, map[string]any{"message": "   "}, http.StatusBadRequest)
}

func TestTaskMessagesUnassigned(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	task := createBoardTask(t, h, f.squadID, "nobody on it")

	// GET on unassigned task returns an empty thread, not an error.
	var thread []domain.Message
	doJSON(t, h, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	require.Empty(t, thread)

	// POST to an unassigned task is rejected with a clear code.
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSON(t, h, http.MethodPost, pathTasksPrefix+task.ID+pathMessages, map[string]any{"message": "hello?"}, http.StatusBadRequest, &errResp)
	require.Equal(t, "no_assignee", errResp.Error.Code)
}

func TestTaskMessagesIncludesAgentThreadMessages(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	task := createBoardTask(t, h, f.squadID, "agent-reported thread")
	assignTask(t, h, task.ID, f.workerID)

	// Agent sends a message carrying the task_id in its payload.
	f.delegate(t, "reply", map[string]any{
		"payload": map[string]any{"message": "blocked on data", "task_id": task.ID},
	})

	var thread []domain.Message
	doJSON(t, h, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	// S-181: the assign lifecycle event opens the thread, then the
	// agent's own message.
	require.Len(t, thread, 2)
	require.Contains(t, string(thread[0].Payload), "Task assigned to you")
	require.Equal(t, f.senderID, thread[1].FromID)
	require.Contains(t, string(thread[1].Payload), "blocked on data")
}
