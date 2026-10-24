// S-292 (ADR-0016 D1): board.* builtin backing endpoints — agent task
// creation (backlog default), self-assign, the claim-fence creation on
// start, and the assignee-side request-changes return-to-pool.
package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/stretchr/testify/require"
)

// requestChanges calls the endpoint and returns the raw recorder for error asserts.
func (f *s290Fixture) requestChanges(t *testing.T, taskID string, body map[string]any) (domain.Task, *httptest.ResponseRecorder) {
	t.Helper()
	var updated domain.Task
	rec := doAgentRequest(t, f.handler, f.agent.ID, f.credential, http.MethodPost, pathMyTasksPrefix+taskID+"/request-changes", body)
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	}
	return updated, rec
}

// ---------------------------------------------------------------------------
// PART A — POST /agents/me/tasks/{id}/request-changes
// ---------------------------------------------------------------------------

func TestS292RequestChangesHappyPath(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 RC")

	task := f.newTask(t, "return me", "")
	claimed := f.claim(t)
	f.complete(t, task.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, nil))

	updated, rec := f.requestChanges(t, task.ID, map[string]any{"comment": "needs retry logic"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, domain.TaskTodo, updated.Status)
	require.Empty(t, updated.AssigneeAgentID, "assignee must be cleared back to the pool")

	// Audited as task.request_changes with the comment.
	var audit []domain.AuditEntry
	doJSON(t, f.handler, http.MethodGet, pathSquadsPrefix+f.squad.ID+pathAudit+"?limit=50", nil, http.StatusOK, &audit)
	var found *domain.AuditEntry
	for i := range audit {
		if audit[i].Action == "task.request_changes" {
			found = &audit[i]
			break
		}
	}
	require.NotNil(t, found, "task.request_changes audit entry expected")
	require.Equal(t, "agent", found.ActorType)
	require.Equal(t, f.agent.ID, found.ActorID)
	var meta map[string]string
	require.NoError(t, json.Unmarshal(found.Metadata, &meta))
	require.Equal(t, "needs retry logic", meta["comment"])
}

func TestS292RequestChangesWrongStatus409(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 RC Status")

	task := f.newTask(t, "still todo", "") // todo, not in-review
	_, rec := f.requestChanges(t, task.ID, map[string]any{"comment": "too early"})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "request_changes_wrong_status", decodeErrCode(t, rec))
}

func TestS292RequestChangesNotAssignee403(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 RC Assignee")

	task := f.newTask(t, "not yours", "")
	claimed := f.claim(t)
	f.complete(t, task.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, nil))

	mateID, mateCred := f.addSquadMate(t, "mate")
	var updated domain.Task
	doAgentJSON(t, f.handler, mateID, mateCred, http.MethodPost,
		pathMyTasksPrefix+task.ID+"/request-changes", map[string]any{"comment": "mine now"},
		http.StatusForbidden, &updated)

	// State untouched: still in-review with the original assignee.
	got := f.getTask(t, task.ID)
	require.Equal(t, domain.TaskInReview, got.Status)
	require.Equal(t, f.agent.ID, got.AssigneeAgentID)
}

func TestS292RequestChangesRequiresComment(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 RC Comment")

	task := f.newTask(t, "needs comment", "")
	claimed := f.claim(t)
	f.complete(t, task.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, nil))

	_, rec := f.requestChanges(t, task.ID, map[string]any{"comment": "   "})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "request_changes_comment_required", decodeErrCode(t, rec))
}

// ---------------------------------------------------------------------------
// board.create_task — POST /agents/me/tasks
// ---------------------------------------------------------------------------

func TestS292CreateMyTaskLandsInBacklog(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 Create")

	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "agent-authored", "description": "from the tool"},
		http.StatusCreated, &created)
	require.Equal(t, domain.TaskBacklog, created.Status)
	require.Equal(t, "agent", created.CreatedByType)
	require.Equal(t, f.agent.ID, created.CreatedByID)
	require.Equal(t, f.squad.ID, created.SquadID)
}

func TestS292CreateMyTaskReadyRequiresLeaderCapability(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 Create Ready")

	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "urgent", "ready": true},
		http.StatusForbidden, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "leader_capability_required", errObj["code"])
}

func TestS292CreateMyTaskRejectsForeignSquadAndMissingTitle(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 Create Guards")

	var other struct {
		ID string `json:"id"`
	}
	doJSON(t, f.handler, http.MethodPost, pathSquads, map[string]any{"name": "S292 Other Squad"}, http.StatusCreated, &other)

	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "wrong squad", "squad_id": other.ID},
		http.StatusBadRequest, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "bad_request", errObj["code"])

	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "   "}, http.StatusBadRequest, &body)
}

// ---------------------------------------------------------------------------
// board.assign — PATCH /agents/me/tasks/{id}/assignee
// ---------------------------------------------------------------------------

func TestS292AssignSelfAllowed(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 Assign")

	// Create via the agent path (backlog, unassigned), then self-assign.
	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "mine to take"}, http.StatusCreated, &created)

	var updated domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+created.ID+"/assignee", map[string]any{"assignee_agent_id": f.agent.ID},
		http.StatusOK, &updated)
	require.Equal(t, f.agent.ID, updated.AssigneeAgentID)
}

func TestS292AssignOtherRequiresLeaderCapability(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 Assign Other")

	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "delegate me"}, http.StatusCreated, &created)
	mateID, _ := f.addSquadMate(t, "mate")

	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+created.ID+"/assignee", map[string]any{"assignee_agent_id": mateID},
		http.StatusForbidden, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "leader_capability_required", errObj["code"])
}

func TestS292AssignForeignSquadTaskForbidden(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S292 Assign Foreign")

	// A task in another squad is invisible to this agent's assign verb.
	var otherSquad domain.Squad
	doJSON(t, f.handler, http.MethodPost, pathSquads, map[string]any{"name": "S292 Foreign Squad"}, http.StatusCreated, &otherSquad)
	var otherAgent domain.Agent
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+otherSquad.ID+pathAgents, map[string]any{"name": "foreign-agent"}, http.StatusCreated, &otherAgent)
	var otherTask domain.Task
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+otherSquad.ID+pathBoardTasks,
		map[string]any{"title": "not in my squad", "assignee_agent_id": otherAgent.ID}, http.StatusCreated, &otherTask)

	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+otherTask.ID+"/assignee", map[string]any{"assignee_agent_id": f.agent.ID},
		http.StatusForbidden, &body)
}

// ---------------------------------------------------------------------------
// board.claim — start mints the fence when no prior claim exists
// ---------------------------------------------------------------------------

func TestS292StartWithoutPriorClaimMintsFence(t *testing.T) {
	t.Parallel()
	f := new292FixtureWithTask(t)

	// No /tasks/claim round trip: straight start (the board.claim path).
	var started domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+f.taskID+"/start", nil, http.StatusOK, &started)
	require.Equal(t, domain.TaskInProgress, started.Status)
	require.NotEmpty(t, started.ExecutionID, "start must mint an execution for the board.claim path")
	require.NotEmpty(t, started.FencingToken, "start must surface the fence to the owning agent")

	// The minted fence completes the task cleanly.
	updated := f.complete(t, f.taskID, f.fenceBody(started.ExecutionID, started.FencingToken, nil))
	require.Equal(t, domain.TaskInReview, updated.Status)
}

func TestS292StartSecondTaskWhileActiveConflicts(t *testing.T) {
	t.Parallel()
	f := new292FixtureWithTask(t)

	// Occupy the agent with task A via the normal claim path.
	occupied := f.claim(t)
	require.Equal(t, f.taskID, occupied.ID)

	// Starting a DIFFERENT task while A is active must conflict and leave
	// the second card untouched.
	second := f.newTask(t, "second startable", "")
	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+second.ID+"/start", nil, http.StatusConflict, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "already_active_task", errObj["code"])
	got := f.getTask(t, second.ID)
	require.Equal(t, domain.TaskTodo, got.Status, "conflicted start must not move the card")
}

func new292FixtureWithTask(t *testing.T) *s292Fixture {
	t.Helper()
	base := newS290Fixture(t, "S292 Start")
	task := base.newTask(t, "startable", "")
	return &s292Fixture{s290Fixture: base, taskID: task.ID}
}

type s292Fixture struct {
	*s290Fixture
	taskID string
}
