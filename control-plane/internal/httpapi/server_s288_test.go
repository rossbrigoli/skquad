package httpapi

// S-288 (ADR-0016 D6): a task is invisible in the agent's chat until it
// is in a claimable column. Assignment/creation thread events are
// suppressed while the task sits in Backlog, and the "Task assigned to
// you" event fires when a human moves the task backlog → todo.

import (
	"net/http"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestS288AssignmentThreadEventDeferredFromBacklog(t *testing.T) {
	t.Parallel()

	handler, _, squad, agent, _ := agentRuntimeSetup(t, "S288 Deferred Squad")

	threadFor := func(taskID string) []domain.Message {
		var thread []domain.Message
		doJSON(t, handler, http.MethodGet, pathTasksPrefix+taskID+pathMessages, nil, http.StatusOK, &thread)
		return thread
	}

	// (a) Create-with-assignee in Backlog → no thread event at all.
	var parked domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Parked with assignee",
		"assignee_agent_id": agent.ID,
		"status":            "backlog",
	}, http.StatusCreated, &parked)
	require.Equal(t, domain.TaskBacklog, parked.Status)
	require.Empty(t, threadFor(parked.ID))

	// (c) Assigning later while the task is still in Backlog → still no event.
	var lateAssigned domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":  "Parked unassigned",
		"status": "backlog",
	}, http.StatusCreated, &lateAssigned)
	doJSON(t, handler, http.MethodPatch, pathTasksPrefix+lateAssigned.ID, map[string]any{
		"assignee_agent_id": agent.ID,
	}, http.StatusOK, &domain.Task{})
	require.Empty(t, threadFor(lateAssigned.ID))

	// (d) Human moves it backlog → todo → the event fires, once, as a
	// delivered user consult addressed to the assignee.
	var promoted domain.Task
	doJSON(t, handler, http.MethodPost, pathTasksPrefix+lateAssigned.ID+"/move", map[string]any{"status": "todo"}, http.StatusOK, &promoted)
	require.Equal(t, domain.TaskTodo, promoted.Status)

	thread := threadFor(lateAssigned.ID)
	require.Len(t, thread, 1)
	require.Equal(t, "user", thread[0].FromType)
	require.Equal(t, agent.ID, thread[0].ToAgentID)
	require.Equal(t, domain.MessageDelivered, thread[0].Status)
	require.Contains(t, string(thread[0].Payload), "Task assigned to you: Parked unassigned")

	// (e) Backlog → todo without an assignee → no event.
	var unassigned domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":  "Promoted but alone",
		"status": "backlog",
	}, http.StatusCreated, &unassigned)
	doJSON(t, handler, http.MethodPost, pathTasksPrefix+unassigned.ID+"/move", map[string]any{"status": "todo"}, http.StatusOK, &domain.Task{})
	require.Empty(t, threadFor(unassigned.ID))

	// (b) Regression: create-with-assignee in todo still seeds the thread
	// exactly as before (opening message = title + description).
	var live domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Straight to todo",
		"description":       "ready to run",
		"assignee_agent_id": agent.ID,
		"status":            "todo",
	}, http.StatusCreated, &live)

	liveThread := threadFor(live.ID)
	require.Len(t, liveThread, 1)
	require.Equal(t, "user", liveThread[0].FromType)
	require.Equal(t, agent.ID, liveThread[0].ToAgentID)
	require.Contains(t, string(liveThread[0].Payload), "Straight to todo")
	require.Contains(t, string(liveThread[0].Payload), "ready to run")
}

// TestS288NonBacklogAssignStillEvents pins that assign/unassign churn on
// already-claimable columns keeps today's behaviour: every new assignee
// gets exactly one "Task assigned to you" thread event.
func TestS288NonBacklogAssignStillEvents(t *testing.T) {
	t.Parallel()

	handler, _, squad, agent, _ := agentRuntimeSetup(t, "S288 Live Squad")

	var second domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "s288-second"}, http.StatusCreated, &second)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title":             "Reassign me",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	// Reassign to the second agent on a todo task → new event fires.
	// Note: the thread view is scoped to the *current* assignee, so the
	// opening seed (addressed to the previous assignee) drops out of the
	// listing — the only visible message is the new assignment event.
	doJSON(t, handler, http.MethodPatch, pathTasksPrefix+task.ID, map[string]any{
		"assignee_agent_id": second.ID,
	}, http.StatusOK, &domain.Task{})

	var thread []domain.Message
	doJSON(t, handler, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	require.Len(t, thread, 1)
	require.Contains(t, string(thread[0].Payload), "Task assigned to you: Reassign me")
	require.Equal(t, second.ID, thread[0].ToAgentID)
	require.Equal(t, "user", thread[0].FromType)
	require.Equal(t, domain.MessageDelivered, thread[0].Status)
}
