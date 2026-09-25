package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// delegationFixture creates a squad with two agents (sender + worker) and
// returns credentials so tests can drive the delegate/handoff loop.
type delegationFixture struct {
	handler    http.Handler
	store      *storage.MemoryStore
	squadID    string
	senderID   string
	workerID   string
	senderCred string
	workerCred string
}

func newDelegationFixture(t *testing.T) *delegationFixture {
	t.Helper()
	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Delegation Squad"}, http.StatusCreated, &squad)
	var sender domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "Coordinator"}, http.StatusCreated, &sender)
	var worker domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "Worker"}, http.StatusCreated, &worker)

	var senderID domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+sender.ID+"/identity", nil, http.StatusCreated, &senderID)
	var workerID domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+worker.ID+"/identity", nil, http.StatusCreated, &workerID)

	return &delegationFixture{
		handler:    handler,
		store:      store,
		squadID:    squad.ID,
		senderID:   sender.ID,
		workerID:   worker.ID,
		senderCred: crWriter.credentialTokens[senderID.CredentialRef],
		workerCred: crWriter.credentialTokens[workerID.CredentialRef],
	}
}

func (f *delegationFixture) delegate(t *testing.T, messageType string, body map[string]any) domain.Message {
	t.Helper()
	body["to_agent_id"] = f.workerID
	body["type"] = messageType
	var sent domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, "/api/v1/agents/me/messages", body, http.StatusCreated, &sent)
	return sent
}

func (f *delegationFixture) boardTasks(t *testing.T) []domain.Task {
	t.Helper()
	var board struct {
		Tasks []domain.Task `json:"tasks"`
	}
	doJSON(t, f.handler, http.MethodGet, "/api/v1/squads/"+f.squadID+"/board", nil, http.StatusOK, &board)
	return board.Tasks
}

func (f *delegationFixture) claim(t *testing.T) domain.Task {
	t.Helper()
	var task domain.Task
	doAgentJSON(t, f.handler, f.workerID, f.workerCred, http.MethodPost, "/api/v1/agents/me/tasks/claim", map[string]any{
		"worker_id": "w-1", "lease_seconds": 60,
	}, http.StatusOK, &task)
	return task
}

func (f *delegationFixture) complete(t *testing.T, task domain.Task, summary string) {
	t.Helper()
	doAgentJSONNoBody(t, f.handler, f.workerID, f.workerCred, http.MethodPost,
		"/api/v1/agents/me/tasks/"+task.ID+"/complete",
		map[string]any{
			"execution_id":  task.ExecutionID,
			"fencing_token": task.FencingToken,
			"status":        "done",
			"summary":       summary,
		}, http.StatusOK)
}

func TestDelegateMaterializesTask(t *testing.T) {
	f := newDelegationFixture(t)

	sent := f.delegate(t, "delegate", map[string]any{
		"message": "Draft the onboarding email\nMore context here",
	})

	// Message is recorded as delivered immediately: the task, not the
	// message, drives the worker.
	require.Equal(t, domain.MessageDelivered, sent.Status)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(sent.Payload, &payload))
	taskID, ok := payload["task_id"].(string)
	require.True(t, ok, "payload must carry the materialized task id")

	tasks := f.boardTasks(t)
	require.Len(t, tasks, 1)
	task := tasks[0]
	require.Equal(t, taskID, task.ID)
	require.Equal(t, "Draft the onboarding email", task.Title, "title = first line of the delegated message")
	require.Equal(t, domain.TaskTodo, task.Status)
	require.Equal(t, f.workerID, task.AssigneeAgentID)
	require.Equal(t, "agent", task.CreatedByType)
	require.Equal(t, f.senderID, task.CreatedByID)
	require.Equal(t, sent.ID, task.OriginMessageID)

	// Worker is woken by the task and can claim it.
	claimed := f.claim(t)
	require.Equal(t, task.ID, claimed.ID)
}

func TestHandoffMaterializesTaskWithExplicitTitle(t *testing.T) {
	f := newDelegationFixture(t)

	sent := f.delegate(t, "handoff", map[string]any{
		"title":   "Review Q3 budget",
		"message": "Full details in the attachment",
	})
	require.Equal(t, domain.MessageDelivered, sent.Status)

	tasks := f.boardTasks(t)
	require.Len(t, tasks, 1)
	require.Equal(t, "Review Q3 budget", tasks[0].Title)
	require.Contains(t, tasks[0].Description, "Full details in the attachment")
	require.Contains(t, tasks[0].Description, sent.ID)
}

func TestDelegatedTaskCompletionNotifiesRequester(t *testing.T) {
	f := newDelegationFixture(t)

	sent := f.delegate(t, "delegate", map[string]any{"message": "Summarize the incident"})
	task := f.claim(t)
	require.NotEmpty(t, task.OriginMessageID)
	require.Equal(t, sent.ID, task.OriginMessageID)

	f.complete(t, task, "Incident summarized: 3 root causes")

	// The requesting agent receives a reply correlated to the original
	// delegate message.
	var inbox []*domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodGet, "/api/v1/agents/me/messages", nil, http.StatusOK, &inbox)
	require.Len(t, inbox, 1)
	reply := inbox[0]
	require.Equal(t, domain.MessageReply, reply.Type)
	require.Equal(t, "agent", reply.FromType)
	require.Equal(t, f.workerID, reply.FromID)
	require.Equal(t, sent.ID, reply.CorrelationID)
	require.Contains(t, string(reply.Payload), "Incident summarized")

	// The requesting squad's owner sees the completion in the inbox.
	var ownerInbox []domain.InboxMessage
	doJSON(t, f.handler, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &ownerInbox)
	found := false
	for _, msg := range ownerInbox {
		if msg.TaskID == task.ID && msg.Kind == domain.InboxTaskCompleted {
			found = true
		}
	}
	require.True(t, found, "owner inbox must carry the delegated completion")
}

func TestDelegatedTaskBlockedNotifiesRequester(t *testing.T) {
	f := newDelegationFixture(t)

	f.delegate(t, "delegate", map[string]any{"message": "Needs credentials"})
	task := f.claim(t)

	doAgentJSONNoBody(t, f.handler, f.workerID, f.workerCred, http.MethodPost,
		"/api/v1/agents/me/tasks/"+task.ID+"/block",
		map[string]any{
			"execution_id":  task.ExecutionID,
			"fencing_token": task.FencingToken,
			"summary":       "missing data source access",
		}, http.StatusOK)

	var ownerInbox []domain.InboxMessage
	doJSON(t, f.handler, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &ownerInbox)
	found := false
	for _, msg := range ownerInbox {
		if msg.TaskID == task.ID && msg.Kind == domain.InboxActionRequired {
			found = true
		}
	}
	require.True(t, found, "owner inbox must carry the blocked-delegation alert")
}

func TestNonDelegateMessagesStillQueue(t *testing.T) {
	f := newDelegationFixture(t)

	sent := f.delegate(t, "consult", map[string]any{"message": "what does the registry say?"})
	require.Equal(t, domain.MessagePending, sent.Status, "consult messages still queue normally")

	tasks := f.boardTasks(t)
	require.Empty(t, tasks, "consult must not materialize tasks")
}

func TestCrossSquadDelegateRespectsGrants(t *testing.T) {
	f := newDelegationFixture(t)

	// Create a second squad with a worker agent; delegation without a grant
	// must be denied (existing authz path, asserted for delegate type).
	var squad2 domain.Squad
	doJSON(t, f.handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Other Squad"}, http.StatusCreated, &squad2)
	var otherWorker domain.Agent
	doJSON(t, f.handler, http.MethodPost, "/api/v1/squads/"+squad2.ID+"/agents", map[string]any{"name": "Foreign Worker"}, http.StatusCreated, &otherWorker)

	var denied domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, "/api/v1/agents/me/messages", map[string]any{
		"to_agent_id": otherWorker.ID,
		"type":        "delegate",
		"message":     "unauthorized work",
	}, http.StatusForbidden, &denied)
}
