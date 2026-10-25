// S-294 (ADR-0016 D5/D8): leader capabilities — can_prioritize unlocks
// board.create_task(ready=true) into TO DO, can_assign_others unlocks
// board.assign of OTHER squad members' tasks (reusing the same assign +
// wake machinery), and leader_approval_allowed lets a leader-capable
// agent move others' in-review tasks to done. Defaults: no grants, no
// leader approval — strictly human.
package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/stretchr/testify/require"
)

// grantLeader sets the squad board-policy leader_grants for the given
// agent via the owner API. NOTE: PUT replaces the whole document — any
// later setBoardPolicy call must re-include grants it wants to keep.
func (f *s290Fixture) grantLeader(t *testing.T, agentID string, canPrioritize, canAssignOthers bool) {
	t.Helper()
	f.setBoardPolicy(t, map[string]any{
		"leader_grants": map[string]any{
			agentID: map[string]any{"can_prioritize": canPrioritize, "can_assign_others": canAssignOthers},
		},
	})
}

// grantLeaderWithApproval sets grants AND leader_approval_allowed in
// one document (the PUT-replaces-whole-doc semantics bites otherwise).
func (f *s290Fixture) grantLeaderWithApproval(t *testing.T, agentID string, approval bool) {
	t.Helper()
	f.setBoardPolicy(t, map[string]any{
		"leader_grants": map[string]any{
			agentID: map[string]any{"can_prioritize": true, "can_assign_others": true},
		},
		"leader_approval_allowed": approval,
	})
}

func (f *s290Fixture) revokeLeader(t *testing.T) {
	t.Helper()
	f.setBoardPolicy(t, map[string]any{})
}

// ---------------------------------------------------------------------------
// can_prioritize — board.create_task(ready=true)
// ---------------------------------------------------------------------------

func TestS294CreateReadyAllowedWithCanPrioritize(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Prioritize")
	f.grantLeader(t, f.agent.ID, true, false)

	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "leader urgent", "ready": true}, http.StatusCreated, &created)
	require.Equal(t, domain.TaskTodo, created.Status, "ready=true with can_prioritize lands in TO DO")
	require.Equal(t, f.agent.ID, created.CreatedByID)

	// Audit carries the ready flag.
	var audit []domain.AuditEntry
	doJSON(t, f.handler, http.MethodGet, pathSquadsPrefix+f.squad.ID+pathAudit+"?limit=50", nil, http.StatusOK, &audit)
	var found *domain.AuditEntry
	for i := range audit {
		if audit[i].Action == "task.create" {
			found = &audit[i]
			break
		}
	}
	require.NotNil(t, found)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(found.Metadata, &meta))
	require.Equal(t, true, meta["ready"])
}

func TestS294CreateReadyDeniedWithoutGrantAndAfterRevoke(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Prioritize Deny")

	// Default: no grant → 403 (same posture as S-292 baseline).
	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "not yet", "ready": true}, http.StatusForbidden, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "leader_capability_required", errObj["code"])

	// Grant, then revoke: the denial must come back immediately (no cache).
	f.grantLeader(t, f.agent.ID, true, false)
	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "granted window", "ready": true}, http.StatusCreated, &created)
	require.Equal(t, domain.TaskTodo, created.Status)

	f.revokeLeader(t)
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "revoked", "ready": true}, http.StatusForbidden, &body)

	// Backlog default (ready omitted) is untouched by the revoke.
	var backlogged domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "still fine"}, http.StatusCreated, &backlogged)
	require.Equal(t, domain.TaskBacklog, backlogged.Status)
}

func TestS294CreateReadyDeniedWithAssignOnlyGrant(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Prioritize Granular")
	f.grantLeader(t, f.agent.ID, false, true) // assign-only leader

	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "wrong grant", "ready": true}, http.StatusForbidden, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "leader_capability_required", errObj["code"])
}

// ---------------------------------------------------------------------------
// can_assign_others — board.assign(other_agent)
// ---------------------------------------------------------------------------

func TestS294AssignOtherAllowedWithCanAssignOthers(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Assign Others")
	mateID, _ := f.addSquadMate(t, "mate")
	f.grantLeader(t, f.agent.ID, false, true)

	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "plan step 2"}, http.StatusCreated, &created)

	var updated domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+created.ID+"/assignee", map[string]any{"assignee_agent_id": mateID},
		http.StatusOK, &updated)
	require.Equal(t, mateID, updated.AssigneeAgentID)
}

func TestS294AssignOtherDeniedAfterRevoke(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Assign Revoke")
	mateID, _ := f.addSquadMate(t, "mate")
	f.grantLeader(t, f.agent.ID, true, true)

	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "assign window"}, http.StatusCreated, &created)
	var updated domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+created.ID+"/assignee", map[string]any{"assignee_agent_id": mateID},
		http.StatusOK, &updated)

	f.revokeLeader(t)
	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+created.ID+"/assignee", map[string]any{"assignee_agent_id": f.agent.ID},
		http.StatusOK, &updated) // self-assign still fine
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+created.ID+"/assignee", map[string]any{"assignee_agent_id": mateID},
		http.StatusForbidden, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "leader_capability_required", errObj["code"])
}

func TestS294AssignOtherDeniedWithPrioritizeOnlyGrant(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Assign Granular")
	mateID, _ := f.addSquadMate(t, "mate")
	f.grantLeader(t, f.agent.ID, true, false) // prioritize-only leader

	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "plan step 3"}, http.StatusCreated, &created)

	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+created.ID+"/assignee", map[string]any{"assignee_agent_id": mateID},
		http.StatusForbidden, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "leader_capability_required", errObj["code"])
}

func TestS294AssignForeignSquadAgentDeniedEvenWithGrant(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Assign Foreign Target")
	f.grantLeader(t, f.agent.ID, true, true)

	var otherSquad domain.Squad
	doJSON(t, f.handler, http.MethodPost, pathSquads, map[string]any{"name": "S294 Foreign"}, http.StatusCreated, &otherSquad)
	var foreign domain.Agent
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+otherSquad.ID+pathAgents, map[string]any{"name": "foreign-agent"}, http.StatusCreated, &foreign)

	var created domain.Task
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPost, "/api/v1/agents/me/tasks",
		map[string]any{"title": "cross-squad trap"}, http.StatusCreated, &created)

	var body map[string]any
	doAgentJSON(t, f.handler, f.agent.ID, f.credential, http.MethodPatch,
		pathMyTasksPrefix+created.ID+"/assignee", map[string]any{"assignee_agent_id": foreign.ID},
		http.StatusForbidden, &body)
	errObj, _ := body["error"].(map[string]any)
	require.Equal(t, "assignee_not_squad_member", errObj["code"])
}

// ---------------------------------------------------------------------------
// leader_approval_allowed — board.approve (in-review → done, others only)
// ---------------------------------------------------------------------------

// otherInReviewTask drives the fixture agent's own task to in-review, so
// a leader (the squad mate) can approve it.
func otherInReviewTask(t *testing.T, f *s290Fixture, title string) domain.Task {
	t.Helper()
	task := f.newTask(t, title, "")
	claimed := f.claim(t)
	return f.complete(t, task.ID, f.fenceBody(claimed.ExecutionID, claimed.FencingToken, nil))
}

func (f *s290Fixture) approveAs(t *testing.T, agentID, cred, taskID string, body map[string]any) (domain.Task, int) {
	t.Helper()
	var updated domain.Task
	rec := doAgentRequest(t, f.handler, agentID, cred, http.MethodPost, pathMyTasksPrefix+taskID+"/approve", body)
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	}
	return updated, rec.Code
}

func TestS294LeaderApproveHappyPath(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Approve")
	leaderID, leaderCred := f.addSquadMate(t, "leader")
	f.grantLeaderWithApproval(t, leaderID, true)

	task := otherInReviewTask(t, f, "approve me")
	updated, code := f.approveAs(t, leaderID, leaderCred, task.ID, map[string]any{"comment": "verified against spec"})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, domain.TaskDone, updated.Status)
	require.Equal(t, f.agent.ID, updated.AssigneeAgentID, "approval must not touch the assignee")

	// Audited as task.leader_approved with the leader + comment.
	var audit []domain.AuditEntry
	doJSON(t, f.handler, http.MethodGet, pathSquadsPrefix+f.squad.ID+pathAudit+"?limit=50", nil, http.StatusOK, &audit)
	var found *domain.AuditEntry
	for i := range audit {
		if audit[i].Action == "task.leader_approved" {
			found = &audit[i]
			break
		}
	}
	require.NotNil(t, found, "task.leader_approved audit entry expected")
	require.Equal(t, leaderID, found.ActorID)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(found.Metadata, &meta))
	require.Equal(t, true, meta["leader_approved"])
	require.Equal(t, f.agent.ID, meta["assignee_agent_id"])
	require.Equal(t, "verified against spec", meta["comment"])
}

func TestS294LeaderApproveDeniedWhenPolicyOff(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Approve Off")
	leaderID, leaderCred := f.addSquadMate(t, "leader")
	f.grantLeader(t, leaderID, true, true)
	// leader_approval_allowed unset → platform default false.

	task := otherInReviewTask(t, f, "stays human-approved")
	_, code := f.approveAs(t, leaderID, leaderCred, task.ID, map[string]any{"comment": "nope"})
	require.Equal(t, http.StatusForbidden, code)

	// Explicitly off also denied after an on/off flip.
	f.grantLeaderWithApproval(t, leaderID, false)
	_, code = f.approveAs(t, leaderID, leaderCred, task.ID, nil)
	require.Equal(t, http.StatusForbidden, code)
	got := f.getTask(t, task.ID)
	require.Equal(t, domain.TaskInReview, got.Status, "denied approval applies nothing")
}

func TestS294LeaderApproveDeniedForNonLeader(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Approve NonLeader")
	f.setBoardPolicy(t, map[string]any{"leader_approval_allowed": true}) // knob on, actor has no grant

	task := otherInReviewTask(t, f, "not your call")
	plainID, plainCred := f.addSquadMate(t, "plain")
	_, code := f.approveAs(t, plainID, plainCred, task.ID, nil)
	require.Equal(t, http.StatusForbidden, code)
}

func TestS294LeaderApproveSelfDenied(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Approve Self")
	f.grantLeaderWithApproval(t, f.agent.ID, true)

	task := otherInReviewTask(t, f, "self approve trap")
	_, code := f.approveAs(t, f.agent.ID, f.credential, task.ID, nil)
	require.Equal(t, http.StatusForbidden, code)
	got := f.getTask(t, task.ID)
	require.Equal(t, domain.TaskInReview, got.Status)
}

func TestS294LeaderApproveWrongStatus(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Approve Status")
	leaderID, leaderCred := f.addSquadMate(t, "leader")
	f.grantLeaderWithApproval(t, leaderID, true)

	todoTask := f.newTask(t, "still todo", "")
	_, code := f.approveAs(t, leaderID, leaderCred, todoTask.ID, nil)
	require.Equal(t, http.StatusConflict, code)
}

func TestS294LeaderApproveCrossSquadDenied(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Approve CrossSquad")

	var otherSquad domain.Squad
	doJSON(t, f.handler, http.MethodPost, pathSquads, map[string]any{"name": "S294 Other"}, http.StatusCreated, &otherSquad)
	var otherAgent domain.Agent
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+otherSquad.ID+pathAgents, map[string]any{"name": "other-agent"}, http.StatusCreated, &otherAgent)
	var otherTask domain.Task
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+otherSquad.ID+pathBoardTasks,
		map[string]any{"title": "foreign in-review", "assignee_agent_id": otherAgent.ID, "status": "in-review"},
		http.StatusCreated, &otherTask)

	// The fixture agent is not even in that squad — 403 forbidden.
	_, code := f.approveAs(t, f.agent.ID, f.credential, otherTask.ID, nil)
	require.Equal(t, http.StatusForbidden, code)
}

func TestS294HumanMoveUnaffected(t *testing.T) {
	t.Parallel()
	f := newS290Fixture(t, "S294 Human Path")
	task := otherInReviewTask(t, f, "human approves")

	var updated domain.Task
	doJSON(t, f.handler, http.MethodPost, pathTasksPrefix+task.ID+"/move",
		map[string]any{"status": "done"}, http.StatusOK, &updated)
	require.Equal(t, domain.TaskDone, updated.Status)
}
