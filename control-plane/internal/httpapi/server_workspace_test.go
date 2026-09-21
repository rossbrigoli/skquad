package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// registerWorkspace POSTs a project-workspace and returns the created
// resource (or asserts the wanted failure status).
func registerWorkspace(t *testing.T, h http.Handler, body map[string]any, want int) domain.RegistryResource {
	t.Helper()
	var res domain.RegistryResource
	doJSON(t, h, http.MethodPost, "/api/v1/registry/project-workspaces", body, want, &res)
	return res
}

func TestGitWorkspaceRegistrationValidation(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	// Valid git workspace.
	valid := registerWorkspace(t, h, map[string]any{
		"name":     "team-repo",
		"endpoint": "https://git.example.com/team/repo.git",
		"auth_ref": "secret/git/team-repo",
		"manifest": map[string]any{"kind": "git", "default_branch": "main"},
	}, http.StatusCreated)
	require.Equal(t, domain.ResProjectWorkspace, valid.Type)
	require.NotEmpty(t, valid.ID)

	// kind must be "git".
	registerWorkspace(t, h, map[string]any{
		"name":     "no-kind",
		"endpoint": "https://git.example.com/team/repo.git",
		"auth_ref": "secret/git/x",
		"manifest": map[string]any{"default_branch": "main"},
	}, http.StatusBadRequest)

	// default_branch required.
	registerWorkspace(t, h, map[string]any{
		"name":     "no-branch",
		"endpoint": "https://git.example.com/team/repo.git",
		"auth_ref": "secret/git/x",
		"manifest": map[string]any{"kind": "git"},
	}, http.StatusBadRequest)

	// auth_ref required.
	registerWorkspace(t, h, map[string]any{
		"name":     "no-auth",
		"endpoint": "https://git.example.com/team/repo.git",
		"manifest": map[string]any{"kind": "git", "default_branch": "main"},
	}, http.StatusBadRequest)

	// endpoint required.
	registerWorkspace(t, h, map[string]any{
		"name":     "no-endpoint",
		"auth_ref": "secret/git/x",
		"manifest": map[string]any{"kind": "git", "default_branch": "main"},
	}, http.StatusBadRequest)
}

func TestTaskWorkspaceLinkage(t *testing.T) {
	f := newDelegationFixture(t)
	h := f.handler

	ws := registerWorkspace(t, h, map[string]any{
		"name":     "link-repo",
		"endpoint": "https://git.example.com/team/repo.git",
		"auth_ref": "secret/git/link",
		"manifest": map[string]any{"kind": "git", "default_branch": "main"},
	}, http.StatusCreated)

	// Grant the workspace to the worker agent.
	var perms []domain.AgentPermission
	doJSON(t, h, http.MethodPut, "/api/v1/agents/"+f.workerID+"/permissions", []map[string]string{
		{"resource_type": string(domain.ResProjectWorkspace), "resource_id": ws.ID},
	}, http.StatusOK, &perms)
	require.Len(t, perms, 1)

	// Materialize a task assigned to the worker via a delegate message.
	sent := f.delegate(t, "delegate", map[string]any{"message": "Do the workspace thing"})
	var payload map[string]any
	require.NoError(t, json.Unmarshal(sent.Payload, &payload))
	taskID, ok := payload["task_id"].(string)
	require.True(t, ok)

	// Worker reports the branch + commit it pushed.
	branch := "skquad/worker/" + taskID
	var updated domain.Task
	doAgentJSON(t, h, f.workerID, f.workerCred, http.MethodPost,
		"/api/v1/agents/me/tasks/"+taskID+"/workspace",
		map[string]any{"workspace_resource_id": ws.ID, "branch": branch, "commit_sha": "abc123def"},
		http.StatusOK, &updated)
	require.Equal(t, ws.ID, updated.WorkspaceResourceID)
	require.Equal(t, branch, updated.WorkspaceBranch)
	require.Equal(t, "abc123def", updated.WorkspaceCommitSHA)

	// Reading the task back shows the linkage persisted.
	var reread domain.Task
	doJSON(t, h, http.MethodGet, "/api/v1/tasks/"+taskID, nil, http.StatusOK, &reread)
	require.Equal(t, branch, reread.WorkspaceBranch)

	// Reporting a workspace the worker is NOT granted → 403.
	other := registerWorkspace(t, h, map[string]any{
		"name":     "other-repo",
		"endpoint": "https://git.example.com/team/other.git",
		"auth_ref": "secret/git/other",
		"manifest": map[string]any{"kind": "git", "default_branch": "main"},
	}, http.StatusCreated)
	doAgentJSONNoBody(t, h, f.workerID, f.workerCred, http.MethodPost,
		"/api/v1/agents/me/tasks/"+taskID+"/workspace",
		map[string]any{"workspace_resource_id": other.ID, "branch": "x", "commit_sha": "y"},
		http.StatusForbidden)

	// A non-assignee (the sender) cannot link workspace refs to the worker's task.
	doAgentJSONNoBody(t, h, f.senderID, f.senderCred, http.MethodPost,
		"/api/v1/agents/me/tasks/"+taskID+"/workspace",
		map[string]any{"workspace_resource_id": ws.ID, "branch": "x", "commit_sha": "y"},
		http.StatusForbidden)
}
