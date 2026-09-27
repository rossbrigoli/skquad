package httpapi

// S-PROMPT WP5 — server-side run-audit prompt hash tests.
//
// Contract under test: the runtime reports the composed-prompt sha it is
// running under in the task-start body; the control plane persists it on
// the active task_executions row (migration 0017) and every read path
// (task detail, board listing) exposes it, so "what did the agent see
// for run X" is one query. The legacy fallback path reports the literal
// "env_legacy"; old runtimes that send no sha keep working (empty).

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const validRunSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// claimAndStart claims the agent's next task and starts it with the given
// start-body (nil = no body, emulating a pre-WP5 runtime).
func claimAndStart(t *testing.T, handler http.Handler, agentID, credential string, taskID string, startBody any) domain.Task {
	t.Helper()
	var claimed domain.Task
	doAgentJSON(t, handler, agentID, credential, http.MethodPost, pathMyTasksPrefix+"claim",
		map[string]any{"started_at": "2026-09-27T00:00:00Z"}, http.StatusOK, &claimed)
	require.Equal(t, taskID, claimed.ID)
	require.NotEmpty(t, claimed.ExecutionID, "claim must create the execution row the sha attaches to")

	var started domain.Task
	doAgentJSON(t, handler, agentID, credential, http.MethodPost, pathMyTasksPrefix+taskID+"/start",
		startBody, http.StatusOK, &started)
	return started
}

func TestStartTaskPersistsPromptSHA(t *testing.T) {
	t.Parallel()
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "RunAudit Squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "audited run",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	started := claimAndStart(t, handler, agent.ID, credential, task.ID, map[string]any{"prompt_sha": validRunSHA})
	require.Equal(t, validRunSHA, started.PromptSHA, "start response must echo the reported sha")

	// Task detail: one query answers "what prompt did run X use".
	var detail domain.Task
	doJSON(t, handler, http.MethodGet, "/api/v1/tasks/"+task.ID, nil, http.StatusOK, &detail)
	require.Equal(t, validRunSHA, detail.PromptSHA)
	require.NotEmpty(t, detail.ExecutionID, "detail must expose the audited execution row")

	// Board listing carries it too (attachExecutionState).
	var boardResp struct {
		Tasks []*domain.Task `json:"tasks"`
	}
	doJSON(t, handler, http.MethodGet, pathSquadsPrefix+squad.ID+"/board", nil, http.StatusOK, &boardResp)
	require.Len(t, boardResp.Tasks, 1)
	require.Equal(t, validRunSHA, boardResp.Tasks[0].PromptSHA)
}

func TestStartTaskAcceptsEnvLegacyMarker(t *testing.T) {
	t.Parallel()
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "RunAudit Legacy Squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "legacy run",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	claimAndStart(t, handler, agent.ID, credential, task.ID, map[string]any{"prompt_sha": "env_legacy"})

	var detail domain.Task
	doJSON(t, handler, http.MethodGet, "/api/v1/tasks/"+task.ID, nil, http.StatusOK, &detail)
	require.Equal(t, "env_legacy", detail.PromptSHA, "legacy fallback path must be auditable as such")
}

func TestStartTaskRejectsMalformedPromptSHA(t *testing.T) {
	t.Parallel()
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "RunAudit Malformed Squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "malformed run",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	// Claim so an active execution exists — the rejection must happen at
	// validation, before any persistence attempt.
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+"claim", nil, http.StatusOK, &claimed)

	for _, bad := range []string{"nope", "deadbeef", validRunSHA + "0", validRunSHA[:63], "ENV_LEGACY", "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"} {
		var body map[string]map[string]string
		doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, pathMyTasksPrefix+task.ID+"/start",
			map[string]any{"prompt_sha": bad}, http.StatusBadRequest, &body)
		require.Equal(t, "invalid_prompt_sha", body["error"]["code"], "sha=%q must be rejected", bad)
	}

	// Nothing was persisted: the detail still reports no sha.
	var detail domain.Task
	doJSON(t, handler, http.MethodGet, "/api/v1/tasks/"+task.ID, nil, http.StatusOK, &detail)
	require.Empty(t, detail.PromptSHA)
}

func TestStartTaskWithoutSHAStaysCompatible(t *testing.T) {
	t.Parallel()
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "RunAudit Compat Squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "pre-WP5 runtime",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	// No body at all: old runtime, start still succeeds, sha stays empty.
	claimAndStart(t, handler, agent.ID, credential, task.ID, nil)

	var detail domain.Task
	doJSON(t, handler, http.MethodGet, "/api/v1/tasks/"+task.ID, nil, http.StatusOK, &detail)
	require.Equal(t, domain.TaskInProgress, detail.Status)
	require.Empty(t, detail.PromptSHA, "absent sha means unknown, never a fabricated value")
}

func TestTaskDetailWithoutExecutionHasNoSHA(t *testing.T) {
	t.Parallel()
	handler := New(testConfig(), storage.NewMemoryStore())

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "NeverRun Squad"}, http.StatusCreated, &squad)
	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title": "never executed",
	}, http.StatusCreated, &task)

	var detail domain.Task
	doJSON(t, handler, http.MethodGet, "/api/v1/tasks/"+task.ID, nil, http.StatusOK, &detail)
	require.Empty(t, detail.PromptSHA)
	require.Empty(t, detail.ExecutionID)
}

func TestPromptSHAQueryRoundTripThroughStore(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	squad := &domain.Squad{ID: "sq-1", Name: "store-squad"}
	created, err := store.CreateSquad(t.Context(), squad)
	require.NoError(t, err)
	agent := &domain.Agent{ID: "ag-1", SquadID: created.ID, Name: "runner"}
	agent, err = store.CreateAgent(t.Context(), agent)
	require.NoError(t, err)
	board, err := store.GetBoard(t.Context(), created.ID)
	require.NoError(t, err)
	task, err := store.CreateTask(t.Context(), &domain.Task{
		BoardID: board.ID, SquadID: created.ID, Title: "t", Status: domain.TaskTodo, AssigneeAgentID: agent.ID,
	})
	require.NoError(t, err)

	_, err = store.ClaimNextTask(t.Context(), agent.ID, "w-1", 0)
	require.NoError(t, err)

	// Reporting against a non-active row must fail closed.
	_, err = store.SetTaskExecutionPromptSHA(t.Context(), "someone-else", task.ID, validRunSHA)
	require.ErrorIs(t, err, storage.ErrNotFound)

	exec, err := store.SetTaskExecutionPromptSHA(t.Context(), agent.ID, task.ID, validRunSHA)
	require.NoError(t, err)
	require.Equal(t, validRunSHA, exec.PromptSHA)

	latest, err := store.GetLatestTaskExecution(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, exec.ID, latest.ID)
	require.Equal(t, validRunSHA, latest.PromptSHA)

	_, err = store.GetLatestTaskExecution(t.Context(), "missing-task")
	require.ErrorIs(t, err, storage.ErrNotFound)
}
