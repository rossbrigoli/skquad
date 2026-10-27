// S-278 (re-scope of S-275): task dependencies — Postgres implementation.
//
// A task may declare "depends on" other tasks in the same squad. While any
// depends-on task is not done, the depending task is blocked for agent
// pickup: ClaimNextTask skips it and the explicit start path answers
// 409 dependencies_unsatisfied. Cycle safety is enforced at insert time
// with a recursive reachability check (a CHECK constraint cannot see the
// rest of the graph).
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const taskDepRefColumns = `t.id::text, t.task_number, t.title, t.status, coalesce(t.assignee_agent_id::text, '')`

// AddTaskDependency records taskID depends-on dependsOnID.
//
// Rejected with ErrInvalidInput: unknown task, self-dependency, or a
// cross-squad edge. Rejected with ErrDependencyExists: duplicate edge.
// Rejected with ErrDependencyCycle: dependsOnID already depends
// (transitively) on taskID.
func (p *PostgresStore) AddTaskDependency(ctx context.Context, taskID string, dependsOnID string, createdByType string, createdByID string) (*domain.TaskDependencyRef, error) {
	if taskID == "" || dependsOnID == "" {
		return nil, fmt.Errorf("%w: task_id and depends_on are required", ErrInvalidInput)
	}
	if taskID == dependsOnID {
		return nil, fmt.Errorf("%w: a task cannot depend on itself", ErrInvalidInput)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer tx.Rollback(ctx)

	var taskSquad, depSquad string
	if err := tx.QueryRow(ctx, `SELECT squad_id::text FROM tasks WHERE id = $1`, taskID).Scan(&taskSquad); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, mapPgErr(err)
	}
	if err := tx.QueryRow(ctx, `SELECT squad_id::text FROM tasks WHERE id = $1`, dependsOnID).Scan(&depSquad); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, mapPgErr(err)
	}
	if taskSquad != depSquad {
		return nil, fmt.Errorf("%w: dependencies must be within the same squad", ErrInvalidInput)
	}

	// Duplicate edge → ErrDependencyExists (PK violation, pre-checked for
	// a precise error rather than a raw constraint name).
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM task_dependencies
			WHERE task_id = $1 AND depends_on_task_id = $2
		)`, taskID, dependsOnID).Scan(&exists); err != nil {
		return nil, mapPgErr(err)
	}
	if exists {
		return nil, ErrDependencyExists
	}

	// Cycle check: adding task→dep closes a cycle iff dep can already
	// reach task by following depends-on edges. Depth-capped defensively.
	var cycle bool
	if err := tx.QueryRow(ctx, `
		WITH RECURSIVE reach(id, depth) AS (
			SELECT $2::uuid, 0
			UNION ALL
			SELECT td.depends_on_task_id, r.depth + 1
			FROM task_dependencies td
			JOIN reach r ON td.task_id = r.id
			WHERE r.depth < 32
		)
		SELECT EXISTS (SELECT 1 FROM reach WHERE id = $1::uuid)`, taskID, dependsOnID).Scan(&cycle); err != nil {
		return nil, mapPgErr(err)
	}
	if cycle {
		return nil, ErrDependencyCycle
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO task_dependencies (task_id, depends_on_task_id, created_by_type, created_by_id)
		VALUES ($1, $2, $3, $4)`, taskID, dependsOnID, createdByType, createdByID); err != nil {
		return nil, mapPgErr(err)
	}
	if err := p.writePendingAuditsTx(ctx, tx, taskID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgErr(err)
	}
	return p.taskDependencyRef(ctx, dependsOnID)
}

// RemoveTaskDependency drops one depends-on edge.
func (p *PostgresStore) RemoveTaskDependency(ctx context.Context, taskID string, dependsOnID string) error {
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM task_dependencies
		WHERE task_id = $1 AND depends_on_task_id = $2`, taskID, dependsOnID)
	if err != nil {
		return mapPgErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTaskDependencies returns the depends-on + dependents views.
func (p *PostgresStore) ListTaskDependencies(ctx context.Context, taskID string) (*domain.TaskDependencies, error) {
	out := &domain.TaskDependencies{DependsOn: []domain.TaskDependencyRef{}, Dependents: []domain.TaskDependencyRef{}}
	dependsOn, err := p.queryDepRefs(ctx, `
		SELECT `+taskDepRefColumns+`
		FROM task_dependencies td
		JOIN tasks t ON t.id = td.depends_on_task_id
		WHERE td.task_id = $1
		ORDER BY t.task_number`, taskID)
	if err != nil {
		return nil, err
	}
	dependents, err := p.queryDepRefs(ctx, `
		SELECT `+taskDepRefColumns+`
		FROM task_dependencies td
		JOIN tasks t ON t.id = td.task_id
		WHERE td.depends_on_task_id = $1
		ORDER BY t.task_number`, taskID)
	if err != nil {
		return nil, err
	}
	out.DependsOn = dependsOn
	out.Dependents = dependents
	return out, nil
}

// UnsatisfiedDependencies returns the not-done depends-on tasks — the
// blocking set for agent pickup.
func (p *PostgresStore) UnsatisfiedDependencies(ctx context.Context, taskID string) ([]domain.TaskDependencyRef, error) {
	refs, err := p.queryDepRefs(ctx, `
		SELECT `+taskDepRefColumns+`
		FROM task_dependencies td
		JOIN tasks t ON t.id = td.depends_on_task_id
		WHERE td.task_id = $1 AND t.status <> $2
		ORDER BY t.task_number`, taskID, domain.TaskDone)
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// SearchSquadTasks finds tasks in a squad for the dependency picker.
func (p *PostgresStore) SearchSquadTasks(ctx context.Context, squadID string, q string, limit int) ([]*domain.Task, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, board_id::text, squad_id::text, title, description, status,
		       coalesce(assignee_agent_id::text, ''), created_by_type, created_by_id::text,
		       position, created_at, updated_at, coalesce(origin_message_id, ''), coalesce(workspace_resource_id, ''), coalesce(workspace_branch, ''), coalesce(workspace_commit_sha, ''), coalesce(result, ''), coalesce(result_status, ''), result_at, task_number
		FROM tasks
		WHERE squad_id = $1
		  AND ($2 = '' OR title ILIKE '%' || $2 || '%' OR task_number::text = regexp_replace($2, '^T-', '', 'i'))
		ORDER BY updated_at DESC
		LIMIT $3`, squadID, q, limit)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	var out []*domain.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, mapPgErr(rows.Err())
}

// BlockedCountsByBoard returns per-task counts of unfinished depends-on
// tasks for one board (S-278).
func (p *PostgresStore) BlockedCountsByBoard(ctx context.Context, boardID string) (map[string]int, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT td.task_id::text, count(*)::int
		FROM task_dependencies td
		JOIN tasks t ON t.id = td.task_id
		JOIN tasks p ON p.id = td.depends_on_task_id
		WHERE t.board_id = $1 AND p.status <> 'done'
		GROUP BY 1`, boardID)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, mapPgErr(err)
		}
		counts[id] = n
	}
	return counts, mapPgErr(rows.Err())
}

func (p *PostgresStore) taskDependencyRef(ctx context.Context, taskID string) (*domain.TaskDependencyRef, error) {
	var ref domain.TaskDependencyRef
	err := p.pool.QueryRow(ctx, `
		SELECT `+taskDepRefColumns+`
		FROM tasks t WHERE t.id = $1`, taskID).Scan(&ref.TaskID, &ref.TaskNumber, &ref.Title, &ref.Status)
	if err != nil {
		return nil, mapPgErr(err)
	}
	ref.Satisfied = ref.Status == domain.TaskDone
	return &ref, nil
}

func (p *PostgresStore) queryDepRefs(ctx context.Context, sql string, args ...any) ([]domain.TaskDependencyRef, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	refs := []domain.TaskDependencyRef{}
	for rows.Next() {
		var ref domain.TaskDependencyRef
		if err := rows.Scan(&ref.TaskID, &ref.TaskNumber, &ref.Title, &ref.Status, &ref.AssigneeAgentID); err != nil {
			return nil, mapPgErr(err)
		}
		ref.Satisfied = ref.Status == domain.TaskDone
		refs = append(refs, ref)
	}
	return refs, mapPgErr(rows.Err())
}
