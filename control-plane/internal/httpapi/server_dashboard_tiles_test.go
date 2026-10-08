// S-242 dashboard tile tests: per-agent model, current_task and
// last_task on GET /api/v1/dashboard, including the pinned JSON shape
// (omitempty behavior verified on raw JSON, not just decoded structs).

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// tilesFixture: one squad with three agents.
//   - worker: bound model with display_name, one done task and one
//     in-progress task (moved after the done one).
//   - reviewer: no model binding, one in-review task, nothing done.
//   - idle: no model, no tasks.
//
// Returns the worker's in-progress task so tests can complete it.
func tilesFixture(t *testing.T, handler http.Handler) (worker, reviewer, idle domain.Agent, active domain.Task) {
	t.Helper()

	var squad domain.Squad
	doJSONAuth(t, handler, "", http.MethodPost, pathSquads, map[string]any{"name": "Tiles Squad"}, http.StatusCreated, &squad)

	mkAgent := func(name string) domain.Agent {
		var a domain.Agent
		doJSONAuth(t, handler, "", http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents,
			map[string]any{"name": name, "role": "worker"}, http.StatusCreated, &a)
		return a
	}
	worker = mkAgent("worker")
	reviewer = mkAgent("reviewer")
	idle = mkAgent("idle")

	// Model with an explicit display_name so label precedence is tested.
	provider := createTestProvider(t, handler, "tiles-prov")
	var model domain.AIModel
	doJSONAuth(t, handler, authAdmin, http.MethodPost, pathAIModels, map[string]any{
		"provider_id":  provider.ID,
		"model_name":   "tiles-model-name",
		"display_name": "Tiles Display",
		"pricing":      validPricing(),
	}, http.StatusCreated, &model)
	doJSONAuth(t, handler, authAdmin, http.MethodPut, pathUsersPrefix+squad.OwnerID+pathModels,
		map[string]any{"model_ids": []string{model.ID}}, http.StatusOK, &[]domain.AIModel{})
	doJSONAuth(t, handler, "", http.MethodPatch, pathAgentsPrefix+worker.ID,
		map[string]any{"ai_model_id": model.ID}, http.StatusOK, &worker)

	mkTask := func(agentID, title string) domain.Task {
		var task domain.Task
		doJSONAuth(t, handler, "", http.MethodPost, pathSquadsPrefix+squad.ID+"/board/tasks",
			map[string]any{"title": title, "assignee_agent_id": agentID}, http.StatusCreated, &task)
		return task
	}
	moveTask := func(taskID, status string) domain.Task {
		var task domain.Task
		doJSONAuth(t, handler, "", http.MethodPost, "/api/v1/tasks/"+taskID+"/move",
			map[string]any{"status": status}, http.StatusOK, &task)
		return task
	}

	// worker: done first, then in-progress (so updated_at orders them).
	done := moveTask(mkTask(worker.ID, "finished work").ID, "done")
	require.Equal(t, domain.TaskDone, done.Status)
	active = moveTask(mkTask(worker.ID, "live work").ID, statusInProgress)
	require.Equal(t, domain.TaskStatus(statusInProgress), active.Status)

	// reviewer: in-review only (no done tasks at all → fallback path).
	moveTask(mkTask(reviewer.ID, "awaiting review").ID, "in-review")

	return worker, reviewer, idle, active
}

// dashboardRawAgentTiles fetches the dashboard and returns the raw
// per-agent JSON objects keyed by agent name (raw maps so key omission
// is visible, unlike decoding into the Go struct).
func dashboardRawAgentTiles(t *testing.T, handler http.Handler) map[string]map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var payload struct {
		Squads []struct {
			Agents []json.RawMessage `json:"agents"`
		} `json:"squads"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	out := map[string]map[string]any{}
	for _, sq := range payload.Squads {
		for _, raw := range sq.Agents {
			var m map[string]any
			require.NoError(t, json.Unmarshal(raw, &m))
			name, ok := m["name"].(string)
			require.True(t, ok)
			out[name] = m
		}
	}
	return out
}

func TestDashboardAgentTilesModelCurrentLast(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())
	worker, _, _, _ := tilesFixture(t, handler)

	agents := dashboardRawAgentTiles(t, handler)

	// --- worker: model + current_task + last_task -------------------
	w := agents["worker"]
	require.NotNil(t, w)
	require.Equal(t, "Tiles Display", w["model"], "model prefers display_name")

	cur, ok := w["current_task"].(map[string]any)
	require.True(t, ok, "worker must have current_task: %v", w)
	require.Equal(t, "live work", cur["title"])
	require.Equal(t, "in-progress", cur["status"])
	require.Equal(t, "T-2", cur["ref"], "ref is the T-<task_number> display ref")
	require.NotEmpty(t, cur["id"])

	last, ok := w["last_task"].(map[string]any)
	require.True(t, ok, "worker must have last_task: %v", w)
	require.Equal(t, "finished work", last["title"])
	require.Equal(t, "done", last["status"])
	require.Equal(t, "T-1", last["ref"])
	require.NotEqual(t, cur["id"], last["id"], "current and last must be distinct tasks")

	// --- reviewer: fallback last_task (in-review), no current -------
	r := agents["reviewer"]
	require.NotNil(t, r)
	_, hasModel := r["model"]
	require.False(t, hasModel, "unbound agent must omit model")
	_, hasCur := r["current_task"]
	require.False(t, hasCur, "no in-progress task → current_task omitted")
	rlast, ok := r["last_task"].(map[string]any)
	require.True(t, ok, "reviewer falls back to in-review task: %v", r)
	require.Equal(t, "awaiting review", rlast["title"])
	require.Equal(t, "in-review", rlast["status"])

	// --- idle: nothing at all ---------------------------------------
	i := agents["idle"]
	require.NotNil(t, i)
	for _, key := range []string{"model", "current_task", "last_task"} {
		_, present := i[key]
		require.False(t, present, "idle agent must omit %s", key)
	}

	// Struct-level check: the pinned contract decodes into DashboardAgent.
	var payload DashboardPayload
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil))
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	var found DashboardAgent
	for _, sq := range payload.Squads {
		for _, a := range sq.Agents {
			if a.ID == worker.ID {
				found = a
			}
		}
	}
	require.NotNil(t, found.CurrentTask)
	require.NotNil(t, found.LastTask)
	require.Equal(t, "Tiles Display", found.Model)
	require.Equal(t, "T-2", found.CurrentTask.Ref)
}

// Once the worker's in-progress task completes, current_task disappears
// and last_task rolls forward to the newly done task.
func TestDashboardAgentTilesLastTaskRollsForward(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())
	_, _, _, active := tilesFixture(t, handler)

	var moved domain.Task
	doJSONAuth(t, handler, "", http.MethodPost, "/api/v1/tasks/"+active.ID+"/move",
		map[string]any{"status": "done"}, http.StatusOK, &moved)
	require.Equal(t, domain.TaskDone, moved.Status)

	agents := dashboardRawAgentTiles(t, handler)
	w := agents["worker"]
	_, hasCur := w["current_task"]
	require.False(t, hasCur, "no in-progress task after completion → current_task omitted")
	last, ok := w["last_task"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, active.ID, last["id"], "last_task rolled forward to the just-completed task")
	require.Equal(t, "done", last["status"])
	require.Equal(t, "T-2", last["ref"])
}
