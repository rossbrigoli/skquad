// S-278 (re-scope of S-275): task dependency endpoints.
//
// User-facing (browser session) routes for declaring "depends on"
// edges, plus the squad-scoped task search that backs the picker.
// The agent-side enforcement lives in the claim/start paths: starting
// a task whose dependencies are not done returns 409
// dependencies_unsatisfied with the blocking task list.
package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// getTaskDependencies returns {depends_on: [...], dependents: [...]} for a task.
func (s *Server) getTaskDependencies(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadAccessibleTask(w, r)
	if !ok {
		return
	}
	deps, err := s.store.ListTaskDependencies(r.Context(), task.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deps)
}

// addTaskDependency records that this task depends on another task in
// the same squad.
func (s *Server) addTaskDependency(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadOwnedTask(w, r)
	if !ok {
		return
	}
	var req struct {
		DependsOn string `json:"depends_on"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	dependsOn := strings.TrimSpace(req.DependsOn)
	if dependsOn == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "depends_on is required")
		return
	}
	u := currentUser(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}
	ref, err := s.store.AddTaskDependency(
		s.pendingUserAuditCtx(r, "task.add_dependency", "task", task.ID, task.SquadID, nil),
		task.ID, dependsOn, "user", u.ID,
	)
	if err != nil {
		// Cycle/exists wrap ErrConflict — check them before the generic
		// mapping so the client gets a precise machine code.
		switch {
		case errors.Is(err, storage.ErrDependencyCycle):
			writeError(w, http.StatusConflict, "dependency_cycle", "that dependency would create a cycle in the task graph")
		case errors.Is(err, storage.ErrDependencyExists):
			writeError(w, http.StatusConflict, "dependency_exists", "this task already depends on that task")
		case errors.Is(err, storage.ErrInvalidInput):
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		default:
			writeStorageError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}

// removeTaskDependency drops one depends-on edge of this task.
func (s *Server) removeTaskDependency(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadOwnedTask(w, r)
	if !ok {
		return
	}
	depTaskID := strings.TrimSpace(chi.URLParam(r, "depTaskID"))
	if depTaskID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "depTaskID is required")
		return
	}
	if err := s.store.RemoveTaskDependency(
		s.pendingUserAuditCtx(r, "task.remove_dependency", "task", task.ID, task.SquadID, nil),
		task.ID, depTaskID,
	); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// searchSquadTasks powers the dependency picker: title substring or
// "T-<n>" / "<n>" reference match within the squad.
func (s *Server) searchSquadTasks(w http.ResponseWriter, r *http.Request) {
	squad, ok := s.loadAccessibleSquad(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	tasks, err := s.store.SearchSquadTasks(r.Context(), squad.ID, q, limit)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if tasks == nil {
		tasks = []*domain.Task{}
	}
	writeJSON(w, http.StatusOK, tasks)
}
