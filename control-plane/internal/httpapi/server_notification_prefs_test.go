package httpapi

// S-199: per-user notification mute preferences.
//
// Coverage:
//   - default (no preference row) ⇒ all types delivered
//   - muted type ⇒ delivery skipped (no error, no row)
//   - unmute ⇒ delivery resumes
//   - PUT validation: unknown type ⇒ 400; duplicates collapsed
//   - GET reflects stored state

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const pathNotifPrefs = "/api/v1/notifications/preferences"

func listNotificationsFor(t *testing.T, handler http.Handler) []domain.Notification {
	t.Helper()
	var items []domain.Notification
	doJSON(t, handler, http.MethodGet, "/api/v1/notifications", nil, http.StatusOK, &items)
	return items
}

func countType(items []domain.Notification, kind domain.NotificationType) int {
	n := 0
	for _, it := range items {
		if it.Type == kind {
			n++
		}
	}
	return n
}

// blockFreshTask creates, claims and blocks a task so the control plane
// emits one task_blocked notification for the squad owner.
func blockFreshTask(t *testing.T, handler http.Handler, squad domain.Squad, agent domain.Agent, credential, title string) {
	t.Helper()
	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             title,
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/block", map[string]any{
		"summary":       "need input",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &domain.Task{})
}

func TestNotificationPreferencesDefaultAllEnabled(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "prefs-default")

	var prefs domain.NotificationPreferences
	doJSON(t, handler, http.MethodGet, pathNotifPrefs, nil, http.StatusOK, &prefs)
	require.Empty(t, prefs.MutedTypes)

	// No preference row exists yet: the blocked alert is delivered.
	blockFreshTask(t, handler, squad, agent, credential, "Default delivery")
	require.Equal(t, 1, countType(listNotificationsFor(t, handler), domain.NotificationTaskBlocked))
}

func TestNotificationPreferencesMuteSkipsDelivery(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "prefs-mute")

	// Baseline: unmuted delivery works.
	blockFreshTask(t, handler, squad, agent, credential, "Before mute")
	require.Equal(t, 1, countType(listNotificationsFor(t, handler), domain.NotificationTaskBlocked))

	doJSON(t, handler, http.MethodPut, pathNotifPrefs, map[string]any{
		"muted_types": []string{"task_blocked"},
	}, http.StatusOK, &domain.NotificationPreferences{})

	var prefs domain.NotificationPreferences
	doJSON(t, handler, http.MethodGet, pathNotifPrefs, nil, http.StatusOK, &prefs)
	require.Equal(t, []domain.NotificationType{domain.NotificationTaskBlocked}, prefs.MutedTypes)

	// Muted: the blocked alert is skipped — count unchanged, no error.
	blockFreshTask(t, handler, squad, agent, credential, "While muted")
	require.Equal(t, 1, countType(listNotificationsFor(t, handler), domain.NotificationTaskBlocked))

	// Unmute: delivery resumes.
	doJSON(t, handler, http.MethodPut, pathNotifPrefs, map[string]any{"muted_types": []string{}},
		http.StatusOK, &domain.NotificationPreferences{})
	blockFreshTask(t, handler, squad, agent, credential, "After unmute")
	require.Equal(t, 2, countType(listNotificationsFor(t, handler), domain.NotificationTaskBlocked))
}

func TestNotificationPreferencesMuteDoesNotAffectOtherTypes(t *testing.T) {
	handler, _, squad, agent, credential := agentRuntimeSetup(t, "prefs-other")

	doJSON(t, handler, http.MethodPut, pathNotifPrefs, map[string]any{
		"muted_types": []string{"task_failed"},
	}, http.StatusOK, &domain.NotificationPreferences{})

	// task_blocked is untouched by a task_failed mute.
	blockFreshTask(t, handler, squad, agent, credential, "Blocked but not muted")
	require.Equal(t, 1, countType(listNotificationsFor(t, handler), domain.NotificationTaskBlocked))
	require.Equal(t, 0, countType(listNotificationsFor(t, handler), domain.NotificationTaskFailed))
}

func TestNotificationPreferencesRejectsUnknownType(t *testing.T) {
	handler, _, _, _, _ := agentRuntimeSetup(t, "prefs-bad")
	doJSONNoBody(t, handler, http.MethodPut, pathNotifPrefs, map[string]any{
		"muted_types": []string{"task_failed", "not_a_real_type"},
	}, http.StatusBadRequest)

	// Nothing was stored by the rejected request.
	var prefs domain.NotificationPreferences
	doJSON(t, handler, http.MethodGet, pathNotifPrefs, nil, http.StatusOK, &prefs)
	require.Empty(t, prefs.MutedTypes)
}

func TestNotificationPreferencesCollapsesDuplicates(t *testing.T) {
	handler, _, _, _, _ := agentRuntimeSetup(t, "prefs-dupe")
	doJSONNoBody(t, handler, http.MethodPut, pathNotifPrefs, map[string]any{
		"muted_types": []string{"agent_died", "agent_died", "task_stuck"},
	}, http.StatusOK)

	var prefs domain.NotificationPreferences
	doJSON(t, handler, http.MethodGet, pathNotifPrefs, nil, http.StatusOK, &prefs)
	require.ElementsMatch(t,
		[]domain.NotificationType{domain.NotificationAgentDied, domain.NotificationTaskStuck},
		prefs.MutedTypes)
}
