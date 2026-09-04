package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestSquadAgentTaskFlow(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name":    "Core Squad",
		"mission": "ship the control plane",
	}, http.StatusCreated, &squad)
	require.NotEmpty(t, squad.ID)
	require.Equal(t, "Core Squad", squad.Name)
	require.NotEmpty(t, squad.Namespace)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name":                "Architect",
		"role":                "technical lead",
		"system_prompt":       "You are a pragmatic architecture lead.",
		"default_provider_id": "",
		"default_model":       "openai/gpt-4o-mini",
	}, http.StatusCreated, &agent)
	require.NotEmpty(t, agent.ID)
	require.Equal(t, squad.ID, agent.SquadID)
	require.Equal(t, "openai/gpt-4o-mini", agent.DefaultModel)
	require.Equal(t, "You are a pragmatic architecture lead.", agent.SystemPrompt)
	require.Equal(t, 300, agent.IdleTimeoutSec)

	var patchedAgent domain.Agent
	doJSON(t, handler, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
		"system_prompt": "You review designs and call out risk.",
	}, http.StatusOK, &patchedAgent)
	require.Equal(t, "You review designs and call out risk.", patchedAgent.SystemPrompt)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Design API slice",
		"description":       "first vertical slice",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)
	require.NotEmpty(t, task.ID)
	require.Equal(t, domain.TaskTodo, task.Status)
	require.Equal(t, agent.ID, task.AssigneeAgentID)

	var moved domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/tasks/"+task.ID+"/move", map[string]any{
		"status": "in-progress",
	}, http.StatusOK, &moved)
	require.Equal(t, domain.TaskInProgress, moved.Status)

	var board struct {
		Board domain.Board  `json:"board"`
		Tasks []domain.Task `json:"tasks"`
	}
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/board", nil, http.StatusOK, &board)
	require.Equal(t, squad.ID, board.Board.SquadID)
	require.Len(t, board.Tasks, 1)
	require.Equal(t, moved.ID, board.Tasks[0].ID)
}

func TestValidationErrorEnvelope(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "",
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])
}

func TestTaskStatusValidationUsesErrorEnvelope(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Status Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Status Agent",
	}, http.StatusCreated, &agent)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Validate status",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPost, "/api/v1/tasks/"+task.ID+"/move", map[string]any{
		"status": "not-real",
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])
	require.Equal(t, "status is invalid", body["error"]["message"])

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status": "todo",
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])
	require.Equal(t, "status must be in-review or done", body["error"]["message"])

	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/heartbeat", map[string]any{
		"status": "paused",
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])
	require.Equal(t, "status is invalid", body["error"]["message"])
}

func TestOIDCAuthProvisionsUser(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), fakeOIDC{
		profile: &auth.Profile{
			Issuer:        "https://issuer.example.com",
			Subject:       "subject-1",
			Email:         "User@Example.com",
			EmailVerified: true,
			Name:          "OIDC User",
		},
	})

	var user domain.User
	doJSON(t, handler, http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &user)
	require.Equal(t, "user@example.com", user.Email)
	require.Equal(t, "https://issuer.example.com", user.OIDCIssuer)
	require.Equal(t, "subject-1", user.OIDCSubject)
	require.True(t, user.EmailVerified)
	require.Equal(t, "OIDC User", user.Name)
	require.Equal(t, domain.RoleUser, user.Role)
}

func TestOIDCAuthKeysUsersByIssuerAndSubject(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	profiles := headerOIDC{
		"Bearer first": {
			Issuer:        "https://issuer.example.com",
			Subject:       "subject-a",
			Email:         "shared@example.com",
			EmailVerified: true,
			Name:          "First",
		},
		"Bearer second": {
			Issuer:        "https://issuer.example.com",
			Subject:       "subject-b",
			Email:         "shared@example.com",
			EmailVerified: true,
			Name:          "Second",
		},
	}
	handler := NewWithOIDCAuthenticator(cfg, store, profiles)

	var first domain.User
	doJSONAuth(t, handler, "Bearer first", http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &first)
	var second domain.User
	doJSONAuth(t, handler, "Bearer second", http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &second)

	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, first.Email, second.Email)
	require.Equal(t, "subject-a", first.OIDCSubject)
	require.Equal(t, "subject-b", second.OIDCSubject)
}

func TestOIDCAuthRejectsInvalidBearer(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), fakeOIDC{
		err: auth.ErrUnauthorized,
	})

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodGet, "/api/v1/auth/me", nil, http.StatusUnauthorized, &body)
	require.Equal(t, "unauthorized", body["error"]["code"])
}

func TestAccessGrantAllowsReadButNotWrite(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		"Bearer owner":  {Email: "owner@example.com", Name: "Owner"},
		"Bearer viewer": {Email: "viewer@example.com", Name: "Viewer"},
	})

	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Shared Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Shared Agent",
	}, http.StatusCreated, &agent)

	var viewer domain.User
	doJSONAuth(t, handler, "Bearer viewer", http.MethodGet, "/api/v1/auth/me", nil, http.StatusOK, &viewer)

	var denied map[string]map[string]string
	doJSONAuth(t, handler, "Bearer viewer", http.MethodGet, "/api/v1/squads/"+squad.ID, nil, http.StatusForbidden, &denied)

	var grant domain.AccessGrant
	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads/"+squad.ID+"/access-grants", map[string]any{
		"grantee_type": "user",
		"grantee_id":   viewer.ID,
		"permissions":  "talk",
	}, http.StatusCreated, &grant)
	require.Equal(t, viewer.ID, grant.GranteeID)

	doJSONAuth(t, handler, "Bearer viewer", http.MethodGet, "/api/v1/squads/"+squad.ID, nil, http.StatusForbidden, &denied)

	var sent domain.Message
	doJSONAuth(t, handler, "Bearer viewer", http.MethodPost, "/api/v1/agents/"+agent.ID+"/chat", map[string]any{
		"message": "talk grant allows chat",
	}, http.StatusCreated, &sent)
	require.Equal(t, viewer.ID, sent.FromID)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/access-grants/"+grant.ID, nil)
	req.Header.Set("Authorization", "Bearer owner")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	doJSONAuth(t, handler, "Bearer owner", http.MethodPost, "/api/v1/squads/"+squad.ID+"/access-grants", map[string]any{
		"grantee_type": "user",
		"grantee_id":   viewer.ID,
		"permissions":  "read",
	}, http.StatusCreated, &grant)

	var readable domain.Squad
	doJSONAuth(t, handler, "Bearer viewer", http.MethodGet, "/api/v1/squads/"+squad.ID, nil, http.StatusOK, &readable)
	require.Equal(t, squad.ID, readable.ID)

	var forbidden map[string]map[string]string
	var history []domain.Message
	doJSONAuth(t, handler, "Bearer viewer", http.MethodGet, "/api/v1/agents/"+agent.ID+"/chat", nil, http.StatusOK, &history)
	require.Len(t, history, 1)

	doJSONAuth(t, handler, "Bearer viewer", http.MethodPost, "/api/v1/agents/"+agent.ID+"/chat", map[string]any{
		"message": "read grant cannot chat",
	}, http.StatusForbidden, &forbidden)

	doJSONAuth(t, handler, "Bearer viewer", http.MethodPatch, "/api/v1/squads/"+squad.ID, map[string]any{
		"mission": "take over",
	}, http.StatusForbidden, &forbidden)
	require.Equal(t, "forbidden", forbidden["error"]["code"])

	var audit []domain.AuditEntry
	doJSONAuth(t, handler, "Bearer owner", http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "access.denied")
}

func TestAccessGrantCreateFailsClosedWhenAuditFails(t *testing.T) {
	t.Parallel()

	base := storage.NewMemoryStore()
	store := failingAuditStore{MemoryStore: base}
	handler := New(testConfig(), store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Audit Required Squad",
	}, http.StatusCreated, &squad)
	viewer, err := base.UpsertUser(context.Background(), &domain.User{
		Email: "viewer@example.com",
		Name:  "Viewer",
		Role:  domain.RoleUser,
	})
	require.NoError(t, err)

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/access-grants", map[string]any{
		"grantee_type": "user",
		"grantee_id":   viewer.ID,
		"permissions":  "read",
	}, http.StatusInternalServerError, &body)
	require.Equal(t, "internal", body["error"]["code"])

	grants, err := base.ListGrants(context.Background(), squad.ID)
	require.NoError(t, err)
	require.Empty(t, grants)
}

func TestRegistryRequiresPlatformAdminForWrites(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), headerOIDC{
		"Bearer user": {Email: "user@example.com", Name: "User"},
	})

	var body map[string]map[string]string
	doJSONAuth(t, handler, "Bearer user", http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":     "OpenAI",
		"kind":     "openai",
		"base_url": "https://api.openai.com/v1",
	}, http.StatusForbidden, &body)
	require.Equal(t, "forbidden", body["error"]["code"])
}

func TestRegistryLLMProviderAndGenericResourceFlow(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var provider domain.LLMProvider
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":          "Local Llama",
		"kind":          "openai-compatible",
		"base_url":      "http://localhost:8123/v1",
		"api_key_ref":   "secret/local-llama",
		"default_model": "ollama/llama3.2",
		"models":        []string{"ollama/llama3.2"},
	}, http.StatusCreated, &provider)
	require.NotEmpty(t, provider.ID)
	require.Equal(t, domain.ResourceActive, provider.Status)
	require.Equal(t, "ollama/llama3.2", provider.DefaultModel)

	var providers []domain.LLMProvider
	doJSON(t, handler, http.MethodGet, "/api/v1/registry/llm-providers", nil, http.StatusOK, &providers)
	require.Len(t, providers, 1)
	require.Equal(t, "ollama/llama3.2", providers[0].DefaultModel)

	var updatedProvider domain.LLMProvider
	doJSON(t, handler, http.MethodPatch, "/api/v1/registry/llm-providers/"+provider.ID, map[string]any{
		"default_model": "ollama/qwen2.5-coder",
		"models":        []string{"ollama/llama3.2", "ollama/qwen2.5-coder"},
	}, http.StatusOK, &updatedProvider)
	require.Equal(t, "ollama/qwen2.5-coder", updatedProvider.DefaultModel)

	var skill domain.RegistryResource
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/skills", map[string]any{
		"name":        "repo-reader",
		"description": "Read project files",
		"manifest": map[string]any{
			"version": "1",
		},
	}, http.StatusCreated, &skill)
	require.NotEmpty(t, skill.ID)
	require.Equal(t, domain.ResSkill, skill.Type)

	var updated domain.RegistryResource
	doJSON(t, handler, http.MethodPatch, "/api/v1/registry/skills/"+skill.ID, map[string]any{
		"description": "Read repository files",
	}, http.StatusOK, &updated)
	require.Equal(t, "Read repository files", updated.Description)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/registry/skills/"+skill.ID+"/deprecate", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	var deprecated domain.RegistryResource
	doJSON(t, handler, http.MethodGet, "/api/v1/registry/skills/"+skill.ID, nil, http.StatusOK, &deprecated)
	require.Equal(t, domain.ResourceDeprecated, deprecated.Status)
}

func TestAuditAndMeteringEndpoints(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(testConfig(), store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Measured Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Metered Agent",
	}, http.StatusCreated, &agent)

	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID:      agent.ID,
		SquadID:      squad.ID,
		ProviderID:   "",
		Model:        "local",
		InputTokens:  120,
		OutputTokens: 35,
		Cost:         0.42,
		Currency:     "USD",
	}))

	var squadUsage domain.MeteringEvent
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/metering", nil, http.StatusOK, &squadUsage)
	require.Equal(t, 120, squadUsage.InputTokens)
	require.Equal(t, 35, squadUsage.OutputTokens)
	require.InDelta(t, 0.42, squadUsage.Cost, 0.0001)

	var agentUsage domain.MeteringEvent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/metering", nil, http.StatusOK, &agentUsage)
	require.Equal(t, 120, agentUsage.InputTokens)
	require.Equal(t, 35, agentUsage.OutputTokens)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.NotEmpty(t, audit)
	require.Contains(t, auditActions(audit), "agent.create")
	require.Contains(t, auditActions(audit), "squad.create")

	var summary domain.MeteringEvent
	doJSON(t, handler, http.MethodGet, "/api/v1/metering/summary", nil, http.StatusOK, &summary)
	require.Equal(t, 155, summary.InputTokens+summary.OutputTokens)
}

func TestGatewayMeteringCallbackRecordsUsageAndAudit(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	cfg := testConfig()
	cfg.GatewayCallbackToken = "callback-token"
	handler := New(cfg, store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Callback Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Callback Agent",
	}, http.StatusCreated, &agent)

	doGatewayCallback(t, handler, "callback-token", map[string]any{
		"agent_id":      agent.ID,
		"squad_id":      squad.ID,
		"model":         "openai/test-model",
		"input_tokens":  10,
		"output_tokens": 5,
		"cost":          0.12,
		"currency":      "USD",
	}, http.StatusAccepted)

	var usage domain.MeteringEvent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/metering", nil, http.StatusOK, &usage)
	require.Equal(t, 10, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.InDelta(t, 0.12, usage.Cost, 0.0001)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "llm.metering.ingest")
}

func TestGatewayMeteringCallbackRejectsBadToken(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.GatewayCallbackToken = "callback-token"
	handler := New(cfg, storage.NewMemoryStore())

	doGatewayCallback(t, handler, "wrong-token", map[string]any{
		"agent_id": "agent-1",
		"squad_id": "squad-1",
	}, http.StatusUnauthorized)
}

func TestGatewayFailureCallbackRecordsAuditOnly(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	cfg := testConfig()
	cfg.GatewayCallbackToken = "callback-token"
	handler := New(cfg, store)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Failure Callback Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Failure Callback Agent",
	}, http.StatusCreated, &agent)

	doGatewayCallback(t, handler, "callback-token", map[string]any{
		"status":   "failure",
		"agent_id": agent.ID,
		"squad_id": squad.ID,
		"model":    "openai/test-model",
		"error":    "upstream timeout",
	}, http.StatusAccepted)

	var usage domain.MeteringEvent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/metering", nil, http.StatusOK, &usage)
	require.Equal(t, 0, usage.InputTokens+usage.OutputTokens)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "llm.failure")
}

func TestSquadAndAgentMutationsWriteCustomResources(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, &fakeCRWriter{})

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Runtime Squad",
	}, http.StatusCreated, &squad)

	var updatedSquad domain.Squad
	doJSON(t, handler, http.MethodPatch, "/api/v1/squads/"+squad.ID, map[string]any{
		"mission": "run agents",
	}, http.StatusOK, &updatedSquad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Runner",
	}, http.StatusCreated, &agent)

	var updatedAgent domain.Agent
	doJSON(t, handler, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
		"role": "worker",
	}, http.StatusOK, &updatedAgent)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/squads/"+squad.ID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	events, err := store.ListKubernetesOutbox(context.Background(), "", 20)
	require.NoError(t, err)
	require.Equal(t, []string{
		domain.KubernetesOpUpsertSquad + ":" + squad.ID,
		domain.KubernetesOpUpsertSquad + ":" + squad.ID,
		domain.KubernetesOpUpsertAgent + ":" + agent.ID,
		domain.KubernetesOpUpsertAgent + ":" + agent.ID,
		domain.KubernetesOpDeleteAgent + ":" + agent.ID,
		domain.KubernetesOpDeleteSquad + ":" + squad.ID,
	}, outboxOperations(events))
}

func TestAgentIdentityCreateAndRotate(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Identity Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Identity Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	require.NotEmpty(t, identity.ID)
	require.Equal(t, agent.ID, identity.AgentID)
	require.Contains(t, identity.CredentialRef, "k8s://"+squad.Namespace+"/agent-"+agent.ID+"-credential-")
	require.Contains(t, identity.VirtualKeyRef, "k8s://"+squad.Namespace+"/agent-"+agent.ID+"-virtual-key-")
	require.NotEmpty(t, crWriter.credentialTokens[identity.CredentialRef])
	require.NotEmpty(t, crWriter.credentialTokens[identity.VirtualKeyRef])

	var conflict map[string]map[string]string
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusConflict, &conflict)
	require.Equal(t, "conflict", conflict["error"]["code"])

	var rotated domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity/rotate", nil, http.StatusOK, &rotated)
	require.Equal(t, identity.ID, rotated.ID)
	require.NotEqual(t, identity.CredentialRef, rotated.CredentialRef)
	require.False(t, rotated.RotatedAt.IsZero())
	require.NotEqual(t, identity.VirtualKeyRef, rotated.VirtualKeyRef)
	require.NotEmpty(t, crWriter.credentialTokens[rotated.CredentialRef])
	require.NotEmpty(t, crWriter.credentialTokens[rotated.VirtualKeyRef])

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "agent_identity.create")
	require.Contains(t, auditActions(audit), "agent_identity.rotate")
	require.Empty(t, crWriter.ops)
	require.Contains(t, crWriter.deletedCredentialRefs, identity.CredentialRef)
	require.Contains(t, crWriter.deletedCredentialRefs, identity.VirtualKeyRef)
}

func TestAgentIdentityProvisionsLiteLLMVirtualKey(t *testing.T) {
	t.Parallel()

	var keyRequests []map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/key/generate", r.URL.Path)
		require.Equal(t, "Bearer sk-test-master", r.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		keyRequests = append(keyRequests, body)
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "sk-agent-virtual-key"})
	}))
	defer gateway.Close()

	cfg := testConfig()
	cfg.LiteLLMAdminURL = gateway.URL
	cfg.LiteLLMMasterKey = "sk-test-master"
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(cfg, storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Gateway Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Gateway Agent",
	}, http.StatusCreated, &agent)

	var provider domain.LLMProvider
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":          "Local Llama",
		"kind":          "openai",
		"base_url":      "http://llama.local/v1",
		"api_key_ref":   "secret/local-llama",
		"default_model": "openai/local-default",
		"models":        []string{"openai/local-default", "openai/local-fast"},
	}, http.StatusCreated, &provider)

	var perms []domain.AgentPermission
	doJSON(t, handler, http.MethodPut, "/api/v1/agents/"+agent.ID+"/permissions", []map[string]string{
		{"resource_type": string(domain.ResLLMProvider), "resource_id": provider.ID},
	}, http.StatusOK, &perms)
	require.Len(t, perms, 1)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	require.NotEmpty(t, identity.ID)
	require.Equal(t, "sk-agent-virtual-key", crWriter.credentialTokens[identity.VirtualKeyRef])
	require.Len(t, keyRequests, 1)
	require.ElementsMatch(t, []any{"openai/local-default", "openai/local-fast"}, keyRequests[0]["models"])
	metadata := keyRequests[0]["metadata"].(map[string]any)
	require.Equal(t, agent.ID, metadata["skquad_agent_id"])
	require.Equal(t, squad.ID, metadata["skquad_squad_id"])
}

func TestAgentPermissionsSetAndList(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Permission Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Permissioned Agent",
	}, http.StatusCreated, &agent)

	var provider domain.LLMProvider
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":     "Permission Provider",
		"kind":     "openai-compatible",
		"base_url": "http://localhost:8123/v1",
	}, http.StatusCreated, &provider)

	var skill domain.RegistryResource
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/skills", map[string]any{
		"name": "permission-skill",
	}, http.StatusCreated, &skill)

	var perms []domain.AgentPermission
	doJSON(t, handler, http.MethodPut, "/api/v1/agents/"+agent.ID+"/permissions", []map[string]string{
		{"resource_type": string(domain.ResLLMProvider), "resource_id": provider.ID},
		{"resource_type": string(domain.ResSkill), "resource_id": skill.ID},
		{"resource_type": string(domain.ResSkill), "resource_id": skill.ID},
	}, http.StatusOK, &perms)
	require.Len(t, perms, 2)

	var listed []domain.AgentPermission
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/permissions", nil, http.StatusOK, &listed)
	require.Equal(t, perms, listed)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "agent_permissions.set")

	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPut, "/api/v1/agents/"+agent.ID+"/permissions", []map[string]string{
		{"resource_type": "not-real", "resource_id": provider.ID},
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])

	doJSON(t, handler, http.MethodPut, "/api/v1/agents/"+agent.ID+"/permissions", []map[string]string{
		{"resource_type": string(domain.ResTool), "resource_id": provider.ID},
	}, http.StatusNotFound, &body)
	require.Equal(t, "not_found", body["error"]["code"])
}

func TestAgentRuntimeResourcesReturnsGrantedActiveResources(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Runtime Resource Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Runtime Resource Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	var provider domain.LLMProvider
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":          "Gateway Model",
		"kind":          "openai-compatible",
		"base_url":      "http://llm-gateway/v1",
		"api_key_ref":   "secret/provider-key",
		"default_model": "gateway/model-a",
		"models":        []string{"gateway/model-a"},
	}, http.StatusCreated, &provider)

	var tool domain.RegistryResource
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/tools", map[string]any{
		"name":        "echo",
		"description": "Echo messages",
		"endpoint":    "plugin://echo",
		"auth_ref":    "secret/tool-key",
		"manifest": map[string]any{
			"package_ref": "builtin://echo",
		},
	}, http.StatusCreated, &tool)

	var deprecated domain.RegistryResource
	doJSON(t, handler, http.MethodPost, "/api/v1/registry/skills", map[string]any{
		"name": "old-skill",
	}, http.StatusCreated, &deprecated)
	doJSONNoBody(t, handler, http.MethodPost, "/api/v1/registry/skills/"+deprecated.ID+"/deprecate", nil, http.StatusNoContent)

	var perms []domain.AgentPermission
	doJSON(t, handler, http.MethodPut, "/api/v1/agents/"+agent.ID+"/permissions", []map[string]string{
		{"resource_type": string(domain.ResLLMProvider), "resource_id": provider.ID},
		{"resource_type": string(domain.ResTool), "resource_id": tool.ID},
		{"resource_type": string(domain.ResSkill), "resource_id": deprecated.ID},
	}, http.StatusOK, &perms)
	require.Len(t, perms, 3)

	var resources []map[string]any
	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, "/api/v1/agents/me/resources", nil, http.StatusOK, &resources)

	require.Len(t, resources, 2)
	require.Equal(t, string(domain.ResLLMProvider), resources[0]["resource_type"])
	require.Equal(t, provider.ID, resources[0]["resource_id"])
	require.Equal(t, "http://llm-gateway/v1", resources[0]["endpoint"])
	require.NotContains(t, resources[0], "api_key_ref")
	manifest := resources[0]["manifest"].(map[string]any)
	require.Equal(t, "openai-compatible", manifest["kind"])
	require.Equal(t, "gateway/model-a", manifest["default_model"])
	require.Equal(t, string(domain.ResTool), resources[1]["resource_type"])
	require.Equal(t, tool.ID, resources[1]["resource_id"])
	require.NotContains(t, resources[1], "auth_ref")
}

func TestAgentRuntimeTaskClaimAndStatusFlow(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Runtime Task Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Runtime Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Claim me",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var listed []domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, "/api/v1/agents/me/tasks", nil, http.StatusOK, &listed)
	require.Len(t, listed, 1)
	require.Equal(t, task.ID, listed[0].ID)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	require.Equal(t, task.ID, claimed.ID)
	require.Equal(t, domain.TaskInProgress, claimed.Status)
	require.NotEmpty(t, claimed.ExecutionID)
	require.NotEmpty(t, claimed.FencingToken)

	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusNoContent)

	var currentAgent domain.Agent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID, nil, http.StatusOK, &currentAgent)
	require.Equal(t, domain.AgentBusy, currentAgent.Status)

	var completed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        string(domain.TaskDone),
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &completed)
	require.Equal(t, domain.TaskDone, completed.Status)

	doAgentJSONNoBody(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusNoContent)

	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID, nil, http.StatusOK, &currentAgent)
	require.Equal(t, domain.AgentIdle, currentAgent.Status)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "task.claim")
	require.Contains(t, auditActions(audit), "task.complete")
}

func TestBoardExposesActiveExecutionState(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Board Execution Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Board Execution Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Work in flight",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	readBoard := func() domain.Task {
		var board struct {
			Board domain.Board  `json:"board"`
			Tasks []domain.Task `json:"tasks"`
		}
		doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/board", nil, http.StatusOK, &board)
		require.Len(t, board.Tasks, 1)
		return board.Tasks[0]
	}

	before := readBoard()
	require.Empty(t, before.ExecutionID, "an unclaimed task must not look in flight")
	require.True(t, before.LeaseExpiresAt.IsZero())

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	require.NotEmpty(t, claimed.ExecutionID)

	inFlight := readBoard()
	require.Equal(t, claimed.ExecutionID, inFlight.ExecutionID, "board must show the active execution")
	require.True(t, inFlight.LeaseExpiresAt.After(time.Now()), "board must carry the live lease deadline")
	require.Empty(t, inFlight.FencingToken, "fencing tokens must not leak to board readers")

	var completed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        string(domain.TaskDone),
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &completed)
	require.Equal(t, domain.TaskDone, completed.Status)

	after := readBoard()
	require.Empty(t, after.ExecutionID, "a finished execution must stop showing as in flight")
}

func TestBoardAfterReaperShowsTaskRequeued(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Reaper Board Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Reaper Board Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Dead worker task",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	require.NotEmpty(t, claimed.ExecutionID)

	// The worker dies: reap with a cutoff past the 2-minute claim lease.
	n, err := store.ReapExpiredTaskExecutions(context.Background(), time.Now().Add(3*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	var board struct {
		Board domain.Board  `json:"board"`
		Tasks []domain.Task `json:"tasks"`
	}
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/board", nil, http.StatusOK, &board)
	require.Len(t, board.Tasks, 1)
	require.Equal(t, domain.TaskTodo, board.Tasks[0].Status, "reaped task must be back in the todo queue")
	require.Empty(t, board.Tasks[0].ExecutionID, "reaped task must not look in flight")
	require.True(t, board.Tasks[0].LeaseExpiresAt.IsZero())
}

func TestRunExecutionReaperReapsLapsedLeaseAndStops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storage.NewMemoryStore()

	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "reaper-worker", OwnerID: "owner-1", Status: domain.SquadActive})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "reaper-worker-agent", Status: domain.AgentIdle})
	require.NoError(t, err)
	board, err := store.GetBoard(ctx, squad.ID)
	require.NoError(t, err)
	task, err := store.CreateTask(ctx, &domain.Task{
		BoardID: board.ID, SquadID: squad.ID, Title: "lapsed lease",
		Status: domain.TaskTodo, AssigneeAgentID: agent.ID,
		CreatedByType: "user", CreatedByID: "owner-1",
	})
	require.NoError(t, err)

	// Claim with a 1ms lease so it lapses immediately.
	claimed, err := store.ClaimNextTask(ctx, agent.ID, "worker-1", time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, task.ID, claimed.ID)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		RunExecutionReaper(runCtx, store, 10*time.Millisecond, 0)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := store.GetTask(ctx, task.ID)
		require.NoError(t, err)
		if got.Status == domain.TaskTodo {
			break
		}
		require.True(t, time.Now().Before(deadline), "task was not re-queued by the reaper in time")
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reaper did not stop on context cancel")
	}
}

func TestAgentRuntimeRejectsStaleTaskExecutionFence(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Fenced Task Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Fenced Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Reject stale completion",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)

	var conflict map[string]map[string]string
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        "done",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": "stale-token",
	}, http.StatusConflict, &conflict)
	require.Equal(t, "conflict", conflict["error"]["code"])

	var current domain.Task
	doJSON(t, handler, http.MethodGet, "/api/v1/tasks/"+task.ID, nil, http.StatusOK, &current)
	require.Equal(t, domain.TaskInProgress, current.Status)

	var completed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        "done",
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &completed)
	require.Equal(t, domain.TaskDone, completed.Status)
}

func TestAgentRuntimeTaskContextIncludesScopedMemory(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Context Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Context Agent",
	}, http.StatusCreated, &agent)
	var otherAgent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Other Agent",
	}, http.StatusCreated, &otherAgent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	var otherIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+otherAgent.ID+"/identity", nil, http.StatusCreated, &otherIdentity)
	otherCredential := crWriter.credentialTokens[otherIdentity.CredentialRef]

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Assemble context",
		"description":       "Use scoped memory",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)

	_, err := store.CreateAgentMemory(context.Background(), &domain.AgentMemory{
		AgentID:      agent.ID,
		SquadID:      squad.ID,
		Content:      "Same squad memory",
		SourceTaskID: task.ID,
		Metadata:     json.RawMessage(`{"kind":"task_completion"}`),
	})
	require.NoError(t, err)
	_, err = store.CreateAgentMemory(context.Background(), &domain.AgentMemory{
		AgentID:  agent.ID,
		Content:  "Private agent memory",
		Metadata: json.RawMessage(`{"kind":"note"}`),
	})
	require.NoError(t, err)
	_, err = store.CreateAgentMemory(context.Background(), &domain.AgentMemory{
		AgentID:  otherAgent.ID,
		SquadID:  squad.ID,
		Content:  "Wrong agent memory",
		Metadata: json.RawMessage(`{"kind":"note"}`),
	})
	require.NoError(t, err)

	var payload struct {
		Task   domain.Task          `json:"task"`
		Memory []domain.AgentMemory `json:"memory"`
		Limits map[string]int       `json:"limits"`
	}
	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, "/api/v1/agents/me/tasks/"+task.ID+"/context?memory_limit=1", nil, http.StatusOK, &payload)

	require.Equal(t, task.ID, payload.Task.ID)
	require.Len(t, payload.Memory, 1)
	require.NotEqual(t, otherAgent.ID, payload.Memory[0].AgentID)
	require.Equal(t, 1, payload.Limits["memory_limit"])

	var forbidden map[string]map[string]string
	doAgentJSON(t, handler, otherAgent.ID, otherCredential, http.MethodGet, "/api/v1/agents/me/tasks/"+task.ID+"/context", nil, http.StatusForbidden, &forbidden)
	require.Equal(t, "forbidden", forbidden["error"]["code"])
}

func TestAgentRuntimeCompletionCanPersistMemory(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Completion Memory Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Completion Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Persist result",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)

	var completed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":         "done",
		"summary":        "Durable completion summary",
		"persist_memory": true,
		"execution_id":   claimed.ExecutionID,
		"fencing_token":  claimed.FencingToken,
	}, http.StatusOK, &completed)
	require.Equal(t, domain.TaskDone, completed.Status)

	memories, err := store.ListAgentMemory(context.Background(), agent.ID, squad.ID, nil, 10)
	require.NoError(t, err)
	require.Len(t, memories, 1)
	require.Equal(t, "Durable completion summary", memories[0].Content)
	require.Equal(t, "Durable completion summary", memories[0].RawContent)
	require.Equal(t, "raw_model_output", memories[0].TrustLevel)
	require.Equal(t, "task_completion", memories[0].Provenance)
	require.Equal(t, "pending_review", memories[0].ReviewStatus)
	require.Equal(t, task.ID, memories[0].SourceTaskID)
}

func TestAssignedTaskMirrorsAgentBusyAndCompletionMirrorsIdle(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Wake Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Wake Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Wake agent",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)
	currentAgent, err := store.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, domain.AgentBusy, currentAgent.Status)
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)

	var completed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+task.ID+"/complete", map[string]any{
		"status":        string(domain.TaskDone),
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &completed)
	require.Equal(t, domain.TaskDone, completed.Status)
	currentAgent, err = store.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, domain.AgentIdle, currentAgent.Status)
}

func TestAgentCompletionStaysBusyWhenMoreAssignedWorkExists(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "More Work Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "More Work Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	var first domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "First task",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &first)
	var second domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Second task",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &second)
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)

	var completed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+first.ID+"/complete", map[string]any{
		"status":        string(domain.TaskDone),
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &completed)

	var currentAgent domain.Agent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID, nil, http.StatusOK, &currentAgent)
	require.Equal(t, domain.AgentBusy, currentAgent.Status)
	require.NotEmpty(t, second.ID)
}

func TestAgentRuntimeAuthRejectsInvalidCredential(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Runtime Auth Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Runtime Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)

	var body map[string]map[string]string
	doAgentJSON(t, handler, agent.ID, "wrong", http.MethodGet, "/api/v1/agents/me/tasks", nil, http.StatusUnauthorized, &body)
	require.Equal(t, "unauthorized", body["error"]["code"])

	doAgentJSON(t, handler, agent.ID, identity.CredentialRef, http.MethodGet, "/api/v1/agents/me/tasks", nil, http.StatusUnauthorized, &body)
	require.Equal(t, "unauthorized", body["error"]["code"])

	var currentAgent domain.Agent
	doAgentJSON(t, handler, agent.ID, crWriter.credentialTokens[identity.CredentialRef], http.MethodPost, "/api/v1/agents/me/heartbeat", map[string]string{
		"status": string(domain.AgentError),
	}, http.StatusOK, &currentAgent)
	require.Equal(t, domain.AgentError, currentAgent.Status)
}

func TestAgentMessagingInboxFlow(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Messaging Squad",
	}, http.StatusCreated, &squad)

	var sender domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Sender",
	}, http.StatusCreated, &sender)
	var recipient domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Recipient",
	}, http.StatusCreated, &recipient)

	var senderIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+sender.ID+"/identity", nil, http.StatusCreated, &senderIdentity)
	senderCredential := crWriter.credentialTokens[senderIdentity.CredentialRef]
	var recipientIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+recipient.ID+"/identity", nil, http.StatusCreated, &recipientIdentity)
	recipientCredential := crWriter.credentialTokens[recipientIdentity.CredentialRef]

	var sent domain.Message
	doAgentJSON(t, handler, sender.ID, senderCredential, http.MethodPost, "/api/v1/agents/me/messages", map[string]any{
		"to_agent_id": recipient.ID,
		"type":        "consult",
		"message":     "please review",
	}, http.StatusCreated, &sent)
	require.Equal(t, "agent", sent.FromType)
	require.Equal(t, sender.ID, sent.FromID)
	require.Equal(t, recipient.ID, sent.ToAgentID)
	require.Equal(t, domain.MessageConsult, sent.Type)
	require.Equal(t, domain.MessagePending, sent.Status)
	require.JSONEq(t, `{"message":"please review"}`, string(sent.Payload))

	var recipientState domain.Agent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+recipient.ID, nil, http.StatusOK, &recipientState)
	require.Equal(t, domain.AgentBusy, recipientState.Status)

	var inbox []domain.Message
	doAgentJSON(t, handler, recipient.ID, recipientCredential, http.MethodGet, "/api/v1/agents/me/messages", nil, http.StatusOK, &inbox)
	require.Len(t, inbox, 1)
	require.Equal(t, sent.ID, inbox[0].ID)

	var acked domain.Message
	doAgentJSON(t, handler, recipient.ID, recipientCredential, http.MethodPost, "/api/v1/agents/me/messages/"+sent.ID+"/ack", nil, http.StatusOK, &acked)
	require.Equal(t, domain.MessageDelivered, acked.Status)
	require.False(t, acked.DeliveredAt.IsZero())

	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+recipient.ID, nil, http.StatusOK, &recipientState)
	require.Equal(t, domain.AgentIdle, recipientState.Status)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "message.send")
	require.Contains(t, auditActions(audit), "message.ack")
}

func TestAgentWorkWaitReportsAvailableWork(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Wait Squad",
	}, http.StatusCreated, &squad)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Waiting Agent",
	}, http.StatusCreated, &agent)

	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	var waitResponse agentWorkWaitResponse
	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, "/api/v1/agents/me/work/wait?timeout_seconds=0", nil, http.StatusOK, &waitResponse)
	require.False(t, waitResponse.WorkAvailable)

	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/board/tasks", map[string]any{
		"title":             "Wake runtime",
		"assignee_agent_id": agent.ID,
	}, http.StatusCreated, &task)
	require.Equal(t, domain.TaskTodo, task.Status)

	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, "/api/v1/agents/me/work/wait?timeout_seconds=0", nil, http.StatusOK, &waitResponse)
	require.True(t, waitResponse.WorkAvailable)
}

func TestAgentMessageFailuresRetryThenDeadLetter(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Message Retry Squad",
	}, http.StatusCreated, &squad)
	var sender domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Sender",
	}, http.StatusCreated, &sender)
	var recipient domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Recipient",
	}, http.StatusCreated, &recipient)

	var senderIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+sender.ID+"/identity", nil, http.StatusCreated, &senderIdentity)
	senderCredential := crWriter.credentialTokens[senderIdentity.CredentialRef]
	var recipientIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+recipient.ID+"/identity", nil, http.StatusCreated, &recipientIdentity)
	recipientCredential := crWriter.credentialTokens[recipientIdentity.CredentialRef]

	var sent domain.Message
	doAgentJSON(t, handler, sender.ID, senderCredential, http.MethodPost, "/api/v1/agents/me/messages", map[string]any{
		"to_agent_id":  recipient.ID,
		"type":         "handoff",
		"message":      "unsupported for now",
		"max_attempts": 2,
	}, http.StatusCreated, &sent)
	require.Equal(t, 2, sent.MaxAttempts)

	var failed domain.Message
	doAgentJSON(t, handler, recipient.ID, recipientCredential, http.MethodPost, "/api/v1/agents/me/messages/"+sent.ID+"/fail", map[string]any{
		"reason": "unsupported handoff",
	}, http.StatusOK, &failed)
	require.Equal(t, domain.MessagePending, failed.Status)
	require.Equal(t, 1, failed.Attempts)
	require.True(t, failed.NextRetryAt.After(time.Now().UTC()))

	var inbox []domain.Message
	doAgentJSON(t, handler, recipient.ID, recipientCredential, http.MethodGet, "/api/v1/agents/me/messages", nil, http.StatusOK, &inbox)
	require.Empty(t, inbox)

	var recipientState domain.Agent
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+recipient.ID, nil, http.StatusOK, &recipientState)
	require.Equal(t, domain.AgentBusy, recipientState.Status)

	var dead domain.Message
	doAgentJSON(t, handler, recipient.ID, recipientCredential, http.MethodPost, "/api/v1/agents/me/messages/"+sent.ID+"/fail", map[string]any{
		"reason": "unsupported handoff",
	}, http.StatusOK, &dead)
	require.Equal(t, domain.MessageDead, dead.Status)
	require.Equal(t, 2, dead.Attempts)
	require.Contains(t, dead.TerminalReason, "unsupported handoff")

	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+recipient.ID, nil, http.StatusOK, &recipientState)
	require.Equal(t, domain.AgentIdle, recipientState.Status)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "message.fail")
}

func TestAgentMessageHistoryIncludesNonPendingMessages(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Message History Squad",
	}, http.StatusCreated, &squad)
	var sender domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Sender",
	}, http.StatusCreated, &sender)
	var recipient domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Recipient",
	}, http.StatusCreated, &recipient)

	var senderIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+sender.ID+"/identity", nil, http.StatusCreated, &senderIdentity)
	senderCredential := crWriter.credentialTokens[senderIdentity.CredentialRef]
	var recipientIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+recipient.ID+"/identity", nil, http.StatusCreated, &recipientIdentity)
	recipientCredential := crWriter.credentialTokens[recipientIdentity.CredentialRef]

	var sent domain.Message
	doAgentJSON(t, handler, sender.ID, senderCredential, http.MethodPost, "/api/v1/agents/me/messages", map[string]any{
		"to_agent_id": recipient.ID,
		"type":        "consult",
		"message":     "hello recipient",
	}, http.StatusCreated, &sent)

	// Ack the message so it is no longer pending. The history endpoint must
	// still return it (it is the full chat history, not just the pending queue).
	var acked domain.Message
	doAgentJSON(t, handler, recipient.ID, recipientCredential, http.MethodPost, "/api/v1/agents/me/messages/"+sent.ID+"/ack", nil, http.StatusOK, &acked)
	require.Equal(t, domain.MessageDelivered, acked.Status)

	var pending []domain.Message
	doAgentJSON(t, handler, recipient.ID, recipientCredential, http.MethodGet, "/api/v1/agents/me/messages", nil, http.StatusOK, &pending)
	require.Empty(t, pending)

	var history []domain.Message
	doAgentJSON(t, handler, recipient.ID, recipientCredential, http.MethodGet, "/api/v1/agents/me/messages/history", nil, http.StatusOK, &history)
	require.Len(t, history, 1)
	require.Equal(t, sent.ID, history[0].ID)
	require.Equal(t, domain.MessageDelivered, history[0].Status)
}

func TestExpiredAgentMessagesDoNotKeepAgentBusy(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Message Expiry Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Agent",
	}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]

	var sent domain.Message
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/chat", map[string]any{
		"message":     "short lived",
		"ttl_seconds": 1,
	}, http.StatusCreated, &sent)
	require.Equal(t, domain.MessagePending, sent.Status)

	time.Sleep(1100 * time.Millisecond)

	var inbox []domain.Message
	doAgentJSON(t, handler, agent.ID, credential, http.MethodGet, "/api/v1/agents/me/messages", nil, http.StatusOK, &inbox)
	require.Empty(t, inbox)

	var updated domain.Agent
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/heartbeat", map[string]any{
		"status": "idle",
	}, http.StatusOK, &updated)
	require.Equal(t, domain.AgentIdle, updated.Status)

	var history []domain.Message
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/chat", nil, http.StatusOK, &history)
	require.Len(t, history, 1)
	require.Equal(t, domain.MessageExpired, history[0].Status)
	require.NotEmpty(t, history[0].TerminalReason)
}

func TestAgentCrossSquadMessageRequiresGrant(t *testing.T) {
	t.Parallel()

	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), storage.NewMemoryStore(), crWriter)

	var sourceSquad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Source Squad",
	}, http.StatusCreated, &sourceSquad)
	var targetSquad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Target Squad",
	}, http.StatusCreated, &targetSquad)

	var sender domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+sourceSquad.ID+"/agents", map[string]any{
		"name": "Sender",
	}, http.StatusCreated, &sender)
	var recipient domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+targetSquad.ID+"/agents", map[string]any{
		"name": "Recipient",
	}, http.StatusCreated, &recipient)

	var senderIdentity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+sender.ID+"/identity", nil, http.StatusCreated, &senderIdentity)
	senderCredential := crWriter.credentialTokens[senderIdentity.CredentialRef]

	var denied map[string]map[string]string
	doAgentJSON(t, handler, sender.ID, senderCredential, http.MethodPost, "/api/v1/agents/me/messages", map[string]any{
		"to_agent_id": recipient.ID,
		"type":        "ping",
	}, http.StatusForbidden, &denied)
	require.Equal(t, "forbidden", denied["error"]["code"])

	var grant domain.AccessGrant
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+targetSquad.ID+"/access-grants", map[string]any{
		"grantee_type": "agent",
		"grantee_id":   sender.ID,
		"permissions":  "talk",
	}, http.StatusCreated, &grant)

	var sent domain.Message
	doAgentJSON(t, handler, sender.ID, senderCredential, http.MethodPost, "/api/v1/agents/me/messages", map[string]any{
		"to_agent_id": recipient.ID,
		"type":        "ping",
	}, http.StatusCreated, &sent)
	require.Equal(t, targetSquad.ID, sent.SquadID)
	require.Equal(t, domain.MessagePing, sent.Type)

	doAgentJSON(t, handler, sender.ID, senderCredential, http.MethodPost, "/api/v1/agents/me/messages", map[string]any{
		"to_agent_id": recipient.ID,
		"type":        "delegate",
	}, http.StatusForbidden, &denied)

	var audit []domain.AuditEntry
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+targetSquad.ID+"/audit", nil, http.StatusOK, &audit)
	require.Contains(t, auditActions(audit), "message.denied")
}

func TestUserChatCreatesAgentMessage(t *testing.T) {
	t.Parallel()

	handler := New(testConfig(), storage.NewMemoryStore())

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": "Chat Squad",
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": "Chat Agent",
	}, http.StatusCreated, &agent)

	var sent domain.Message
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/chat", map[string]any{
		"message": "hello agent",
	}, http.StatusCreated, &sent)
	require.Equal(t, "user", sent.FromType)
	require.Equal(t, agent.ID, sent.ToAgentID)
	require.Equal(t, domain.MessagePending, sent.Status)

	var history []domain.Message
	doJSON(t, handler, http.MethodGet, "/api/v1/agents/"+agent.ID+"/chat", nil, http.StatusOK, &history)
	require.Len(t, history, 1)
	require.Equal(t, sent.ID, history[0].ID)
}

func testConfig() *config.Config {
	return &config.Config{
		Addr:               ":0",
		AuthMode:           config.AuthDev,
		DevEmail:           "dev@skquad.local",
		DevName:            "Dev Admin",
		DefaultIdleTimeout: 5 * time.Minute,
	}
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any, wantStatus int, out any) {
	t.Helper()
	doJSONAuth(t, handler, "", method, path, body, wantStatus, out)
}

func doJSONNoBody(t *testing.T, handler http.Handler, method, path string, body any, wantStatus int) {
	t.Helper()

	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
}

func doGatewayCallback(t *testing.T, handler http.Handler, token string, body any, wantStatus int) {
	t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gateway/metering", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
}

func doJSONAuth(t *testing.T, handler http.Handler, authorization, method, path string, body any, wantStatus int, out any) {
	t.Helper()

	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out))
}

func doAgentJSON(t *testing.T, handler http.Handler, agentID, token, method, path string, body any, wantStatus int, out any) {
	t.Helper()
	rec := doAgentRequest(t, handler, agentID, token, method, path, body)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out))
}

func doAgentJSONNoBody(t *testing.T, handler http.Handler, agentID, token, method, path string, body any, wantStatus int) {
	t.Helper()
	rec := doAgentRequest(t, handler, agentID, token, method, path, body)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
}

func doAgentRequest(t *testing.T, handler http.Handler, agentID, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Skquad-Agent-ID", agentID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

type fakeOIDC struct {
	profile *auth.Profile
	err     error
}

func (f fakeOIDC) Authenticate(context.Context, string) (*auth.Profile, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.profile == nil {
		return nil, errors.New("missing fake profile")
	}
	return f.profile, nil
}

type headerOIDC map[string]*auth.Profile

func (h headerOIDC) Authenticate(_ context.Context, authorization string) (*auth.Profile, error) {
	profile, ok := h[authorization]
	if !ok {
		return nil, auth.ErrUnauthorized
	}
	return profile, nil
}

type failingAuditStore struct {
	*storage.MemoryStore
}

func (failingAuditStore) RecordAudit(context.Context, *domain.AuditEntry) error {
	return errors.New("audit unavailable")
}

func auditActions(entries []domain.AuditEntry) []string {
	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	return actions
}

func outboxOperations(events []*domain.KubernetesOutboxEvent) []string {
	operations := make([]string, 0, len(events))
	for _, event := range events {
		operations = append(operations, event.Operation+":"+event.AggregateID)
	}
	return operations
}

type fakeCRWriter struct {
	ops                   []string
	agentStatuses         []domain.AgentStatus
	credentialTokens      map[string]string
	deletedCredentialRefs []string
}

func (f *fakeCRWriter) UpsertSquad(_ context.Context, squad *domain.Squad) error {
	f.ops = append(f.ops, "upsert-squad:"+squad.ID)
	return nil
}

func (f *fakeCRWriter) DeleteSquad(_ context.Context, squad *domain.Squad) error {
	f.ops = append(f.ops, "delete-squad:"+squad.ID)
	return nil
}

func (f *fakeCRWriter) UpsertAgent(_ context.Context, agent *domain.Agent, _ *domain.AgentIdentity) error {
	f.ops = append(f.ops, "upsert-agent:"+agent.ID)
	f.agentStatuses = append(f.agentStatuses, agent.Status)
	return nil
}

func (f *fakeCRWriter) DeleteAgent(_ context.Context, agent *domain.Agent) error {
	f.ops = append(f.ops, "delete-agent:"+agent.ID)
	return nil
}

func (f *fakeCRWriter) WriteAgentCredential(_ context.Context, credentialRef string, _ string, token string) error {
	if f.credentialTokens == nil {
		f.credentialTokens = map[string]string{}
	}
	f.credentialTokens[credentialRef] = token
	return nil
}

func (f *fakeCRWriter) DeleteAgentCredential(_ context.Context, credentialRef string) error {
	f.deletedCredentialRefs = append(f.deletedCredentialRefs, credentialRef)
	if f.credentialTokens != nil {
		delete(f.credentialTokens, credentialRef)
	}
	return nil
}
