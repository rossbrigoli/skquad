// S-278: task dependencies — MemoryStore implementation.
//
// Mirrors the Postgres semantics: same-squad only, no self-dependency,
// no cycles (transitive BFS), duplicate edges rejected, cascade on task
// delete, and blocked-for-pickup while any depends-on task is not done.
package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// AddTaskDependency records taskID depends-on dependsOnID.
func (m *MemoryStore) AddTaskDependency(ctx context.Context, taskID string, dependsOnID string, createdByType string, createdByID string) (*domain.TaskDependencyRef, error) {
	if taskID == "" || dependsOnID == "" {
		return nil, fmt.Errorf("%w: task_id and depends_on are required", ErrInvalidInput)
	}
	if taskID == dependsOnID {
		return nil, fmt.Errorf("%w: a task cannot depend on itself", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[taskID]
	if !ok {
		return nil, ErrNotFound
	}
	dep, ok := m.tasks[dependsOnID]
	if !ok {
		return nil, ErrNotFound
	}
	if task.SquadID != dep.SquadID {
		return nil, fmt.Errorf("%w: dependencies must be within the same squad", ErrInvalidInput)
	}
	if m.taskDeps[taskID][dependsOnID] {
		return nil, ErrDependencyExists
	}
	// Cycle check: dep already reaching task (transitively) would close a
	// cycle when task→dep is added.
	if m.taskReachesLocked(dependsOnID, taskID) {
		return nil, ErrDependencyCycle
	}
	if m.taskDeps[taskID] == nil {
		m.taskDeps[taskID] = map[string]bool{}
	}
	m.taskDeps[taskID][dependsOnID] = true
	return m.taskDependencyRefLocked(dependsOnID)
}

// RemoveTaskDependency drops one depends-on edge.
func (m *MemoryStore) RemoveTaskDependency(ctx context.Context, taskID string, dependsOnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if deps, ok := m.taskDeps[taskID]; ok && deps[dependsOnID] {
		delete(deps, dependsOnID)
		if len(deps) == 0 {
			delete(m.taskDeps, taskID)
		}
		return nil
	}
	return ErrNotFound
}

// ListTaskDependencies returns the depends-on + dependents views.
func (m *MemoryStore) ListTaskDependencies(ctx context.Context, taskID string) (*domain.TaskDependencies, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := &domain.TaskDependencies{DependsOn: []domain.TaskDependencyRef{}, Dependents: []domain.TaskDependencyRef{}}
	for depID := range m.taskDeps[taskID] {
		ref, err := m.taskDependencyRefLocked(depID)
		if err != nil {
			return nil, err
		}
		out.DependsOn = append(out.DependsOn, *ref)
	}
	for otherID, deps := range m.taskDeps {
		if otherID == taskID {
			continue
		}
		if deps[taskID] {
			ref, err := m.taskDependencyRefLocked(otherID)
			if err != nil {
				return nil, err
			}
			out.Dependents = append(out.Dependents, *ref)
		}
	}
	sortTaskDependencyRefs(out.DependsOn)
	sortTaskDependencyRefs(out.Dependents)
	return out, nil
}

// UnsatisfiedDependencies returns the not-done depends-on tasks.
func (m *MemoryStore) UnsatisfiedDependencies(ctx context.Context, taskID string) ([]domain.TaskDependencyRef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	refs := []domain.TaskDependencyRef{}
	for depID := range m.taskDeps[taskID] {
		ref, err := m.taskDependencyRefLocked(depID)
		if err != nil {
			return nil, err
		}
		if !ref.Satisfied {
			refs = append(refs, *ref)
		}
	}
	sortTaskDependencyRefs(refs)
	return refs, nil
}

// SearchSquadTasks finds tasks in a squad for the dependency picker.
func (m *MemoryStore) SearchSquadTasks(ctx context.Context, squadID string, q string, limit int) ([]*domain.Task, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	needle := strings.ToLower(strings.TrimSpace(q))
	var matches []*domain.Task
	for _, t := range m.tasks {
		if t.SquadID != squadID {
			continue
		}
		if needle != "" {
			refMatch := strings.EqualFold(fmt.Sprintf("T-%d", t.TaskNumber), needle) || fmt.Sprintf("%d", t.TaskNumber) == needle
			if !strings.Contains(strings.ToLower(t.Title), needle) && !refMatch {
				continue
			}
		}
		matches = append(matches, t)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].UpdatedAt.After(matches[j].UpdatedAt) })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// BlockedCountsByBoard returns per-task counts of unfinished depends-on
// tasks for one board (S-278).
func (m *MemoryStore) BlockedCountsByBoard(ctx context.Context, boardID string) (map[string]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	counts := map[string]int{}
	for taskID, deps := range m.taskDeps {
		task, ok := m.tasks[taskID]
		if !ok || task.BoardID != boardID {
			continue
		}
		for depID := range deps {
			if dep, ok := m.tasks[depID]; ok && dep.Status != domain.TaskDone {
				counts[taskID]++
			}
		}
	}
	return counts, nil
}

// taskReachesLocked reports whether `from` depends (transitively) on
// `target`. Caller must hold m.mu. Depth-capped defensively against
// pre-existing malformed graphs.
func (m *MemoryStore) taskReachesLocked(from string, target string) bool {
	visited := map[string]bool{from: true}
	frontier := []string{from}
	for depth := 0; depth < 32 && len(frontier) > 0; depth++ {
		var next []string
		for _, id := range frontier {
			for depID := range m.taskDeps[id] {
				if depID == target {
					return true
				}
				if !visited[depID] {
					visited[depID] = true
					next = append(next, depID)
				}
			}
		}
		frontier = next
	}
	return false
}

func (m *MemoryStore) taskDependencyRefLocked(taskID string) (*domain.TaskDependencyRef, error) {
	t, ok := m.tasks[taskID]
	if !ok {
		return nil, ErrNotFound
	}
	return &domain.TaskDependencyRef{
		TaskID:     t.ID,
		TaskNumber: t.TaskNumber,
		Title:      t.Title,
		Status:     t.Status,
		Satisfied:  t.Status == domain.TaskDone,
	}, nil
}

func sortTaskDependencyRefs(refs []domain.TaskDependencyRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].TaskNumber < refs[j].TaskNumber })
}
