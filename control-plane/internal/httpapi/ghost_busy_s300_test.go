// S-300: ghost-busy fix — a todo blocked by unfinished dependencies is
// WAITING, not pending work, so the agent surfaces as `blocked` instead of
// being held busy; completing an upstream task must re-sync the agents
// assigned to dependent tasks; and a blocked agent must stay wakeable.
package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

type s300Fixture struct {
	handler         http.Handler
	crWriter        *fakeCRWriter
	squadID         string
	agentID         string // the downstream (dependent-task) agent
	upstreamAgentID string
	credential      string
	upstreamID      string
	downstreamID    string
}

func newS300Fixture(t *testing.T) *s300Fixture {
	t.Helper()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "S300 Squad"}, http.StatusCreated, &squad)

	// Downstream agent ("Bob" in the live incident): owns a todo that
	// depends on another task.
	var downstream domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Downstream"}, http.StatusCreated, &downstream)

	// Upstream agent ("Alice"): owns the blocking task.
	var upstream domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Upstream"}, http.StatusCreated, &upstream)

	var upTask domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Travel aggregator",
		"assignee_agent_id": upstream.ID,
	}, http.StatusCreated, &upTask)

	var downTask domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Search for hotel in Sydney",
		"assignee_agent_id": downstream.ID,
	}, http.StatusCreated, &downTask)

	addDep(t, handler, downTask.ID, upTask.ID)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+downstream.ID+pathIdentity, nil, http.StatusCreated, &identity)

	return &s300Fixture{
		handler:         handler,
		crWriter:        crWriter,
		squadID:         squad.ID,
		agentID:         downstream.ID,
		upstreamAgentID: upstream.ID,
		credential:      crWriter.credentialTokens[identity.CredentialRef],
		upstreamID:      upTask.ID,
		downstreamID:    downTask.ID,
	}
}

func (f *s300Fixture) agentStatus(t *testing.T) domain.AgentStatus {
	t.Helper()
	var agent domain.Agent
	doJSON(t, f.handler, http.MethodGet, pathAgentsPrefix+f.agentID, nil, http.StatusOK, &agent)
	return agent.Status
}

func (f *s300Fixture) heartbeat(t *testing.T, status string) {
	t.Helper()
	doAgentJSON(t, f.handler, f.agentID, f.credential, http.MethodPost, pathMyHeartbeat, map[string]any{"status": status}, http.StatusOK, &domain.Agent{})
}

// TestS300HeartbeatMarksAgentBlockedWhenOnlyTodoIsDependencyBlocked: the
// ghost-busy regression, with Ross's blocked state. An agent whose only
// open work is a dependency-blocked todo is neither running nor idle —
// it is BLOCKED: work exists but ClaimNextTask cannot reach it.
func TestS300HeartbeatMarksAgentBlockedWhenOnlyTodoIsDependencyBlocked(t *testing.T) {
	f := newS300Fixture(t)

	f.heartbeat(t, "idle")
	require.Equal(t, domain.AgentBlocked, f.agentStatus(t),
		"blocked todo must resolve the idle heartbeat to blocked, never busy")

	// Sanity: the downstream task really is a todo with an unsatisfied dep.
	deps := getDeps(t, f.handler, f.downstreamID)
	require.Len(t, deps.DependsOn, 1)
	require.False(t, deps.DependsOn[0].Satisfied)
}

// TestS300BlockedIsStableAcrossHeartbeats: repeated idle heartbeats keep
// the derived blocked state (they must not decay to idle or creep to busy).
func TestS300BlockedIsStableAcrossHeartbeats(t *testing.T) {
	f := newS300Fixture(t)

	for i := 0; i < 3; i++ {
		f.heartbeat(t, "idle")
		require.Equal(t, domain.AgentBlocked, f.agentStatus(t))
	}
}

// TestS300NoWorkIsIdleNotBlocked: an agent with no tasks at all is idle.
// Blocked requires work that cannot be started.
func TestS300NoWorkIsIdleNotBlocked(t *testing.T) {
	f := newS300Fixture(t)

	doJSON(t, f.handler, http.MethodPost, pathTasksPrefix+f.downstreamID+"/move", map[string]any{
		"status": "done",
	}, http.StatusOK, &domain.Task{})

	f.heartbeat(t, "idle")
	require.Equal(t, domain.AgentIdle, f.agentStatus(t),
		"no open work must be idle, not blocked")
}

// TestS300MixedWorkIsBusy: one blocked todo plus one claimable todo is
// BUSY — blocked only applies when ALL remaining todos are blocked.
func TestS300MixedWorkIsBusy(t *testing.T) {
	f := newS300Fixture(t)

	var freeTask domain.Task
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squadID+pathBoardTasks, map[string]any{
		"title":             "Unrelated free task",
		"assignee_agent_id": f.agentID,
	}, http.StatusCreated, &freeTask)

	f.heartbeat(t, "idle")
	require.Equal(t, domain.AgentBusy, f.agentStatus(t),
		"a claimable todo outranks a blocked one")
}

// TestS300BlockedAgentIsWakeableByChat: the critical gotcha. wakeAgent
// only flipped *idle* agents to busy; a blocked agent would have been
// unwakeable, so a direct chat message could never start its pod.
func TestS300BlockedAgentIsWakeableByChat(t *testing.T) {
	f := newS300Fixture(t)

	f.heartbeat(t, "idle")
	require.Equal(t, domain.AgentBlocked, f.agentStatus(t))

	var res map[string]any
	doJSON(t, f.handler, http.MethodPost, pathAgentsPrefix+f.agentID+"/wake", map[string]any{}, http.StatusOK, &res)
	require.Equal(t, true, res["waking"],
		"a blocked agent must still be pre-warmable by chat")
	require.Equal(t, string(domain.AgentBusy), res["status"])
}

// TestHeartbeatBusyWithUnblockedTodo: control case — an unblocked todo
// still counts as pending work.
func TestS300HeartbeatUpgradesToUnblockedTodo(t *testing.T) {
	f := newS300Fixture(t)

	// Complete the upstream task as a human move so the edge is satisfied.
	doJSON(t, f.handler, http.MethodPost, pathTasksPrefix+f.upstreamID+"/move", map[string]any{
		"status": "done",
	}, http.StatusOK, &domain.Task{})

	f.heartbeat(t, "idle")
	require.Equal(t, domain.AgentBusy, f.agentStatus(t),
		"unblocked todo must upgrade idle heartbeat to busy")
}

// TestDependentAgentWokenOnUpstreamDone: completing the upstream task
// via the agent complete path must flip the downstream agent's status
// from idle to busy (its todo just became claimable).
func TestS300DependentAgentWokenOnUpstreamDone(t *testing.T) {
	f := newS300Fixture(t)

	// Downstream agent is blocked while its only todo waits upstream.
	f.heartbeat(t, "idle")
	require.Equal(t, domain.AgentBlocked, f.agentStatus(t))

	// Upstream agent claims its task (execution fence) then completes it.
	var upIdentity domain.AgentIdentity
	doJSON(t, f.handler, http.MethodPost, pathAgentsPrefix+f.upstreamAgentID+pathIdentity, nil, http.StatusCreated, &upIdentity)
	upCred := f.crWriter.credentialTokens[upIdentity.CredentialRef]
	var claimed domain.Task
	doAgentJSON(t, f.handler, f.upstreamAgentID, upCred, http.MethodPost,
		"/api/v1/agents/me/tasks/claim", map[string]any{"worker_id": "w-up", "lease_seconds": 60}, http.StatusOK, &claimed)
	require.Equal(t, f.upstreamID, claimed.ID)
	doAgentJSON(t, f.handler, f.upstreamAgentID, upCred, http.MethodPost,
		pathMyTasksPrefix+f.upstreamID+pathComplete, map[string]any{
			"execution_id":  claimed.ExecutionID,
			"fencing_token": claimed.FencingToken,
			"status":        "done",
			"summary":       "aggregated",
		}, http.StatusOK, &domain.Task{})

	// The dependent-sync must have re-evaluated the downstream agent.
	require.Equal(t, domain.AgentBusy, f.agentStatus(t),
		"completing upstream task must wake the dependent agent's status")
}

// TestDependentAgentWokenOnHumanMove: same via the human move path.
func TestS300DependentAgentWokenOnHumanMove(t *testing.T) {
	f := newS300Fixture(t)

	f.heartbeat(t, "idle")
	require.Equal(t, domain.AgentBlocked, f.agentStatus(t))

	doJSON(t, f.handler, http.MethodPost, pathTasksPrefix+f.upstreamID+"/move", map[string]any{
		"status": "done",
	}, http.StatusOK, &domain.Task{})

	require.Equal(t, domain.AgentBusy, f.agentStatus(t),
		"human move of upstream to done must re-sync the dependent agent")
}

// TestInProgressStillPending: in-progress work always counts, deps are
// irrelevant once the task is running.
func TestS300InProgressStillPending(t *testing.T) {
	f := newS300Fixture(t)

	// Move the downstream task to in_progress directly (human move).
	doJSON(t, f.handler, http.MethodPost, pathTasksPrefix+f.downstreamID+"/move", map[string]any{
		"status": "in-progress",
	}, http.StatusOK, &domain.Task{})

	f.heartbeat(t, "idle")
	require.Equal(t, domain.AgentBusy, f.agentStatus(t),
		"in_progress must count as pending regardless of dependencies")
}
