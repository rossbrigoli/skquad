package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// TestTaskThreadLifecycleEvents pins the S-181 contract: task lifecycle
// events (create-with-assignee, agent completion, agent block) are
// materialized as thread-only messages (status=delivered, so they never
// enter the agent's pending inbox) addressed to the assignee with
// task_id in the payload — while the owner's inbox notifications stay.
func TestTaskThreadLifecycleEvents(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "thread-events-squad")

	// Create with assignee → the thread opens with the creation event.
	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Lifecycle audit",
		"description":       "check the logs",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var thread []domain.Message
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	require.Len(t, thread, 1)
	require.Equal(t, "user", thread[0].FromType)
	require.Equal(t, agent.ID, thread[0].ToAgentID)
	require.Equal(t, "delivered", string(thread[0].Status))
	require.Contains(t, string(thread[0].Payload), "Lifecycle audit")
	require.Contains(t, string(thread[0].Payload), "check the logs")

	// Agent completes with a summary → the agent's turn appears in the
	// thread as a delivered reply.
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        string(domain.TaskInReview),
		"summary":       "all logs reviewed, nothing anomalous",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	require.Len(t, thread, 2)
	require.Equal(t, "agent", thread[1].FromType)
	require.Equal(t, agent.ID, thread[1].ToAgentID)
	require.Equal(t, "delivered", string(thread[1].Status))
	require.Contains(t, string(thread[1].Payload), "all logs reviewed, nothing anomalous")

	// The owner still got the inbox notification (both, not either).
	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, pathInbox, nil, http.StatusOK, &inbox)
	require.NotEmpty(t, inbox)
	require.Equal(t, domain.InboxTaskCompleted, inbox[0].Kind)
	require.Equal(t, task.ID, inbox[0].TaskID)
}

// TestTaskThreadEmptySummarySurfaced pins the control-plane half of S-182:
// a completion with no summary must still leave an explicit thread entry
// instead of vanishing.
func TestTaskThreadEmptySummarySurfaced(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "thread-empty-summary-squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Silent finish",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        string(domain.TaskInReview),
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	var thread []domain.Message
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	require.Len(t, thread, 2)
	require.Equal(t, "agent", thread[1].FromType)
	require.Contains(t, string(thread[1].Payload), "without a summary")
}

// TestTaskThreadTurnStreaming pins the S-183 contract: the assigned agent
// can stream working turns into its task thread via
// POST /api/v1/agents/me/tasks/{id}/thread; the turn lands as a
// delivered agent reply carrying task_id, non-assignees are rejected,
// and empty/missing bodies are 400s.
func TestTaskThreadTurnStreaming(t *testing.T) {
	handler, crWriter, squad, agent, credential := agentRuntimeSetup(t, "thread-streaming-squad")

	// Second agent in the same squad — must not be able to write to agent 1's thread.
	var other domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "other-streamer"}, http.StatusCreated, &other)
	var otherIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+other.ID+pathIdentity, nil, http.StatusCreated, &otherIdentity)
	otherCredential := crWriter.credentialTokens[otherIdentity.CredentialRef]

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Streamed task",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	// Empty / missing bodies are rejected.
	var emptyErr map[string]map[string]string
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/thread", map[string]any{"message": "   "}, http.StatusBadRequest, &emptyErr)
	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/thread", nil, http.StatusBadRequest)

	// Non-assignee is forbidden.
	var forbidden map[string]map[string]string
	doAgentJSON(t, handler, other.ID, otherCredential, http.MethodPost, pathMyTasksPrefix+task.ID+"/thread", map[string]any{"message": "sneaky"}, http.StatusForbidden, &forbidden)
	require.Equal(t, "not_assignee", forbidden["error"]["code"])

	// Assignee streams a working turn → thread gains a delivered agent reply.
	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/thread", map[string]any{"message": "checking the runtime API surface"}, http.StatusNoContent)

	var thread []domain.Message
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	require.Len(t, thread, 2) // creation event + streamed turn
	require.Equal(t, "agent", thread[1].FromType)
	require.Equal(t, agent.ID, thread[1].ToAgentID)
	require.Equal(t, "delivered", string(thread[1].Status))
	require.Contains(t, string(thread[1].Payload), "checking the runtime API surface")
	require.Contains(t, string(thread[1].Payload), task.ID)
}
