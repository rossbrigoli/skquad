package domain

// S-228: only TO DO column tasks are claimable by agents (in-progress is
// the crash-resume exception). Backlog and every other column must never
// satisfy the pickup allowlist.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentPickupStatusesAllowlist(t *testing.T) {
	require.ElementsMatch(t, []TaskStatus{TaskTodo, TaskInProgress}, AgentPickupStatuses())
	require.NotContains(t, AgentPickupStatuses(), TaskBacklog)

	require.True(t, TaskTodo.IsAgentPickupStatus())
	require.True(t, TaskInProgress.IsAgentPickupStatus())

	for _, s := range []TaskStatus{TaskBacklog, TaskInReview, TaskDone, TaskBlocked, TaskStatus("icebox")} {
		require.False(t, s.IsAgentPickupStatus(), "%q must not be agent-pickupable", s)
	}
}
