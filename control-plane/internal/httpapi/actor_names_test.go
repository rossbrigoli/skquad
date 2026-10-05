package httpapi

// S-235: actor display-name resolution on the task thread and the status
// timeline. The wire gains additive `from_display` / `actor_display`
// fields resolved at READ time: agents show their current name (a rename
// after the event must show the NEW name), users show the first token of
// their profile name with the email local-part as fallback.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func TestActorDisplayName(t *testing.T) {
	t.Parallel()

	// Users: first token of the profile name.
	require.Equal(t, "Ross", actorDisplayName("user", "Ross Brigoli", "ross@example.com"))
	require.Equal(t, "Ada", actorDisplayName("user", "  Ada  Lovelace  ", "ada@example.com"))
	// Users without a profile name fall back to the email local-part.
	require.Equal(t, "ross", actorDisplayName("user", "", "ross@example.com"))
	require.Equal(t, "dev", actorDisplayName("user", "   ", "dev@skquad.local"))
	// Nothing resolvable → empty (caller keeps its own fallback).
	require.Equal(t, "", actorDisplayName("user", "", ""))
	// Agents: name verbatim; system actors never resolve.
	require.Equal(t, "Bob", actorDisplayName("agent", "Bob", ""))
	require.Equal(t, "", actorDisplayName("agent", "  ", ""))
	require.Equal(t, "", actorDisplayName("system", "whatever", "x@y.z"))
}

// s235Fixture: squad owned by OIDC user "Ross Brigoli" with one runtime
// agent "Bob" (identity + credential wired through the fake CR writer).
type s235Fixture struct {
	handler   http.Handler
	store     *storage.MemoryStore
	userID    string
	agentID   string
	agentCred string
	squadID   string
}

func newS235Fixture(t *testing.T) *s235Fixture {
	t.Helper()

	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	// Dev-mode auth provisions the configured dev principal; give the
	// human a real first name so display resolution is exercised.
	cfg := testConfig()
	cfg.DevName = "Ross Brigoli"
	cfg.DevEmail = "ross@example.com"
	handler := NewWithCRWriter(cfg, store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "S235 Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Bob"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agent.ID+pathIdentity, nil, http.StatusCreated, &identity)
	cred := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, cred)

	var me domain.User
	doJSON(t, handler, http.MethodGet, pathAuthMe, nil, http.StatusOK, &me)

	return &s235Fixture{
		handler: handler, store: store, userID: me.ID,
		agentID: agent.ID, agentCred: cred, squadID: squad.ID,
	}
}

func (f *s235Fixture) createTask(t *testing.T) domain.Task {
	t.Helper()
	var task domain.Task
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squadID+pathBoardTasks, map[string]any{
		"title": "Fix the thing", "assignee_agent_id": f.agentID,
	}, http.StatusCreated, &task)
	return task
}

func TestS235TaskThreadResolvesActorDisplays(t *testing.T) {
	f := newS235Fixture(t)
	task := f.createTask(t)

	// Human message on the thread.
	doJSON(t, f.handler, http.MethodPost, pathTasksPrefix+task.ID+pathMessages, map[string]any{
		"message": "human says hi",
	}, http.StatusCreated, &domain.Message{})

	// Agent reply via the agent-side endpoint (agent actor). The task page
	// thread filters on payload.task_id, so the payload carries it.
	payload, err := json.Marshal(map[string]string{"message": "agent says hi", "task_id": task.ID})
	require.NoError(t, err)
	doAgentJSON(t, f.handler, f.agentID, f.agentCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.agentID, "type": "reply", "payload": json.RawMessage(payload),
	}, http.StatusCreated, &domain.Message{})

	var thread []domain.Message
	doJSON(t, f.handler, http.MethodGet, pathTasksPrefix+task.ID+pathMessages, nil, http.StatusOK, &thread)
	// 3 rows: the task-creation origin message (user), the human reply,
	// and the agent reply — all scoped to this task.
	require.Len(t, thread, 3)

	displayByType := map[string]string{}
	for _, msg := range thread {
		displayByType[msg.FromType] = msg.FromDisplay
	}
	require.Equal(t, "Ross", displayByType["user"], "human message shows first name")
	require.Equal(t, "Bob", displayByType["agent"], "agent message shows agent name")
}

func TestS235AuditTimelineResolvesActorDisplays(t *testing.T) {
	f := newS235Fixture(t)
	task := f.createTask(t)

	// Human moves the task → user-actor audit entry.
	doJSON(t, f.handler, http.MethodPost, pathTasksPrefix+task.ID+"/move", map[string]any{
		"status": "in-progress",
	}, http.StatusOK, &domain.Task{})

	// Agent starts the task → agent-actor audit entry.
	doAgentJSONNoBody(t, f.handler, f.agentID, f.agentCred, http.MethodPost,
		pathMyTasksPrefix+task.ID+"/start", nil, http.StatusOK)

	var audit []domain.AuditEntry
	doJSON(t, f.handler, http.MethodGet, pathSquadsPrefix+f.squadID+pathAudit+"?limit=50", nil, http.StatusOK, &audit)

	byAction := map[string]domain.AuditEntry{}
	for _, e := range audit {
		byAction[e.Action] = e
	}
	move, ok := byAction["task.move"]
	require.True(t, ok, "task.move audited")
	require.Equal(t, "user", move.ActorType)
	require.Equal(t, "Ross", move.ActorDisplay, "human movement shows first name, not GUID")

	start, ok := byAction["task.start"]
	require.True(t, ok, "task.start audited")
	require.Equal(t, "agent", start.ActorType)
	require.Equal(t, "Bob", start.ActorDisplay, "agent movement says WHICH agent")
}

func TestS235AgentRenameShowsCurrentNameAtReadTime(t *testing.T) {
	f := newS235Fixture(t)
	task := f.createTask(t)

	doAgentJSONNoBody(t, f.handler, f.agentID, f.agentCred, http.MethodPost,
		pathMyTasksPrefix+task.ID+"/start", nil, http.StatusOK)

	// Rename the agent AFTER the event was recorded. The public API
	// freezes agent names (k8s deployment binding), so the rename goes
	// straight through the store — read-time resolution must still
	// surface the CURRENT name.
	agent, err := f.store.GetAgent(context.Background(), f.agentID)
	require.NoError(t, err)
	agent.Name = "Bobby"
	_, err = f.store.UpdateAgent(context.Background(), agent)
	require.NoError(t, err)

	var audit []domain.AuditEntry
	doJSON(t, f.handler, http.MethodGet, pathSquadsPrefix+f.squadID+pathAudit+"?limit=50", nil, http.StatusOK, &audit)
	for _, e := range audit {
		if e.Action == "task.start" && e.ActorType == "agent" {
			require.Equal(t, "Bobby", e.ActorDisplay, "read-time resolution shows the CURRENT name")
			return
		}
	}
	t.Fatal("task.start agent audit entry not found")
}

func TestS235UserWithoutFirstNameFallsBackToEmail(t *testing.T) {
	store := storage.NewMemoryStore()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner:        {Email: "ross@example.com", Name: "Ross Brigoli", EmailVerified: true},
		"Bearer nofirst": {Email: "nofirst@example.com", Name: "", EmailVerified: true},
	})
	var squad domain.Squad
	doJSONAuth(t, handler, "Bearer nofirst", http.MethodPost, pathSquads, map[string]any{"name": "Fallback Squad"}, http.StatusCreated, &squad)

	// The nameless user creates a task → user-actor audit entry with no
	// profile name; display must fall back to the email local-part.
	var task domain.Task
	doJSONAuth(t, handler, "Bearer nofirst", http.MethodPost, pathSquadsPrefix+squad.ID+pathBoardTasks, map[string]any{
		"title": "No first name here",
	}, http.StatusCreated, &task)

	var audit []domain.AuditEntry
	doJSONAuth(t, handler, "Bearer nofirst", http.MethodGet, pathSquadsPrefix+squad.ID+pathAudit+"?limit=50", nil, http.StatusOK, &audit)
	for _, e := range audit {
		if e.Action == "task.create" && e.ActorType == "user" {
			require.Equal(t, "nofirst", e.ActorDisplay, "empty profile name falls back to email local-part")
			return
		}
	}
	t.Fatal("task.create user audit entry not found")
}

func TestS235SystemActorsAndUnknownIdsStayEmpty(t *testing.T) {
	f := newS235Fixture(t)
	srv := &Server{store: f.store}
	unknown := "11111111-1111-1111-1111-111111111111"
	refs := []actorRef{
		{kind: "system", id: "00000000-0000-0000-0000-000000000000"},
		{kind: "agent", id: unknown},
		{kind: "user", id: ""},
	}
	resolved := srv.resolveActorDisplays(context.Background(), refs)
	require.Equal(t, "", actorDisplay(resolved, "system", "00000000-0000-0000-0000-000000000000"))
	require.Equal(t, "", actorDisplay(resolved, "agent", unknown))
	require.Equal(t, "", actorDisplay(resolved, "user", ""))
}

// TestS235WireBackwardCompatible asserts the additive fields do not
// disturb existing consumers: from_type/from_id and actor_type/actor_id
// still present and unchanged; actor_display omitted when empty.
func TestS235WireBackwardCompatible(t *testing.T) {
	f := newS235Fixture(t)
	task := f.createTask(t)
	doJSON(t, f.handler, http.MethodPost, pathTasksPrefix+task.ID+"/move", map[string]any{
		"status": "in-review",
	}, http.StatusOK, &domain.Task{})

	var raw []map[string]any
	doJSON(t, f.handler, http.MethodGet, pathSquadsPrefix+f.squadID+pathAudit+"?limit=50", nil, http.StatusOK, &raw)
	require.NotEmpty(t, raw)
	for _, entry := range raw {
		require.Contains(t, entry, "actor_type")
		require.Contains(t, entry, "actor_id")
		if entry["actor_type"] == "user" {
			require.Equal(t, "Ross", entry["actor_display"])
		}
	}
	// omitempty: unset display never reaches the wire.
	b, err := json.Marshal(domain.AuditEntry{ActorType: "system", ActorID: "x"})
	require.NoError(t, err)
	require.NotContains(t, string(b), "actor_display")
	b2, err := json.Marshal(domain.Message{FromType: "agent", FromID: "y"})
	require.NoError(t, err)
	require.NotContains(t, string(b2), "from_display")
}
