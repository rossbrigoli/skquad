package httpapi

// S-213: Backlog column. Backlog tasks are visible to humans on the board
// but invisible to every agent-facing pickup/listing path until a human
// moves them out. These tests pin that contract end-to-end through the API.

import (
	"net/http"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestBacklogTaskIsInvisibleToAgentPickupPaths(t *testing.T) {
	t.Parallel()

	handler, _, squad, agent, credential := agentRuntimeSetup(t, "Backlog Squad")

	// Human creates a task directly into Backlog, assigned to the agent.
	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Parked idea",
		"assignee_agent_id": agent.ID,
		"status":            "backlog",
	}, http.StatusCreated, &task)
	require.Equal(t, domain.TaskBacklog, task.Status)

	// The board (human view) shows it.
	var board struct {
		Board domain.Board   `json:"board"`
		Tasks []*domain.Task `json:"tasks"`
	}
	doJSON(t, handler, http.MethodGet, pathSquadsPrefix+squad.ID+pathBoard, nil, http.StatusOK, &board)
	require.Len(t, board.Tasks, 1)
	require.Equal(t, task.ID, board.Tasks[0].ID)

	// The agent-facing listing excludes it.
	var listed []*domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, "/api/v1/agents/me/tasks", nil, http.StatusOK, &listed)
	require.Empty(t, listed)

	// Claim serves nothing while the only assigned task sits in Backlog.
	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusNoContent)

	// Starting it by id is rejected with a dedicated 409.
	var body map[string]map[string]string
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/start", nil, http.StatusConflict, &body)
	require.Equal(t, "task_in_backlog", body["error"]["code"])

	// A human moves it out of Backlog — that move is the instruction.
	var moved domain.Task
	doJSON(t, handler, http.MethodPost, pathTasksPrefix+task.ID+"/move", map[string]any{"status": "todo"}, http.StatusOK, &moved)
	require.Equal(t, domain.TaskTodo, moved.Status)

	// Now the agent can see and claim it.
	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, "/api/v1/agents/me/tasks", nil, http.StatusOK, &listed)
	require.Len(t, listed, 1)
	require.Equal(t, task.ID, listed[0].ID)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	require.Equal(t, task.ID, claimed.ID)
	require.Equal(t, domain.TaskInProgress, claimed.Status)
}

func TestCreateTaskStatusValidation(t *testing.T) {
	t.Parallel()

	handler, _, squad, _, _ := agentRuntimeSetup(t, "Create Status Squad")

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":  "Nonsense column",
		"status": "icebox",
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])

	// Omitted status still defaults to todo.
	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title": "Classic",
	}, http.StatusCreated, &task)
	require.Equal(t, domain.TaskTodo, task.Status)
}

func TestBacklogMoveDoesNotWakeAgentBusy(t *testing.T) {
	t.Parallel()

	handler, _, squad, agent, _ := agentRuntimeSetup(t, "No Wake Squad")

	// Assign a backlog task directly, then move backlog -> backlog-adjacent
	// states that are not pickup states: the agent must not be marked busy.
	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Still parked",
		"assignee_agent_id": agent.ID,
		"status":            "backlog",
	}, http.StatusCreated, &task)

	var moved domain.Task
	doJSON(t, handler, http.MethodPost, pathTasksPrefix+task.ID+"/move", map[string]any{"status": "backlog"}, http.StatusOK, &moved)

	var a domain.Agent
	doJSON(t, handler, http.MethodGet, pathAgentsPrefix+agent.ID, nil, http.StatusOK, &a)
	require.NotEqual(t, domain.AgentBusy, a.Status)
}
