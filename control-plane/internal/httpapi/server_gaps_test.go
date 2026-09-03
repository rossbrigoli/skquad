package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// doRaw sends a literal request body so malformed payloads can be exercised.
func doRaw(t *testing.T, handler http.Handler, method, path, body string, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func errorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body["error"]
}

// promoteAdmin provisions the principal behind authorization through OIDC, then
// grants it the platform_admin role directly in storage (there is no bootstrap
// admin in the API surface by design).
func promoteAdmin(t *testing.T, store *storage.MemoryStore, handler http.Handler, authorization string) domain.User {
	t.Helper()
	var user domain.User
	doJSONAuth(t, handler, authorization, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &user)
	require.NoError(t, store.SetUserRole(context.Background(), user.ID, domain.RolePlatformAdmin))
	user.Role = domain.RolePlatformAdmin
	return user
}

func newCRBackedHandler() (http.Handler, *fakeCRWriter) {
	crWriter := &fakeCRWriter{}
	return NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter), crWriter
}

// agentRuntimeSetup returns a handler plus a ready-to-use agent credential.
func agentRuntimeSetup(t *testing.T, squadName string) (http.Handler, *fakeCRWriter, domain.Squad, domain.Agent, string) {
	t.Helper()
	handler, crWriter := newCRBackedHandler()

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": squadName}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "worker"}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)

	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)
	return handler, crWriter, squad, agent, credential
}

// failingCRWriter fails the n-th WriteAgentCredential call so credential
// rollback can be observed.
type failingCRWriter struct {
	writeCalls  []string
	deleteCalls []string
	failOnWrite int
}

func (f *failingCRWriter) UpsertSquad(context.Context, *domain.Squad) error { return nil }
func (f *failingCRWriter) DeleteSquad(context.Context, *domain.Squad) error { return nil }
func (f *failingCRWriter) UpsertAgent(context.Context, *domain.Agent, *domain.AgentIdentity) error {
	return nil
}
func (f *failingCRWriter) DeleteAgent(context.Context, *domain.Agent) error { return nil }

func (f *failingCRWriter) WriteAgentCredential(_ context.Context, ref, _, _ string) error {
	f.writeCalls = append(f.writeCalls, ref)
	if f.failOnWrite == len(f.writeCalls) {
		return errors.New("secret write failed")
	}
	return nil
}

func (f *failingCRWriter) DeleteAgentCredential(_ context.Context, ref string) error {
	f.deleteCalls = append(f.deleteCalls, ref)
	return nil
}

// TestAgentIdentityWithoutCRWriter covers the no-Kubernetes deployment path: the
// API server runs with a noop CR writer, so identity creation must still succeed
// rather than hard-failing on a missing cluster.
func TestAgentIdentityWithoutCRWriter(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "No CR Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "worker"}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	require.NotEmpty(t, identity.CredentialRef)
	require.NotEmpty(t, identity.VirtualKeyRef)

	var rotated domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity/rotate", nil, http.StatusOK, &rotated)
	require.NotEmpty(t, rotated.CredentialRef)
}

func TestAgentIdentityRollsBackSecretsWhenWriteFails(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	crWriter := &failingCRWriter{failOnWrite: 2}
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Rollback Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "worker"}, http.StatusCreated, &agent)

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusInternalServerError, &body)
	require.Equal(t, "internal", body["error"]["code"])

	// The already-written credential secret must be revoked, and no identity may
	// be persisted without its secrets.
	require.Len(t, crWriter.writeCalls, 2)
	require.Equal(t, crWriter.writeCalls[0], crWriter.deleteCalls[0])

	_, err := store.GetAgentIdentity(context.Background(), agent.ID)
	require.ErrorIs(t, err, storage.ErrNotFound)

	// A second attempt must not leave a half-written identity behind either.
	crWriter.failOnWrite = 4
	var retryBody map[string]map[string]string
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusInternalServerError, &retryBody)
	_, err = store.GetAgentIdentity(context.Background(), agent.ID)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestHealthzServesUnauthenticatedProbes(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())
	rec := doRaw(t, handler, http.MethodGet, "/healthz", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestNewWithDependenciesWiresOIDCAndCRWriter(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithDependencies(cfg, store, headerOIDC{
		"Bearer admin": {Email: "admin@example.com", Name: "Admin"},
	}, crWriter)
	promoteAdmin(t, store, handler, "Bearer admin")

	// OIDC is enforced: no bearer token must not reach the handler.
	rec := doRaw(t, handler, http.MethodGet, "/api/v1/squads", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Wired Squad",
	}, http.StatusCreated, &squad)
	// Mutations are queued for the Kubernetes outbox worker, which owns the
	// CRWriter; assert the wiring produced the event.
	events, err := store.ListKubernetesOutbox(context.Background(), "", 20)
	require.NoError(t, err)
	require.Contains(t, outboxOperations(events), domain.KubernetesOpUpsertSquad+":"+squad.ID)
}

func TestListSquadsScopesToOwnerUnlessAdminRequestsAll(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer alice": {Email: "alice@example.com", Name: "Alice"},
		"Bearer bob":   {Email: "bob@example.com", Name: "Bob"},
		"Bearer admin": {Email: "admin@example.com", Name: "Admin"},
	})
	promoteAdmin(t, store, handler, "Bearer admin")

	var aliceSquad, bobSquad domain.Squad
	doJSONAuth(t, handler, "Bearer alice", http.MethodPost, "/api/v1/squads", map[string]any{"name": "Alice Squad"}, http.StatusCreated, &aliceSquad)
	doJSONAuth(t, handler, "Bearer bob", http.MethodPost, "/api/v1/squads", map[string]any{"name": "Bob Squad"}, http.StatusCreated, &bobSquad)

	var aliceSquads []domain.Squad
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/squads", nil, http.StatusOK, &aliceSquads)
	require.Len(t, aliceSquads, 1)
	require.Equal(t, aliceSquad.ID, aliceSquads[0].ID)

	// ?all=true only widens the scope for platform admins.
	doJSONAuth(t, handler, "Bearer alice", http.MethodGet, "/api/v1/squads?all=true", nil, http.StatusOK, &aliceSquads)
	require.Len(t, aliceSquads, 1, "non-admin must not be able to list every squad")

	var allSquads []domain.Squad
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/squads?all=true", nil, http.StatusOK, &allSquads)
	require.Len(t, allSquads, 2)

	var adminOwn []domain.Squad
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/squads", nil, http.StatusOK, &adminOwn)
	require.Empty(t, adminOwn, "admin without ?all=true sees only squads they own")
}

func TestListAgentsAndGrantsEnforceSquadAccess(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), headerOIDC{
		"Bearer owner":  {Email: "owner@example.com", Name: "Owner"},
		"Bearer reader": {Email: "reader@example.com", Name: "Reader"},
	})

	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads", map[string]any{"name": "Listed Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "Listed Agent"}, http.StatusCreated, &agent)

	var agents []domain.Agent
	doJSONAuth(t, handler, "Bearer owner", http.MethodGet, "/api/v1/squads/"+squad.ID+"/agents", nil, http.StatusOK, &agents)
	require.Len(t, agents, 1)
	require.Equal(t, agent.ID, agents[0].ID)

	var denied map[string]map[string]string
	doJSONAuth(t, handler, "Bearer reader", http.MethodGet, "/api/v1/squads/"+squad.ID+"/agents", nil, http.StatusForbidden, &denied)
	require.Equal(t, "forbidden", denied["error"]["code"])

	// listGrants is owner-only: a read grant is not enough.
	var viewer domain.User
	doJSONAuth(t, handler, "Bearer reader", http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &viewer)
	var grant domain.AccessGrant
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads/"+squad.ID+"/access-grants", map[string]any{
		"grantee_type": "user",
		"grantee_id":   viewer.ID,
		"permissions":  "read",
	}, http.StatusCreated, &grant)

	var grants []domain.AccessGrant
	doJSONAuth(t, handler, "Bearer owner", http.MethodGet, "/api/v1/squads/"+squad.ID+"/access-grants", nil, http.StatusOK, &grants)
	require.Len(t, grants, 1)
	require.Equal(t, grant.ID, grants[0].ID)

	var forbiddenGrants map[string]map[string]string
	doJSONAuth(t, handler, "Bearer reader", http.MethodGet, "/api/v1/squads/"+squad.ID+"/access-grants", nil, http.StatusForbidden, &forbiddenGrants)
	require.Equal(t, "forbidden", forbiddenGrants["error"]["code"])
}

func TestListAuditRequiresPlatformAdminAndFilters(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer admin": {Email: "admin@example.com", Name: "Admin"},
		"Bearer user":  {Email: "user@example.com", Name: "User"},
	})

	var denied map[string]map[string]string
	doJSONAuth(t, handler, "Bearer user", http.MethodGet, "/api/v1/audit", nil, http.StatusForbidden, &denied)
	promoteAdmin(t, store, handler, "Bearer admin")
	require.Equal(t, "forbidden", denied["error"]["code"])

	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/squads", map[string]any{"name": "Audited Squad"}, http.StatusCreated, &squad)

	var entries []domain.AuditEntry
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/audit?squad_id="+squad.ID, nil, http.StatusOK, &entries)
	require.Contains(t, auditActions(entries), "squad.create")

	// A squad_id that matches nothing must not leak other squads' entries.
	var filtered []domain.AuditEntry
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/audit?squad_id=does-not-exist", nil, http.StatusOK, &filtered)
	require.Empty(t, filtered)

	var limited []domain.AuditEntry
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/audit?limit=1", nil, http.StatusOK, &limited)
	require.Len(t, limited, 1)
}

func TestRegistryResourceListingAndUnknownType(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var created domain.RegistryResource
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/tools", map[string]any{
		"name":     "git",
		"endpoint": "https://git.example.com",
	}, http.StatusCreated, &created)

	var resources []domain.RegistryResource
	doJSON(t, handler, http.MethodGet, "/api/v1/registry/tools", nil, http.StatusOK, &resources)
	require.Len(t, resources, 1)
	require.Equal(t, created.ID, resources[0].ID)

	// A different type must not return the tool.
	var empty []domain.RegistryResource
	doJSON(t, handler, http.MethodGet, "/api/v1/registry/skills", nil, http.StatusOK, &empty)
	require.Empty(t, empty)

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodGet, "/api/v1/registry/not-a-type", nil, http.StatusNotFound, &body)
	require.Equal(t, "not_found", body["error"]["code"])
}

func TestGetAndDeprecateLLMProvider(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer admin": {Email: "admin@example.com", Name: "Admin"},
		"Bearer user":  {Email: "user@example.com", Name: "User"},
	})
	promoteAdmin(t, store, handler, "Bearer admin")

	var provider domain.LLMProvider
	doJSONAuth(t, handler, "Bearer admin", http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":     "OpenAI",
		"kind":     "openai",
		"base_url": "https://api.openai.com/v1",
	}, http.StatusCreated, &provider)

	var fetched domain.LLMProvider
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/registry/llm-providers/"+provider.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, provider.ID, fetched.ID)
	require.Equal(t, domain.ResourceActive, fetched.Status)

	// Reads are not admin-only, but a missing provider is still a 404.
	doJSONAuth(t, handler, "Bearer user", http.MethodGet, "/api/v1/registry/llm-providers/"+provider.ID, nil, http.StatusOK, &fetched)

	var missing map[string]map[string]string
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/registry/llm-providers/nope", nil, http.StatusNotFound, &missing)
	require.Equal(t, "not_found", missing["error"]["code"])

	var denied map[string]map[string]string
	doJSONAuth(t, handler, "Bearer user", http.MethodPost, "/api/v1/registry/llm-providers/"+provider.ID+"/deprecate", nil, http.StatusForbidden, &denied)
	require.Equal(t, "forbidden", denied["error"]["code"])

	req := httptest.NewRequest(http.MethodPost, "/api/v1/registry/llm-providers/"+provider.ID+"/deprecate", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/registry/llm-providers/"+provider.ID, nil, http.StatusOK, &fetched)
	require.Equal(t, domain.ResourceDeprecated, fetched.Status)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/registry/llm-providers/does-not-exist/deprecate", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	var audit []domain.AuditEntry
	doJSONAuth(t, handler, "Bearer admin", http.MethodGet, "/api/v1/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "registry.llm_provider.deprecate")
}

func TestAgentStartsAssignedTaskAndBlocksUnassignedOnes(t *testing.T) {
	t.Parallel()

	handler, crWriter, squad, agent, credential := agentRuntimeSetup(t, "Start Squad")

	// A second agent in the same squad must not be able to start agent 1's task.
	var other domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "other"}, http.StatusCreated, &other)
	var otherIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+other.ID+"/identity", nil, http.StatusCreated, &otherIdentity)
	otherCredential := crWriter.credentialTokens[otherIdentity.CredentialRef]

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Start me",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var forbidden map[string]map[string]string
	doAgentJSON(t, handler, other.ID, otherCredential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/start", nil, http.StatusForbidden, &forbidden)
	require.Equal(t, "forbidden", forbidden["error"]["code"])
	require.Equal(t, "task is not assigned to this agent", forbidden["error"]["message"])

	var started domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/start", nil, http.StatusOK, &started)
	require.Equal(t, domain.TaskInProgress, started.Status)

	var busy domain.Agent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID, nil, http.StatusOK, &busy)
	require.Equal(t, domain.AgentBusy, busy.Status)

	var body map[string]map[string]string
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/does-not-exist/start", nil, http.StatusNotFound, &body)
	require.Equal(t, "not_found", body["error"]["code"])

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "task.start")
}

func TestAgentBlocksTaskWithLeaseAndRejectsStaleFence(t *testing.T) {
	t.Parallel()

	handler, _, squad, agent, credential := agentRuntimeSetup(t, "Block Squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Block me",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)

	var body map[string]map[string]string
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/block", map[string]any{}, http.StatusBadRequest, &body)
	require.Equal(t, "execution_id is required", body["error"]["message"])

	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/block", map[string]any{
		"execution_id": claimed.ExecutionID,
	}, http.StatusBadRequest, &body)
	require.Equal(t, "fencing_token is required", body["error"]["message"])

	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/block", map[string]any{
		"execution_id":  claimed.ExecutionID,
		"fencing_token": "stale-token",
	}, http.StatusConflict, &body)
	require.Equal(t, "conflict", body["error"]["code"])

	var blocked domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/block", map[string]any{
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
		"summary":       "waiting on credentials",
	}, http.StatusOK, &blocked)
	require.Equal(t, domain.TaskBlocked, blocked.Status)

	// A blocked task leaves no assigned work pending, so the agent goes idle.
	var idle domain.Agent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID, nil, http.StatusOK, &idle)
	require.Equal(t, domain.AgentIdle, idle.Status)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "task.block")
}

func TestUpdateTaskValidatesPayloadAndAssignee(t *testing.T) {
	t.Parallel()

	handler, _, squad, agent, _ := agentRuntimeSetup(t, "Update Squad")

	var otherSquad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Other Squad"}, http.StatusCreated, &otherSquad)
	var foreignAgent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+otherSquad.ID+"/agents", map[string]any{"name": "foreign"}, http.StatusCreated, &foreignAgent)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Original title",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var updated domain.Task
	doJSON(t, handler, http.MethodPatch, "/api/v1/tasks/"+task.ID, map[string]any{
		"title":       "Renamed",
		"description": "updated description",
	}, http.StatusOK, &updated)
	require.Equal(t, "Renamed", updated.Title)
	require.Equal(t, "updated description", updated.Description)

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPatch, "/api/v1/tasks/"+task.ID, map[string]any{"title": "   "}, http.StatusBadRequest, &body)
	require.Equal(t, "title must not be empty", body["error"]["message"])

	// Assigning to an agent from another squad would break squad isolation.
	doJSON(t, handler, http.MethodPatch, "/api/v1/tasks/"+task.ID, map[string]any{
		"assignee_agent_id": foreignAgent.ID,
	}, http.StatusBadRequest, &body)
	require.Equal(t, "assignee_agent_id must belong to this squad", body["error"]["message"])

	doJSON(t, handler, http.MethodPatch, "/api/v1/tasks/"+task.ID, map[string]any{
		"assignee_agent_id": "does-not-exist",
	}, http.StatusNotFound, &body)
	require.Equal(t, "not_found", body["error"]["code"])

	// Unassigning is allowed and clears the mirror on the previous assignee.
	var unassigned domain.Task
	doJSON(t, handler, http.MethodPatch, "/api/v1/tasks/"+task.ID, map[string]any{
		"assignee_agent_id": "",
	}, http.StatusOK, &unassigned)
	require.Empty(t, unassigned.AssigneeAgentID)

	rec := doRaw(t, handler, http.MethodPatch, "/api/v1/tasks/"+task.ID, "{not json", "")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "bad_request", errorEnvelope(t, rec)["code"])

	rec = doRaw(t, handler, http.MethodPatch, "/api/v1/tasks/does-not-exist", `{"title":"x"}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "task.update")
}

func TestUpdateTaskDeniedForReadonlyGrant(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), headerOIDC{
		"Bearer owner":  {Email: "owner@example.com", Name: "Owner"},
		"Bearer reader": {Email: "reader@example.com", Name: "Reader"},
	})

	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads", map[string]any{"name": "Guarded Squad"}, http.StatusCreated, &squad)
	var task domain.Task
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title": "Guarded task",
	}, http.StatusCreated, &task)

	var viewer domain.User
	doJSONAuth(t, handler, "Bearer reader", http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &viewer)
	var grant domain.AccessGrant
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads/"+squad.ID+"/access-grants", map[string]any{
		"grantee_type": "user",
		"grantee_id":   viewer.ID,
		"permissions":  "read",
	}, http.StatusCreated, &grant)

	// Read access can fetch the task but not mutate it.
	var readable domain.Task
	doJSONAuth(t, handler, "Bearer reader", http.MethodGet, "/api/v1/tasks/"+task.ID, nil, http.StatusOK, &readable)

	var forbidden map[string]map[string]string
	doJSONAuth(t, handler, "Bearer reader", http.MethodPatch, "/api/v1/tasks/"+task.ID, map[string]any{"title": "hijacked"}, http.StatusForbidden, &forbidden)
	require.Equal(t, "forbidden", forbidden["error"]["code"])

	doJSONAuth(t, handler, "Bearer reader", http.MethodPost, "/api/v1/tasks/"+task.ID+"/move", map[string]any{"status": "done"}, http.StatusForbidden, &forbidden)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/tasks/"+task.ID, nil)
	req.Header.Set("Authorization", "Bearer reader")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A stranger with no grant at all cannot even read it.
	var strangerBody map[string]map[string]string
	doJSONAuth(t, handler, "Bearer reader", http.MethodGet, "/api/v1/tasks/does-not-exist", nil, http.StatusNotFound, &strangerBody)
}

func TestDeleteTaskRemovesItAndSyncsAssignedAgent(t *testing.T) {
	t.Parallel()

	handler, _, squad, agent, _ := agentRuntimeSetup(t, "Delete Squad")

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Delete me",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	// Assigning pending work marks the agent busy; deleting it must release them.
	var busy domain.Agent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID, nil, http.StatusOK, &busy)
	require.Equal(t, domain.AgentBusy, busy.Status)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/tasks/"+task.ID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodGet, "/api/v1/tasks/"+task.ID, nil, http.StatusNotFound, &body)

	var idle domain.Agent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID, nil, http.StatusOK, &idle)
	require.Equal(t, domain.AgentIdle, idle.Status)

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/tasks/does-not-exist", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "task.delete")
}
