package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// TestTaskResultAndRichInboxOnComplete pins the S-181 contract for task
// completion: a done task persists a dedicated Result field, and the
// owner's inbox notification carries an email-style subject and body
// with a link to the task screen (while the legacy one-line message stays).
func TestTaskResultAndRichInboxOnComplete(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s181-complete-squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Ship the report",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksClaim, nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+pathComplete, map[string]any{
		"status":        string(domain.TaskDone),
		"summary":       "Report shipped: covered Q3 numbers and attached the charts.",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	var got domain.Task
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID, nil, http.StatusOK, &got)
	require.Equal(t, domain.TaskDone, got.Status)
	require.Equal(t, "Report shipped: covered Q3 numbers and attached the charts.", got.Result)
	require.Equal(t, string(domain.TaskDone), got.ResultStatus)
	require.False(t, got.ResultAt.IsZero())

	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, pathInbox, nil, http.StatusOK, &inbox)
	require.NotEmpty(t, inbox)
	msg := inbox[0]
	require.Equal(t, domain.InboxTaskCompleted, msg.Kind)
	require.Equal(t, task.ID, msg.TaskID)
	require.Contains(t, msg.Subject, `Task "Ship the report" completed:`)
	require.Contains(t, msg.Subject, "Report shipped")
	require.Contains(t, msg.Body, "Task: Ship the report")
	require.Contains(t, msg.Body, "Status: done")
	require.Contains(t, msg.Body, "Agent: "+agent.Name)
	require.Contains(t, msg.Body, "When: ")
	require.Contains(t, msg.Body, "/squads/"+squad.ID+"/tasks/"+task.ID)
	// Backward compatibility: the legacy one-line message is still there.
	require.Contains(t, msg.Message, "moved task")
}

// TestTaskResultAndRichInboxOnBlock pins the S-181 contract for blocked
// tasks: the blocked reason is persisted as the task Result and the
// inbox subject reads "Task ... is blocked because of ...".
func TestTaskResultAndRichInboxOnBlock(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s181-block-squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Deploy the thing",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksClaim, nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+pathBlock, map[string]any{
		"summary":       "registry credentials expired",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	var got domain.Task
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID, nil, http.StatusOK, &got)
	require.Equal(t, domain.TaskBlocked, got.Status)
	require.Equal(t, "registry credentials expired", got.Result)
	require.Equal(t, string(domain.TaskBlocked), got.ResultStatus)
	require.False(t, got.ResultAt.IsZero())

	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, pathInbox, nil, http.StatusOK, &inbox)
	require.NotEmpty(t, inbox)
	msg := inbox[0]
	require.Equal(t, domain.InboxActionRequired, msg.Kind)
	require.Contains(t, msg.Subject, `Task "Deploy the thing" is blocked because of registry credentials expired`)
	require.Contains(t, msg.Body, "Status: blocked")
	require.Contains(t, msg.Body, "registry credentials expired")
	require.Contains(t, msg.Body, "/squads/"+squad.ID+"/tasks/"+task.ID)
}

// TestTaskInReviewLeavesResultEmpty pins that in-review completions do
// not populate the dedicated Result field (S-181 scopes it to
// done/blocked); the UI falls back to the thread for those.
func TestTaskInReviewLeavesResultEmpty(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s181-inreview-squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Reviewable work",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksClaim, nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+pathComplete, map[string]any{
		"status":        string(domain.TaskInReview),
		"summary":       "ready for review",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	var got domain.Task
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID, nil, http.StatusOK, &got)
	require.Equal(t, domain.TaskInReview, got.Status)
	require.Empty(t, got.Result)
	require.Empty(t, got.ResultStatus)
	require.True(t, got.ResultAt.IsZero())
}

// TestBlockedWithoutReasonSubjectFallback pins that a block with no
// reason still produces a readable subject.
func TestBlockedWithoutReasonSubjectFallback(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "s181-block-empty-squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Mystery block",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksClaim, nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+pathBlock, map[string]any{
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	var inbox []domain.InboxMessage
	doJSON(t, handler, http.MethodGet, pathInbox, nil, http.StatusOK, &inbox)
	require.NotEmpty(t, inbox)
	require.Contains(t, inbox[0].Subject, "is blocked because of no reason was given")
	require.Contains(t, inbox[0].Body, "(no summary provided)")
}

// TestTaskNotifySubjectBodyTruncation checks the helper caps runaway
// summaries so subjects stay email-subject sized.
func TestTaskNotifySubjectBodyTruncation(t *testing.T) {
	task := &domain.Task{ID: "t1", SquadID: "s1", Title: "Long"}
	huge := strings.Repeat("x", 500)
	subject, body := taskNotifySubjectBody(task, "agent", string(domain.TaskBlocked), huge)
	require.LessOrEqual(t, len([]rune(subject)), maxInboxSubjectChars)
	require.LessOrEqual(t, len([]rune(body)), maxInboxBodyChars)
	require.True(t, strings.HasPrefix(subject, `Task "Long" is blocked because of xxx`))
}
