// S-301: task-subagent thread snapshot. The runtime publishes a
// replaceable view of the nested run via
// POST /api/v1/agents/me/tasks/{id}/subagent-thread and the UI reads it
// via GET /api/v1/tasks/{id}/subagent-thread. Crucially, publishing
// creates NO message rows — the chat feed and the task thread stay clean
// while the Details panel streams the live thread.
package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

type snapshotResp struct {
	Snapshot struct {
		Thread []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"thread"`
		Turns int      `json:"turns"`
		Steps []string `json:"steps"`
	} `json:"snapshot"`
	Running bool `json:"running"`
}

func TestSubagentThreadSnapshotRoundTrip(t *testing.T) {
	handler, crWriter, squad, agent, credential := agentRuntimeSetup(t, "subagent-thread-squad")

	// Second agent in the squad — must not be able to publish snapshots.
	var other domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "other-publisher"}, http.StatusCreated, &other)
	var otherIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+other.ID+pathIdentity, nil, http.StatusCreated, &otherIdentity)
	otherCredential := crWriter.credentialTokens[otherIdentity.CredentialRef]

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Snapshot task",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	// No active execution yet → 404 (best-effort for the runtime).
	var nf map[string]map[string]string
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/subagent-thread",
		map[string]any{"snapshot": map[string]any{"thread": []any{}, "turns": 1, "steps": []any{}}},
		http.StatusNotFound, &nf)
	require.Equal(t, "no_active_execution", nf["error"]["code"])

	// Claim → active execution; publish lands a snapshot.
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	require.Equal(t, task.ID, claimed.ID)

	snap1 := map[string]any{
		"thread": []any{map[string]any{"role": "user", "content": "do the thing"}},
		"turns":  1,
		"steps":  []string{"exec"},
	}
	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/subagent-thread",
		map[string]any{"snapshot": snap1}, http.StatusNoContent)

	var resp snapshotResp
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+"/subagent-thread", nil, http.StatusOK, &resp)
	require.True(t, resp.Running)
	require.Equal(t, 1, resp.Snapshot.Turns)
	require.Len(t, resp.Snapshot.Thread, 1)
	require.Equal(t, "do the thing", resp.Snapshot.Thread[0].Content)
	require.Equal(t, []string{"exec"}, resp.Snapshot.Steps)

	// Replace semantics: the second publish overwrites the view.
	snap2 := map[string]any{
		"thread": []any{
			map[string]any{"role": "user", "content": "do the thing"},
			map[string]any{"role": "assistant", "content": "on it"},
		},
		"turns": 2,
		"steps": []string{"exec", "write_file"},
	}
	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/subagent-thread",
		map[string]any{"snapshot": snap2}, http.StatusNoContent)

	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+"/subagent-thread", nil, http.StatusOK, &resp)
	require.Equal(t, 2, resp.Snapshot.Turns)
	require.Len(t, resp.Snapshot.Thread, 2)
	require.Len(t, resp.Snapshot.Steps, 2)

	// Publishing created NO message rows: the task thread still holds
	// only the creation event.
	var thread []domain.Message
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	require.Len(t, thread, 1)
	require.Contains(t, string(thread[0].Payload), "Snapshot task")

	// Non-assignee cannot publish.
	var forbidden map[string]map[string]string
	doAgentJSON(t, handler, other.ID, otherCredential, http.MethodPost, pathMyTasksPrefix+task.ID+"/subagent-thread",
		map[string]any{"snapshot": map[string]any{"thread": []any{}}}, http.StatusForbidden, &forbidden)
	require.Equal(t, "not_assignee", forbidden["error"]["code"])

	// Non-object snapshot is rejected.
	var bad map[string]map[string]string
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/subagent-thread",
		map[string]any{"snapshot": []any{1, 2}}, http.StatusBadRequest, &bad)
	require.Equal(t, "bad_snapshot", bad["error"]["code"])

	// After completion the snapshot survives but running flips false —
	// the Details panel keeps showing the final thread.
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        string(domain.TaskInReview),
		"summary":       "done",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})

	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+"/subagent-thread", nil, http.StatusOK, &resp)
	require.False(t, resp.Running)
	require.Equal(t, 2, resp.Snapshot.Turns)
}

// TestSubagentThreadSnapshotUnknownTask: GET on a task that never ran
// returns an empty snapshot rather than an error, so the panel renders
// "no activity yet" instead of breaking.
func TestSubagentThreadSnapshotUnknownTask(t *testing.T) {
	handler, _, squad, _, _ := agentRuntimeSetup(t, "subagent-thread-empty-squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title": "Never executed",
	}, http.StatusCreated, &task)

	var resp snapshotResp
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+"/subagent-thread", nil, http.StatusOK, &resp)
	require.False(t, resp.Running)
	require.Empty(t, resp.Snapshot.Thread)
	require.Equal(t, 0, resp.Snapshot.Turns)
}
