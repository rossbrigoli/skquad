// S-290 (ADR-0016 D2+D3): agent-decided review on complete and the
// assignee-scoped fenced reopen with a rolling-24h cap.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

type s290Fixture struct {
	handler    http.Handler
	store      *storage.MemoryStore
	crWriter   *fakeCRWriter
	squad      domain.Squad
	agent      domain.Agent
	credential string
}

func newS290Fixture(t *testing.T, name string) *s290Fixture {
	t.Helper()
	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": name}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": name + "-agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)
	return &s290Fixture{handler: handler, store: store, crWriter: crWriter, squad: squad, agent: agent, credential: credential}
}

// addSquadMate creates a second agent in the fixture's squad and returns
// (agentID, credential) for cross-agent guard tests.
func (f *s290Fixture) addSquadMate(t *testing.T, name string) (string, string) {
	t.Helper()
	var mate domain.Agent
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squad.ID+pathAgents, map[string]any{"name": name}, http.StatusCreated, &mate)
	var identity domain.AgentIdentity
	doJSON(t, f.handler, http.MethodPost, pathAgentsPrefix+mate.ID+pathIdentity, nil, http.StatusCreated, &identity)
	cred := f.crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, cred)
	return mate.ID, cred
}

// newTask creates a task assigned to the fixture agent in the given column
// (default todo, matching the create default).
func (f *s290Fixture) newTask(t *testing.T, title string, status domain.TaskStatus) domain.Task {
	t.Helper()
	body := map[string]any{"title": title, "assignee_agent_id": f.agent.ID}
	if status != "" {
		body["status"] = string(status)
	}
	var task domain.Task
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squad.ID+pathBoardTasks, body, http.StatusCreated, &task)
	return task
}

func (f *s290Fixture) claim(t *testing.T) domain.Task {
	t.Helper()
	var claimed domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, pathMyTasksClaim, nil, http.StatusOK, &claimed)
	require.NotEmpty(t, claimed.ExecutionID)
	require.NotEmpty(t, claimed.FencingToken)
	return claimed
}

func (f *s290Fixture) complete(t *testing.T, taskID string, body map[string]any) domain.Task {
	t.Helper()
	var updated domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, pathMyTasksPrefix+taskID+pathComplete, body, http.StatusOK, &updated)
	return updated
}

func (f *s290Fixture) fenceBody(executionID, fencingToken string, extra map[string]any) map[string]any {
	body := map[string]any{"execution_id": executionID, "fencing_token": fencingToken}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (f *s290Fixture) block(t *testing.T, c domain.Task, summary string) {
	t.Helper()
	doAgentJSONNoBody(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+c.ID+pathBlock, f.fenceBody(c.ExecutionID, c.FencingToken, map[string]any{"summary": summary}), http.StatusOK)
}

func (f *s290Fixture) getTask(t *testing.T, taskID string) domain.Task {
	t.Helper()
	var task domain.Task
	doJSON(t, f.handler, http.MethodGet, pathTasksPrefix+taskID, nil, http.StatusOK, &task)
	return task
}

func (f *s290Fixture) setBoardPolicy(t *testing.T, policy map[string]any) {
	t.Helper()
	var resp map[string]any
	doJSON(t, f.handler, http.MethodPut, pathSquadsPrefix+f.squad.ID+"/board-policy", policy, http.StatusOK, &resp)
}

// decodeErrCode extracts error.code from an error response recorder.
func decodeErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Error.Code
}

// ---------------------------------------------------------------------------
// D2 — complete with review_needed
// ---------------------------------------------------------------------------

func TestS290CompleteReviewNeededNilAndTrueLandInReview(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S290 Review Default")

	// nil review_needed (today's body shape) → in-review.
	task := f.newTask(t, "nil review_needed", "")
	claimed := f.claim(t)
	updated := f.complete(t, task.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, nil))
	require.Equal(t, domain.TaskInReview, updated.Status)

	// explicit true → in-review.
	task2 := f.newTask(t, "true review_needed", "")
	claimed2 := f.claim(t)
	updated2 := f.complete(t, task2.ID, f.fenceBody(claimed2.ExecutionID, claimed2.FencingToken, map[string]any{"review_needed": true}))
	require.Equal(t, domain.TaskInReview, updated2.Status)
}

func TestS290CompleteSelfApprovalRequiresJustification(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S290 Justification")

	task := f.newTask(t, "no justification", "")
	claimed := f.claim(t)
	rec := doAgentRequest(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+task.ID+pathComplete,
		f.fenceBody(claimed.ExecutionID, claimed.FencingToken, map[string]any{"review_needed": false}))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "self_approval_justification_required", decodeErrCode(t, rec))

	// Nothing applied: task still in-progress, execution still active.
	require.Equal(t, domain.TaskInProgress, f.getTask(t, task.ID).Status)
}

func TestS290CompleteSelfApprovalDoneAndAudited(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S290 SelfApprove")

	task := f.newTask(t, "trivial fix", "")
	claimed := f.claim(t)
	updated := f.complete(t, claimed.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, map[string]any{
		"review_needed": false,
		"justification": "typo-only change, unit tests green, no side effects",
		"summary":       "fixed typo in README",
	}))
	require.Equal(t, domain.TaskDone, updated.Status)
	// S-181 result mechanism unchanged: summary stored with ResultStatus=done.
	require.Equal(t, "fixed typo in README", updated.Result)
	require.Equal(t, string(domain.TaskDone), updated.ResultStatus)

	// Audit trail carries self_approved + justification.
	var audit []domain.AuditEntry
	doJSON(t, f.handler, http.MethodGet, pathSquadsPrefix+f.squad.ID+pathAudit, nil, http.StatusOK, &audit)
	var found bool
	for _, entry := range audit {
		if entry.Action != "task.complete" || entry.ResourceID != task.ID {
			continue
		}
		found = true
		var meta map[string]any
		require.NoError(t, json.Unmarshal(entry.Metadata, &meta))
		require.Equal(t, true, meta["self_approved"])
		require.Equal(t, "typo-only change, unit tests green, no side effects", meta["justification"])
	}
	require.True(t, found, "expected a task.complete audit entry for the self-approved completion")
}

func TestS290CompleteSelfApprovalDisabledByPolicy(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S290 Policy Off")
	f.setBoardPolicy(t, map[string]any{"self_approve_default": false})

	task := f.newTask(t, "policy says no", "")
	claimed := f.claim(t)
	rec := doAgentRequest(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+task.ID+pathComplete,
		f.fenceBody(claimed.ExecutionID, claimed.FencingToken, map[string]any{
			"review_needed": false,
			"justification": "wants to self-approve anyway",
		}))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "self_approval_disabled", decodeErrCode(t, rec))

	// Nothing applied: task stays in-progress with its live execution.
	require.Equal(t, domain.TaskInProgress, f.getTask(t, task.ID).Status)
}

// ---------------------------------------------------------------------------
// D3 — reopen
// ---------------------------------------------------------------------------

func TestS290ReopenHappyPath(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S290 Reopen")

	task := f.newTask(t, "needs another pass", "")
	claimed := f.claim(t)
	f.complete(t, claimed.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, nil))
	require.Equal(t, domain.TaskInReview, f.getTask(t, task.ID).Status)

	var reopened domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+task.ID+"/reopen",
		map[string]any{"reason": "reviewer flagged missing tests", "reviewer_feedback": "add a regression test for the cap path", "worker_id": "worker-2"},
		http.StatusOK, &reopened)
	require.Equal(t, domain.TaskInProgress, reopened.Status)
	// New attempt: different execution + fencing token, live lease.
	require.NotEmpty(t, reopened.ExecutionID)
	require.NotEmpty(t, reopened.FencingToken)
	require.NotEqual(t, claimed.ExecutionID, reopened.ExecutionID)
	require.NotEqual(t, claimed.FencingToken, reopened.FencingToken)
	require.True(t, reopened.LeaseExpiresAt.After(time.Now()))
	// Reviewer feedback surfaced on the payload for the task brief.
	require.Equal(t, "add a regression test for the cap path", reopened.ReviewerFeedback)

	// Prior completed attempt retained; new attempt is the reopen-created one.
	execs, err := f.store.ListTaskExecutions(context.Background(), task.ID)
	require.NoError(t, err)
	require.Len(t, execs, 2)
	require.Equal(t, domain.TaskExecutionCompleted, execs[0].Status)
	require.Equal(t, domain.TaskInReview, execs[0].ResultStatus)
	require.False(t, execs[0].ReopenSeq)
	require.Equal(t, domain.TaskExecutionActive, execs[1].Status)
	require.True(t, execs[1].ReopenSeq)
	require.Equal(t, "worker-2", execs[1].WorkerID)
	require.Equal(t, "add a regression test for the cap path", execs[1].ReviewerFeedback)

	// Start on the resumed task surfaces the feedback + live fence details.
	var started domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+task.ID+"/start", nil, http.StatusOK, &started)
	require.Equal(t, reopened.ExecutionID, started.ExecutionID)
	require.Equal(t, "add a regression test for the cap path", started.ReviewerFeedback)

	// Audit trail records the reopen with its reason.
	var audit []domain.AuditEntry
	doJSON(t, f.handler, http.MethodGet, pathSquadsPrefix+f.squad.ID+pathAudit, nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "task.reopen")
}

func TestS290ReopenGuards(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S290 Reopen Guards")

	// Empty reason → 400 before anything else.
	task := f.newTask(t, "empty reason", "")
	claimed := f.claim(t)
	f.complete(t, claimed.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, nil))
	rec := doAgentRequest(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+task.ID+"/reopen", map[string]any{"reason": "   "})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "reopen_reason_required", decodeErrCode(t, rec))

	// Not the assignee (same squad, different agent) → 409 reopen_not_allowed.
	mateID, mateCred := f.addSquadMate(t, "squad-mate")
	rec = doAgentRequest(t, f.handler, mateID, mateCred, http.MethodPost,
		pathMyTasksPrefix+task.ID+"/reopen", map[string]any{"reason": "want it back"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "reopen_not_allowed", decodeErrCode(t, rec))

	// Wrong status: todo / backlog / in-progress / blocked → reopen_wrong_status.
	// Fresh fixture per status so claim never picks a sibling task.
	for _, status := range []domain.TaskStatus{domain.TaskTodo, domain.TaskBacklog, domain.TaskInProgress, domain.TaskBlocked} {
		w := newS290Fixture(t, "S290 Wrong "+string(status))
		createAs := status
		if status == domain.TaskBlocked {
			createAs = domain.TaskTodo // driven into blocked via the lifecycle
		}
		wrong := w.newTask(t, "wrong "+string(status), createAs)
		if status == domain.TaskInProgress || status == domain.TaskBlocked {
			c := w.claim(t)
			require.Equal(t, wrong.ID, c.ID)
			if status == domain.TaskBlocked {
				w.block(t, c, "stuck")
				require.Equal(t, domain.TaskBlocked, w.getTask(t, wrong.ID).Status)
			}
		}
		rec = doAgentRequest(t, w.handler, w.agent.ID, w.credential, http.MethodPost,
			pathMyTasksPrefix+wrong.ID+"/reopen", map[string]any{"reason": "nope"})
		require.Equal(t, http.StatusConflict, rec.Code, "status "+string(status)+": "+rec.Body.String())
		require.Equal(t, "reopen_wrong_status", decodeErrCode(t, rec), "status "+string(status))
	}

	// done → reopen_final_status (D4: never agent-reopenable).
	doneTask := f.newTask(t, "approved done", "")
	c := f.claim(t)
	f.complete(t, doneTask.ID, f.fenceBody(c.ExecutionID, c.FencingToken, map[string]any{"status": string(domain.TaskDone), "summary": "shipped"}))
	rec = doAgentRequest(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+doneTask.ID+"/reopen", map[string]any{"reason": "changed my mind"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "reopen_final_status", decodeErrCode(t, rec))
	require.Equal(t, domain.TaskDone, f.getTask(t, doneTask.ID).Status)
}

func TestS290ReopenCapExceeded(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S290 Reopen Cap")
	f.setBoardPolicy(t, map[string]any{"reopen_cap_per_day": 1})

	task := f.newTask(t, "cap me", "")
	claimed := f.claim(t)
	f.complete(t, claimed.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, nil))

	// First reopen (cap=1 → one allowed) succeeds.
	reopened := f.reopenOK(t, task.ID, "first reopen")
	require.Equal(t, domain.TaskInProgress, reopened.Status)

	// Complete again into review using the reopen fence, second reopen
	// within 24h → 429.
	f.complete(t, reopened.ID, f.fenceBody(reopened.ExecutionID, reopened.FencingToken, nil))
	require.Equal(t, domain.TaskInReview, f.getTask(t, task.ID).Status)
	rec := doAgentRequest(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+task.ID+"/reopen", map[string]any{"reason": "second reopen"})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Equal(t, "reopen_cap_exceeded", decodeErrCode(t, rec))

	// Card stays in-review — the loop is broken for the agent.
	require.Equal(t, domain.TaskInReview, f.getTask(t, task.ID).Status)
}

func (f *s290Fixture) reopenOK(t *testing.T, taskID, reason string) domain.Task {
	t.Helper()
	var reopened domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost,
		pathMyTasksPrefix+taskID+"/reopen", map[string]any{"reason": reason}, http.StatusOK, &reopened)
	return reopened
}

// ---------------------------------------------------------------------------
// Transition matrix: all 6 columns × {complete(true), complete(false), reopen}
// for the agent actor (task always assigned to the calling agent, default
// policy: self-approve allowed, cap 3).
// ---------------------------------------------------------------------------

func TestS290AgentTransitionMatrix(t *testing.T) {
	t.Parallel()

	type expectation struct {
		wantStatus int
		wantCode   string // "" for 2xx
		wantColumn domain.TaskStatus
	}
	// action → expectation per starting column.
	matrix := map[domain.TaskStatus]map[string]expectation{
		domain.TaskBacklog: {
			"complete_true":  {http.StatusConflict, "conflict", domain.TaskBacklog},
			"complete_false": {http.StatusConflict, "conflict", domain.TaskBacklog},
			"reopen":         {http.StatusConflict, "reopen_wrong_status", domain.TaskBacklog},
		},
		domain.TaskTodo: {
			"complete_true":  {http.StatusConflict, "conflict", domain.TaskTodo},
			"complete_false": {http.StatusConflict, "conflict", domain.TaskTodo},
			"reopen":         {http.StatusConflict, "reopen_wrong_status", domain.TaskTodo},
		},
		domain.TaskInProgress: {
			"complete_true":  {http.StatusOK, "", domain.TaskInReview},
			"complete_false": {http.StatusOK, "", domain.TaskDone},
			"reopen":         {http.StatusConflict, "reopen_wrong_status", domain.TaskInProgress},
		},
		domain.TaskInReview: {
			"complete_true":  {http.StatusConflict, "conflict", domain.TaskInReview},
			"complete_false": {http.StatusConflict, "conflict", domain.TaskInReview},
			"reopen":         {http.StatusOK, "", domain.TaskInProgress},
		},
		domain.TaskBlocked: {
			"complete_true":  {http.StatusConflict, "conflict", domain.TaskBlocked},
			"complete_false": {http.StatusConflict, "conflict", domain.TaskBlocked},
			"reopen":         {http.StatusConflict, "reopen_wrong_status", domain.TaskBlocked},
		},
		domain.TaskDone: {
			"complete_true":  {http.StatusConflict, "conflict", domain.TaskDone},
			"complete_false": {http.StatusConflict, "conflict", domain.TaskDone},
			"reopen":         {http.StatusConflict, "reopen_final_status", domain.TaskDone},
		},
	}

	for _, start := range []domain.TaskStatus{domain.TaskBacklog, domain.TaskTodo, domain.TaskInProgress, domain.TaskInReview, domain.TaskBlocked, domain.TaskDone} {
		for _, action := range []string{"complete_true", "complete_false", "reopen"} {
			want := matrix[start][action]
			t.Run(string(start)+"/"+action, func(t *testing.T) {
				f := newS290Fixture(t, "S290 Matrix "+string(start)+" "+action)
				// Only backlog is created directly in-column; the other
				// columns are driven there through the lifecycle so the
				// claim/fence setup is realistic.
				createAs := start
				if createAs != domain.TaskBacklog {
					createAs = domain.TaskTodo
				}
				task := f.newTask(t, "matrix card", createAs)
				// Drive the task into the starting column with live fences
				// where the column implies one.
				fenceExecution, fenceToken := "exec-none", "fence-none"
				switch start {
				case domain.TaskInProgress:
					c := f.claim(t)
					task, fenceExecution, fenceToken = c, c.ExecutionID, c.FencingToken
				case domain.TaskInReview:
					c := f.claim(t)
					f.complete(t, c.ID, f.fenceBody(c.ExecutionID, c.FencingToken, nil))
				case domain.TaskBlocked:
					c := f.claim(t)
					f.block(t, c, "stuck")
				case domain.TaskDone:
					c := f.claim(t)
					f.complete(t, c.ID, f.fenceBody(c.ExecutionID, c.FencingToken, map[string]any{"status": string(domain.TaskDone), "summary": "shipped"}))
				}

				var path string
				var body map[string]any
				switch action {
				case "complete_true":
					path = pathMyTasksPrefix + task.ID + pathComplete
					body = f.fenceBody(fenceExecution, fenceToken, map[string]any{"review_needed": true, "summary": "done working"})
				case "complete_false":
					path = pathMyTasksPrefix + task.ID + pathComplete
					body = f.fenceBody(fenceExecution, fenceToken, map[string]any{"review_needed": false, "justification": "self-evident, tests green"})
				case "reopen":
					path = pathMyTasksPrefix + task.ID + "/reopen"
					body = map[string]any{"reason": "matrix reopen"}
				}

				rec := doAgentRequest(t, f.handler, f.agent.ID, f.credential, http.MethodPost, path, body)
				require.Equal(t, want.wantStatus, rec.Code, rec.Body.String())
				if want.wantCode != "" {
					require.Equal(t, want.wantCode, decodeErrCode(t, rec))
				}
				require.Equal(t, want.wantColumn, f.getTask(t, task.ID).Status)
			})
		}
	}
}
