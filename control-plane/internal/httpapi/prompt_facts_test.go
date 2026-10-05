package httpapi

// Platform-prompt awareness tests: the composed platform block must carry
// the enabled-tool inventory and the bound-model facts, and must never
// fail over missing/unresolvable model data.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func factTestSetup(t *testing.T) (*Server, *storage.MemoryStore, *domain.Agent) {
	t.Helper()
	store := storage.NewMemoryStore()
	s := &Server{cfg: testConfig(), store: store}
	ctx := context.Background()
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "facts-squad", OwnerID: "owner-1", Status: domain.SquadActive})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: "facts-agent", Role: "worker", Status: domain.AgentIdle})
	require.NoError(t, err)
	return s, store, agent
}

func composedPlatformTier(t *testing.T, s *Server, agent *domain.Agent) string {
	t.Helper()
	comp, err := s.composePromptForAgent(context.Background(), agent)
	require.NoError(t, err)
	for _, tier := range comp.Tiers {
		if tier.Name == "platform" {
			return tier.Content
		}
	}
	t.Fatal("platform tier missing from composition")
	return ""
}

func TestComposePromptToolsInventory(t *testing.T) {
	s, store, agent := factTestSetup(t)
	ctx := context.Background()
	// Default seed: only send_message enabled. Enable exec too; leave
	// web_fetch/web_search disabled.
	yes := true
	_, err := store.UpdateBuiltinTool(ctx, domain.BuiltinToolExec, &yes, nil, "admin")
	require.NoError(t, err)

	platform := composedPlatformTier(t, s, agent)
	require.Contains(t, platform, "- exec — run shell commands inside your sandboxed agent pod; the container is your boundary")
	require.Contains(t, platform, "- send_message — send a message to a squad-mate agent (cross-squad needs an access grant; humans are NOT reachable via send_message)")
	// notify_owner ships enabled by default (like send_message), and so
	// does send_inbox (S-193 seed parity) — both described distinctly in
	// the inventory.
	require.Contains(t, platform, "- notify_owner — drop an action_required message directly into your squad owner's inbox")
	require.Contains(t, platform, "- send_inbox — deliver content a HUMAN asked you to send to your squad owner's inbox")
	require.NotContains(t, platform, "- web_fetch")
	require.NotContains(t, platform, "- web_search")
	require.NotContains(t, platform, "(none — you have no tools this run)")
}

func TestComposePromptNoToolsEnabled(t *testing.T) {
	s, store, agent := factTestSetup(t)
	ctx := context.Background()
	no := false
	for _, name := range domain.BuiltinToolNames {
		_, err := store.UpdateBuiltinTool(ctx, name, &no, nil, "admin")
		require.NoError(t, err)
	}
	platform := composedPlatformTier(t, s, agent)
	require.Contains(t, platform, "(none — you have no tools this run)")
	require.Contains(t, platform, "YOUR TOOLS")
}

func TestRenderEnabledToolsUnknownName(t *testing.T) {
	lines := renderEnabledTools([]*domain.BuiltinToolConfig{
		{Name: "mystery_tool", Enabled: true},
		{Name: "exec", Enabled: false},
	})
	require.Equal(t, []string{"- mystery_tool"}, lines)
}

func seedModel(t *testing.T, store *storage.MemoryStore, providerName, display, model string, ctxWin int, supportsTools bool) *domain.AIModel {
	t.Helper()
	ctx := context.Background()
	prov, err := store.CreateAIProvider(ctx, &domain.AIProvider{Name: providerName, Kind: "openai", BaseURL: "https://example.invalid"})
	require.NoError(t, err)
	m, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID:    prov.ID,
		DisplayName:   display,
		ModelName:     model,
		ContextWindow: ctxWin,
		SupportsTools: supportsTools,
	})
	require.NoError(t, err)
	return m
}

func TestComposePromptModelResolved(t *testing.T) {
	s, store, agent := factTestSetup(t)
	ctx := context.Background()
	primary := seedModel(t, store, "Anthropic", "Claude Opus 4.6", "claude-opus-4-6", 200000, true)
	fallback := seedModel(t, store, "OpenAI", "GPT-5.2", "gpt-5.2", 400000, true)

	agent.AIModelID = primary.ID
	agent.FallbackAIModelID = fallback.ID
	_, err := store.UpdateAgent(ctx, agent)
	require.NoError(t, err)

	platform := composedPlatformTier(t, s, agent)
	require.Contains(t, platform, "You are running on: Claude Opus 4.6 (claude-opus-4-6, provider\nAnthropic). Context window: 200000 tokens.")
	require.Contains(t, platform, "Tool calling: supported. Fallback model: GPT-5.2.")
}

func TestComposePromptModelUnbound(t *testing.T) {
	s, _, agent := factTestSetup(t)
	platform := composedPlatformTier(t, s, agent) // no AIModelID
	require.Contains(t, platform, "You are running on: unknown (unknown, provider\nunknown). Context window: unknown.")
	require.Contains(t, platform, "Tool calling: unknown. Fallback model: unknown.")
}

func TestComposePromptModelDangling(t *testing.T) {
	s, _, agent := factTestSetup(t)
	agent.AIModelID = "00000000-dead-beef-0000-000000000000" // never registered
	platform := composedPlatformTier(t, s, agent)
	require.Contains(t, platform, "You are running on: unknown (unknown, provider\nunknown). Context window: unknown.")
}

func TestComposePromptFallbackMissing(t *testing.T) {
	s, store, agent := factTestSetup(t)
	ctx := context.Background()
	primary := seedModel(t, store, "Anthropic", "Claude Opus 4.6", "claude-opus-4-6", 200000, false)
	agent.AIModelID = primary.ID
	agent.FallbackAIModelID = "11111111-dead-beef-0000-000000000000" // never registered
	_, err := store.UpdateAgent(ctx, agent)
	require.NoError(t, err)

	platform := composedPlatformTier(t, s, agent)
	require.Contains(t, platform, "Tool calling: NOT supported. Fallback model: unknown.")
}

func TestComposePromptNoFallbackConfigured(t *testing.T) {
	s, store, agent := factTestSetup(t)
	ctx := context.Background()
	model := seedModel(t, store, "Anthropic", "Claude Opus 4.6", "claude-opus-4-6", 0, true)
	agent.AIModelID = model.ID
	_, err := store.UpdateAgent(ctx, agent)
	require.NoError(t, err)

	platform := composedPlatformTier(t, s, agent)
	require.Contains(t, platform, "Context window: unknown.")
	require.Contains(t, platform, "Fallback model: none configured.")
}

// The composed response must still serialize cleanly with the enlarged
// platform block (guards against accidental template/cap regressions).
func TestComposePromptSerializes(t *testing.T) {
	s, store, agent := factTestSetup(t)
	seedModel(t, store, "Anthropic", "Claude Opus 4.6", "claude-opus-4-6", 200000, true)
	ctx := context.Background()
	models, err := store.ListAIModels(ctx, "")
	require.NoError(t, err)
	agent.AIModelID = models[0].ID
	_, err = store.UpdateAgent(ctx, agent)
	require.NoError(t, err)

	comp, err := s.composePromptForAgent(ctx, agent)
	require.NoError(t, err)
	require.NotEmpty(t, comp.SHA256)
	b, err := json.Marshal(compositionView(comp, true))
	require.NoError(t, err)
	require.Contains(t, string(b), "YOUR TOOLS")
	require.Contains(t, string(b), "WHEN A TOOL CALL FAILS")
}

// --- YOUR SQUAD / YOUR PLATFORM OWNER sections --------------------------

func TestComposePromptSquadSection(t *testing.T) {
	s, store, agent := factTestSetup(t)
	ctx := context.Background()
	// Add a second squad-mate so the roster has more than the agent itself.
	_, err := store.CreateAgent(ctx, &domain.Agent{SquadID: agent.SquadID, Name: "watson", Role: "investigator", Status: domain.AgentIdle})
	require.NoError(t, err)

	platform := composedPlatformTier(t, s, agent)
	require.Contains(t, platform, "YOUR SQUAD")
	require.Contains(t, platform, "You belong to squad facts-squad. Your squad-mates are:")
	require.Contains(t, platform, "facts-agent (worker); watson (investigator)")
	require.Contains(t, platform, "`send_message` reaches these squad-mates only.")
}

func TestComposePromptOwnerRendered(t *testing.T) {
	s, store, agent := factTestSetup(t)
	ctx := context.Background()
	_, err := store.UpsertUser(ctx, &domain.User{Email: "ross@acme.test", Name: "Ross Brigoli", Role: domain.RolePlatformAdmin})
	require.NoError(t, err)
	// Admin with no display name falls back to email.
	_, err = store.UpsertUser(ctx, &domain.User{Email: "admin2@acme.test", Role: domain.RolePlatformAdmin})
	require.NoError(t, err)
	// Plain user must never appear in the owner line.
	_, err = store.UpsertUser(ctx, &domain.User{Email: "pleb@acme.test", Name: "Just A User", Role: domain.RoleUser})
	require.NoError(t, err)

	platform := composedPlatformTier(t, s, agent)
	// Names are sorted for deterministic composition (ETag stability).
	require.Contains(t, platform, "The platform owner of this skquad instance is: Ross Brigoli, admin2@acme.test.")
	require.NotContains(t, platform, "Just A User")
}

func TestComposePromptOwnerUnknown(t *testing.T) {
	s, store, agent := factTestSetup(t)
	ctx := context.Background()
	_, err := store.UpsertUser(ctx, &domain.User{Email: "pleb@acme.test", Name: "Just A User", Role: domain.RoleUser})
	require.NoError(t, err)

	platform := composedPlatformTier(t, s, agent)
	require.Contains(t, platform, "The platform owner of this skquad instance is: unknown.")
}

func TestResolvePlatformOwnerEmptyStore(t *testing.T) {
	s, _, _ := factTestSetup(t)
	require.Equal(t, "unknown", s.resolvePlatformOwner(context.Background()))
}
