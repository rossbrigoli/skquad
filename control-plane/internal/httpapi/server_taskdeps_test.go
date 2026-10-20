// S-278 (re-scope of S-275): task dependency endpoints + pickup guards.
package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// doJSONRaw performs a user-session JSON request and returns the raw
// recorder so tests can assert on error codes in the body.
func doJSONRaw(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set(headerContentType, jsonContentType)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func addDep(t *testing.T, h http.Handler, taskID, dependsOn string) domain.TaskDependencyRef {
	t.Helper()
	var ref domain.TaskDependencyRef
	doJSON(t, h, http.MethodPost, pathTasksPrefix+taskID+"/dependencies", map[string]any{"depends_on": dependsOn}, http.StatusCreated, &ref)
	return ref
}

func getDeps(t *testing.T, h http.Handler, taskID string) domain.TaskDependencies {
	t.Helper()
	var deps domain.TaskDependencies
	doJSON(t, h, http.MethodGet, pathTasksPrefix+taskID+"/dependencies", nil, http.StatusOK, &deps)
	return deps
}

func TestTaskDependenciesLifecycle(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	a := createBoardTask(t, h, f.squadID, "alpha work")
	b := createBoardTask(t, h, f.squadID, "beta work")

	// A depends on B.
	ref := addDep(t, h, a.ID, b.ID)
	require.Equal(t, b.ID, ref.TaskID)
	require.Equal(t, "beta work", ref.Title)
	require.False(t, ref.Satisfied)

	// A sees its depends-on; B sees its dependents.
	depsA := getDeps(t, h, a.ID)
	require.Len(t, depsA.DependsOn, 1)
	require.Equal(t, b.ID, depsA.DependsOn[0].TaskID)
	require.Empty(t, depsA.Dependents)

	depsB := getDeps(t, h, b.ID)
	require.Empty(t, depsB.DependsOn)
	require.Len(t, depsB.Dependents, 1)
	require.Equal(t, a.ID, depsB.Dependents[0].TaskID)

	// Duplicate edge → 409 dependency_exists.
	rec := doJSONRaw(t, h, http.MethodPost, pathTasksPrefix+a.ID+"/dependencies", map[string]any{"depends_on": b.ID})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "dependency_exists")

	// Self-dependency → 400.
	rec = doJSONRaw(t, h, http.MethodPost, pathTasksPrefix+a.ID+"/dependencies", map[string]any{"depends_on": a.ID})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Direct cycle (B→A while A→B) → 409 dependency_cycle.
	rec = doJSONRaw(t, h, http.MethodPost, pathTasksPrefix+b.ID+"/dependencies", map[string]any{"depends_on": a.ID})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "dependency_cycle")

	// Transitive cycle: B→C added, so C→A would close A→B→C→A → 409.
	c := createBoardTask(t, h, f.squadID, "gamma work")
	addDep(t, h, b.ID, c.ID)
	rec = doJSONRaw(t, h, http.MethodPost, pathTasksPrefix+c.ID+"/dependencies", map[string]any{"depends_on": a.ID})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "dependency_cycle")

	// Remove the A→B edge.
	doJSONNoBody(t, h, http.MethodDelete, pathTasksPrefix+a.ID+"/dependencies/"+b.ID, nil, http.StatusNoContent)
	require.Empty(t, getDeps(t, h, a.ID).DependsOn)

	// Removing a non-existent edge → 404.
	doJSONNoBody(t, h, http.MethodDelete, pathTasksPrefix+a.ID+"/dependencies/"+a.ID, nil, http.StatusNotFound)
}

func TestTaskDependenciesCrossSquadRejected(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	var otherSquad domain.Squad
	doJSON(t, h, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Other Squad"}, http.StatusCreated, &otherSquad)
	alien := createBoardTask(t, h, otherSquad.ID, "alien task")

	mine := createBoardTask(t, h, f.squadID, "mine")
	rec := doJSONRaw(t, h, http.MethodPost, pathTasksPrefix+mine.ID+"/dependencies", map[string]any{"depends_on": alien.ID})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "same squad")
}

func TestTaskDependenciesClaimSkipsBlocked(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	// A is top-of-todo but depends on B (lower). Claim must serve B first.
	a := createBoardTask(t, h, f.squadID, "blocked alpha")
	b := createBoardTask(t, h, f.squadID, "unblocker beta")
	assignTask(t, h, a.ID, f.workerID)
	assignTask(t, h, b.ID, f.workerID)
	addDep(t, h, a.ID, b.ID)

	claimed := f.claim(t)
	require.Equal(t, b.ID, claimed.ID, "claim must skip the dependency-blocked task")

	// Completing the dependency unblocks A.
	doAgentJSONNoBody(t, h, f.workerID, f.workerCred, http.MethodPost,
		"/api/v1/agents/me/tasks/"+b.ID+"/complete",
		map[string]any{"status": "done", "execution_id": claimed.ExecutionID, "fencing_token": claimed.FencingToken, "summary": "unblocked"},
		http.StatusOK)

	claimed2 := f.claim(t)
	require.Equal(t, a.ID, claimed2.ID)
}

func TestTaskDependenciesStartGuard(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	a := createBoardTask(t, h, f.squadID, "guarded alpha")
	b := createBoardTask(t, h, f.squadID, "blocker beta")
	assignTask(t, h, a.ID, f.workerID)
	addDep(t, h, a.ID, b.ID)

	rec := doAgentRequest(t, h, f.workerID, f.workerCred, http.MethodPost,
		"/api/v1/agents/me/tasks/"+a.ID+"/start", map[string]any{})
	require.Equal(t, http.StatusConflict, rec.Code)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
	errObj, _ := parsed["error"].(map[string]any)
	require.Equal(t, "dependencies_unsatisfied", errObj["code"])
	blocking, _ := parsed["blocking"].([]any)
	require.Len(t, blocking, 1)
	require.Equal(t, b.ID, blocking[0].(map[string]any)["task_id"])
}

func TestTaskDependenciesSearch(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	a := createBoardTask(t, h, f.squadID, "write quarterly report")
	createBoardTask(t, h, f.squadID, "unrelated chore")

	var byTitle []domain.Task
	doJSON(t, h, http.MethodGet, pathSquadsPrefix+f.squadID+"/tasks/search?q=quarterly", nil, http.StatusOK, &byTitle)
	require.Len(t, byTitle, 1)
	require.Equal(t, a.ID, byTitle[0].ID)

	// By T-reference.
	var byRef []domain.Task
	doJSON(t, h, http.MethodGet, pathSquadsPrefix+f.squadID+"/tasks/search?q=T-"+strconv.Itoa(a.TaskNumber), nil, http.StatusOK, &byRef)
	require.Len(t, byRef, 1)
	require.Equal(t, a.ID, byRef[0].ID)

	// Empty query returns recent tasks.
	var all []domain.Task
	doJSON(t, h, http.MethodGet, pathSquadsPrefix+f.squadID+"/tasks/search", nil, http.StatusOK, &all)
	require.Len(t, all, 2)
}
