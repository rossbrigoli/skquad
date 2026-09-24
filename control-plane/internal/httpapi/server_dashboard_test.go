package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/auth"
	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// dashboardFixture creates a squad with one agent and tasks moved into the
// given statuses, plus optional metering for the squad/agent pair.
func dashboardFixture(t *testing.T, handler http.Handler, bearer, squadName string, statuses []string, inTokens, outTokens int) (domain.Squad, domain.Agent) {
	t.Helper()
	var squad domain.Squad
	doJSONAuth(t, handler, bearer, http.MethodPost, "/api/v1/squads", map[string]any{"name": squadName}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, bearer, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": squadName + " agent",
		"role": "worker",
	}, http.StatusCreated, &agent)
	for _, status := range statuses {
		var task domain.Task
		doJSONAuth(t, handler, bearer, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
			"title":             squadName + " task " + status,
			"assignee_agent_id": agent.ID,
		}, http.StatusCreated, &task)
		if status != "todo" {
			doJSONAuth(t, handler, bearer, http.MethodPost, "/api/v1/tasks/"+task.ID+"/move", map[string]any{"status": status}, http.StatusOK, &task)
		}
	}
	return squad, agent
}

func TestDashboardAdminSeesAllWithAggregates(t *testing.T) {
	t.Parallel()

	// Live provider: any HTTP answer counts as online.
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 404 still proves the endpoint answers
	}))
	t.Cleanup(live.Close)

	// Dead provider: closed listener → connection refused → offline.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	previous := dashboardProbeTimeout
	dashboardProbeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { dashboardProbeTimeout = previous })

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)

	squadA, agentA := dashboardFixture(t, handler, "", "Alpha Squad", []string{"todo", "in-progress", "in-progress"}, 1000, 200)
	dashboardFixture(t, handler, "", "Beta Squad", []string{"todo"}, 0, 0)

	// Metering: squad A total includes agent A's usage.
	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID: agentA.ID, SquadID: squadA.ID, InputTokens: 1000, OutputTokens: 200, Cost: 0.012, Currency: "USD", Timestamp: time.Now(),
	}))

	doJSONNoBody(t, handler, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name": "Live Provider", "kind": "openai", "base_url": live.URL,
	}, http.StatusCreated)
	doJSONNoBody(t, handler, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name": "Dead Provider", "kind": "openai", "base_url": deadURL,
	}, http.StatusCreated)
	doJSONNoBody(t, handler, http.MethodPost, "/api/v1/registry/skills", map[string]any{
		"name": "Deploy Skill", "description": "ships things",
	}, http.StatusCreated)

	var payload DashboardPayload
	doJSON(t, handler, http.MethodGet, "/api/v1/dashboard", nil, http.StatusOK, &payload)

	require.Equal(t, "all", payload.Scope)
	require.Len(t, payload.Squads, 2)

	byName := map[string]DashboardSquad{}
	for _, sq := range payload.Squads {
		byName[sq.Name] = sq
	}
	alpha := byName["Alpha Squad"]
	require.Equal(t, 1, alpha.TaskCounts["todo"])
	require.Equal(t, 2, alpha.TaskCounts["in-progress"])
	require.NotNil(t, alpha.Cost)
	require.Equal(t, 1000, alpha.Cost.InputTokens)
	require.Equal(t, 200, alpha.Cost.OutputTokens)
	require.InDelta(t, 0.012, alpha.Cost.Cost, 1e-9)
	require.Equal(t, "Dev Admin", alpha.OwnerName)
	require.Len(t, alpha.Agents, 1)
	require.Equal(t, agentA.ID, alpha.Agents[0].ID)
	require.NotNil(t, alpha.Agents[0].Cost)
	require.InDelta(t, 0.012, alpha.Agents[0].Cost.Cost, 1e-9)

	beta := byName["Beta Squad"]
	require.Equal(t, 1, beta.TaskCounts["todo"])
	require.Len(t, beta.Agents, 1)

	require.Len(t, payload.Providers, 2)
	provByName := map[string]DashboardProvider{}
	for _, p := range payload.Providers {
		provByName[p.Name] = p
	}
	require.True(t, provByName["Live Provider"].Online)
	require.False(t, provByName["Dead Provider"].Online)
	require.Equal(t, "unreachable", provByName["Dead Provider"].Error)

	require.Len(t, payload.Resources, 1)
	require.Equal(t, "Deploy Skill", payload.Resources[0].Name)
	require.Equal(t, domain.ResSkill, payload.Resources[0].Type)
}

func TestDashboardPersonalScopeOwnedGrantedAndExcluded(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), headerOIDC{
		"Bearer owner":    {Issuer: "https://issuer.example.com", Subject: "own-1", Email: "owner@example.com", EmailVerified: true, Name: "Owner"},
		"Bearer viewer":   {Issuer: "https://issuer.example.com", Subject: "view-1", Email: "viewer@example.com", EmailVerified: true, Name: "Viewer"},
		"Bearer stranger": {Issuer: "https://issuer.example.com", Subject: "str-1", Email: "stranger@example.com", EmailVerified: true, Name: "Stranger"},
	})

	ownedSquad, _ := dashboardFixture(t, handler, "Bearer owner", "Owned Squad", []string{"todo", "in-progress"}, 0, 0)

	// Viewer without a grant sees nothing.
	var viewerPayload DashboardPayload
	doJSONAuth(t, handler, "Bearer viewer", http.MethodGet, "/api/v1/dashboard", nil, http.StatusOK, &viewerPayload)
	require.Equal(t, "personal", viewerPayload.Scope)
	require.Empty(t, viewerPayload.Squads)

	// Grant "read" → the squad surfaces on the viewer's dashboard.
	grantBody, err := json.Marshal(map[string]any{
		"grantee_type": "user", "grantee_id": mustUserID(t, handler, "Bearer viewer"), "permissions": "read",
	})
	require.NoError(t, err)
	grantReq := httptest.NewRequest(http.MethodPost, "/api/v1/squads/"+ownedSquad.ID+"/access-grants", bytes.NewReader(grantBody))
	grantReq.Header.Set("Content-Type", "application/json")
	grantReq.Header.Set("Authorization", "Bearer owner")
	grantRec := httptest.NewRecorder()
	handler.ServeHTTP(grantRec, grantReq)
	require.Equal(t, http.StatusCreated, grantRec.Code, grantRec.Body.String())

	doJSONAuth(t, handler, "Bearer viewer", http.MethodGet, "/api/v1/dashboard", nil, http.StatusOK, &viewerPayload)
	require.Len(t, viewerPayload.Squads, 1)
	require.Equal(t, ownedSquad.ID, viewerPayload.Squads[0].ID)
	require.Equal(t, 1, viewerPayload.Squads[0].TaskCounts["in-progress"])

	// The stranger never gets the squad, even after the grant to someone else.
	var strangerPayload DashboardPayload
	doJSONAuth(t, handler, "Bearer stranger", http.MethodGet, "/api/v1/dashboard", nil, http.StatusOK, &strangerPayload)
	require.Empty(t, strangerPayload.Squads)
}

func TestDashboardUnauthenticated(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), fakeOIDC{err: auth.ErrUnauthorized})

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodGet, "/api/v1/dashboard", nil, http.StatusUnauthorized, &body)
	require.Equal(t, "unauthorized", body["error"]["code"])
}

func mustUserID(t *testing.T, handler http.Handler, bearer string) string {
	t.Helper()
	var u domain.User
	doJSONAuth(t, handler, bearer, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &u)
	require.NotEmpty(t, u.ID)
	return u.ID
}
