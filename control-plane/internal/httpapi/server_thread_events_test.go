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
