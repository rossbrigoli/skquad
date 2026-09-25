package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const (
	emailSuffix       = "@example.test"
	deleteModelErrFmt = "delete model: %v"
	createAgentErrFmt = "create agent: %v"
)

// WP1 (ADR-0010, S-106): ai_models + user_model_grants + agent binding.

func seedAIModelFixture(t *testing.T, store Store) (*domain.User, *domain.Squad, *domain.LLMProvider, *domain.AIModel) {
	t.Helper()
	ctx := context.Background()
	tag := uuid.NewString()[:8]

	user, err := store.UpsertUser(ctx, &domain.User{
		OIDCSubject: "subj-" + tag,
		Email:       "ai-" + tag + emailSuffix,
		Name:        "AI Test User",
		Role:        domain.RoleUser,
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	squad, err := store.CreateSquad(ctx, &domain.Squad{
		Name:      "squad-" + tag,
		OwnerID:   user.ID,
		Namespace: "ns-" + tag,
	})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	provider, err := store.CreateLLMProvider(ctx, &domain.LLMProvider{
		Name:         "prov-" + tag,
		Kind:         "openai",
		BaseURL:      "https://api.example.test",
		APIKeyRef:    "k8s:secret/prov-" + tag,
		RegisteredBy: user.ID,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	model, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID:                 provider.ID,
		DisplayName:                "Test Model " + tag,
		ModelName:                  "test-model-" + tag,
		ContextWindow:              128000,
		SupportsTools:              true,
		Pricing:                    json.RawMessage(`{"input_per_1m":1.25,"cached_input_per_1m":0.125,"cache_write_per_1m":1.5,"output_per_1m":10}`),
		LongContextThresholdTokens: 272000,
		RegisteredBy:               user.ID,
	})
	if err != nil {
		t.Fatalf("create ai model: %v", err)
	}
	return user, squad, provider, model
}

// --- Memory store tests (always run) ---

func TestMemoryAIModelRoundTrip(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	user, _, provider, model := seedAIModelFixture(t, store)

	got, err := store.GetAIModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("get ai model: %v", err)
	}
	if got.ProviderID != provider.ID || got.DisplayName != model.DisplayName || got.ModelName != model.ModelName {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.ContextWindow != 128000 || !got.SupportsTools || got.LongContextThresholdTokens != 272000 {
		t.Fatalf("capability fields wrong: %+v", got)
	}
	if got.Status != domain.ResourceActive {
		t.Fatalf("default status = %q, want active", got.Status)
	}
	if got.RegisteredBy != user.ID {
		t.Fatalf("registered_by = %q, want %q", got.RegisteredBy, user.ID)
	}
}

func TestMemoryAIModelUpdatePersists(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, _, _, model := seedAIModelFixture(t, store)

	got, err := store.GetAIModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("get ai model: %v", err)
	}
	got.DisplayName = "Renamed Model"
	got.ContextWindow = 200000
	updated, err := store.UpdateAIModel(ctx, got)
	if err != nil {
		t.Fatalf("update ai model: %v", err)
	}
	if updated.DisplayName != "Renamed Model" || updated.ContextWindow != 200000 {
		t.Fatalf("update not persisted: %+v", updated)
	}
	if updated.CreatedAt.IsZero() || !updated.UpdatedAt.After(updated.CreatedAt.Add(-time.Second)) {
		t.Fatalf("timestamps wrong: created=%v updated=%v", updated.CreatedAt, updated.UpdatedAt)
	}
}

func TestMemoryAIModelDeprecateAndDelete(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, _, _, model := seedAIModelFixture(t, store)

	if err := store.DeprecateAIModel(ctx, model.ID); err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	active, err := store.ListAIModels(ctx, domain.ResourceActive)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	for _, m := range active {
		if m.ID == model.ID {
			t.Fatal("deprecated model still listed as active")
		}
	}
	deprecated, err := store.ListAIModels(ctx, domain.ResourceDeprecated)
	if err != nil {
		t.Fatalf("list deprecated: %v", err)
	}
	if len(deprecated) != 1 || deprecated[0].ID != model.ID {
		t.Fatalf("deprecated list = %+v, want exactly the deprecated model", deprecated)
	}
	all, err := store.ListAIModels(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list all = %d, want 1", len(all))
	}

	if err := store.DeleteAIModel(ctx, model.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetAIModel(ctx, model.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
}

func TestMemoryAIModelUnknownProviderRejected(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, _, _, seeded := seedAIModelFixture(t, store)

	_, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID:  uuid.NewString(),
		DisplayName: "Orphan",
		ModelName:   "orphan-model",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("create with unknown provider err = %v, want ErrNotFound", err)
	}
	_ = seeded
}

func TestMemoryAIModelUniqueProviderModelName(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, _, provider, model := seedAIModelFixture(t, store)

	dup := &domain.AIModel{
		ProviderID:  provider.ID,
		DisplayName: "Duplicate display",
		ModelName:   model.ModelName,
	}
	if _, err := store.CreateAIModel(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate (provider, model_name) err = %v, want ErrConflict", err)
	}

	// Same model_name under a different provider must be allowed.
	other, err := store.CreateLLMProvider(ctx, &domain.LLMProvider{
		Name: "prov-other-" + model.ModelName, Kind: "anthropic",
		BaseURL: "https://other.example.test", APIKeyRef: "k8s:secret/other",
	})
	if err != nil {
		t.Fatalf("create other provider: %v", err)
	}
	if _, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID: other.ID, DisplayName: "Other display", ModelName: model.ModelName,
	}); err != nil {
		t.Fatalf("same model_name under different provider: %v", err)
	}
}

func TestMemoryProviderDeleteRestrictedByAIModels(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, _, provider, model := seedAIModelFixture(t, store)

	err := store.DeleteLLMProvider(ctx, provider.ID)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("provider delete with referencing models err = %v, want ErrConflict", err)
	}
	if _, err := store.GetLLMProvider(ctx, provider.ID); err != nil {
		t.Fatalf("provider vanished despite RESTRICT: %v", err)
	}

	if err := store.DeleteAIModel(ctx, model.ID); err != nil {
		t.Fatalf(deleteModelErrFmt, err)
	}
	if err := store.DeleteLLMProvider(ctx, provider.ID); err != nil {
		t.Fatalf("provider delete after models removed: %v", err)
	}
}

func TestMemoryAIModelDeleteRestrictedByAgentBinding(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, squad, _, model := seedAIModelFixture(t, store)

	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:   squad.ID,
		Name:      "bound-agent",
		AIModelID: model.ID,
	})
	if err != nil {
		t.Fatalf("create bound agent: %v", err)
	}
	if err := store.DeleteAIModel(ctx, model.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete bound model err = %v, want ErrConflict", err)
	}

	// Fallback-slot binding must restrict too.
	agent.AIModelID = ""
	agent.FallbackAIModelID = model.ID
	if _, err := store.UpdateAgent(ctx, agent); err != nil {
		t.Fatalf("move binding to fallback: %v", err)
	}
	if err := store.DeleteAIModel(ctx, model.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete fallback-bound model err = %v, want ErrConflict", err)
	}

	// Unbind, then delete succeeds.
	agent.FallbackAIModelID = ""
	if _, err := store.UpdateAgent(ctx, agent); err != nil {
		t.Fatalf("unbind agent: %v", err)
	}
	if err := store.DeleteAIModel(ctx, model.ID); err != nil {
		t.Fatalf("delete after unbind: %v", err)
	}
}

func TestMemoryUserModelGrantUniqueAndRevoke(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	user, _, _, model := seedAIModelFixture(t, store)

	grant, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
		GranteeUserID: user.ID, AIModelID: model.ID, GrantedBy: user.ID,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.ID == "" || grant.CreatedAt.IsZero() {
		t.Fatalf("grant not stamped: %+v", grant)
	}
	if _, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
		GranteeUserID: user.ID, AIModelID: model.ID, GrantedBy: user.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate grant err = %v, want ErrConflict", err)
	}

	grants, err := store.ListUserModelGrants(ctx, user.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 || grants[0].AIModelID != model.ID {
		t.Fatalf("grants = %+v, want one for the model", grants)
	}

	if err := store.RevokeModelFromUser(ctx, user.ID, model.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	grants, err = store.ListUserModelGrants(ctx, user.ID)
	if err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("grants after revoke = %+v, want empty", grants)
	}
	if err := store.RevokeModelFromUser(ctx, user.ID, model.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke err = %v, want ErrNotFound", err)
	}

	// Granting to an unknown user or unknown model must fail with ErrNotFound.
	if _, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
		GranteeUserID: uuid.NewString(), AIModelID: model.ID, GrantedBy: user.ID,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant unknown user err = %v, want ErrNotFound", err)
	}
	if _, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
		GranteeUserID: user.ID, AIModelID: uuid.NewString(), GrantedBy: user.ID,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant unknown model err = %v, want ErrNotFound", err)
	}
}

// grantTwoUsers seeds a second user and a second model, grants `model` to
// both users and the second model to `user`. Extracted from
// TestMemoryListUsersGrantedModel for cognitive complexity
// (S-126 / S3776).
func grantTwoUsers(t *testing.T, store *MemoryStore, ctx context.Context, user *domain.User, model *domain.AIModel) (*domain.User, *domain.AIModel) {
	t.Helper()
	other, err := store.UpsertUser(ctx, &domain.User{
		OIDCSubject: "other-" + model.ModelName, Email: "other-" + model.ModelName + emailSuffix,
	})
	if err != nil {
		t.Fatalf("upsert other user: %v", err)
	}
	otherModel, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID: model.ProviderID, DisplayName: "Second", ModelName: "second-" + model.ModelName,
	})
	if err != nil {
		t.Fatalf("create second model: %v", err)
	}
	for _, uid := range []string{user.ID, other.ID} {
		if _, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
			GranteeUserID: uid, AIModelID: model.ID, GrantedBy: user.ID,
		}); err != nil {
			t.Fatalf("grant %s: %v", uid, err)
		}
	}
	if _, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
		GranteeUserID: user.ID, AIModelID: otherModel.ID, GrantedBy: user.ID,
	}); err != nil {
		t.Fatalf("grant second model: %v", err)
	}
	return other, otherModel
}

func TestMemoryListUsersGrantedModel(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	user, _, _, model := seedAIModelFixture(t, store)
	other, _ := grantTwoUsers(t, store, ctx, user, model)

	holders, err := store.ListUsersGrantedModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("list holders: %v", err)
	}
	if len(holders) != 2 {
		t.Fatalf("holders = %+v, want 2", holders)
	}
	seen := map[string]bool{}
	for _, h := range holders {
		seen[h.GranteeUserID] = true
		if h.AIModelID != model.ID {
			t.Fatalf("wrong model in holders list: %+v", h)
		}
	}
	if !seen[user.ID] || !seen[other.ID] {
		t.Fatalf("holders missing expected users: %+v", holders)
	}

	// Deleting the model cascades its grants (FK ON DELETE CASCADE).
	if err := store.DeleteAIModel(ctx, model.ID); err != nil {
		t.Fatalf(deleteModelErrFmt, err)
	}
	holders, err = store.ListUsersGrantedModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("list holders after cascade: %v", err)
	}
	if len(holders) != 0 {
		t.Fatalf("grants not cascaded: %+v", holders)
	}
}

func TestMemoryAgentModelBindingRoundTrip(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, squad, _, model := seedAIModelFixture(t, store)

	fallback, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID: model.ProviderID, DisplayName: "Fallback", ModelName: "fallback-" + model.ModelName,
	})
	if err != nil {
		t.Fatalf("create fallback model: %v", err)
	}

	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:           squad.ID,
		Name:              "binding-agent",
		AIModelID:         model.ID,
		FallbackAIModelID: fallback.ID,
	})
	if err != nil {
		t.Fatalf(createAgentErrFmt, err)
	}
	if agent.AIModelID != model.ID || agent.FallbackAIModelID != fallback.ID {
		t.Fatalf("create did not echo binding: %+v", agent)
	}

	got, err := store.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.AIModelID != model.ID || got.FallbackAIModelID != fallback.ID {
		t.Fatalf("agent binding not persisted: %+v", got)
	}

	listed, err := store.ListAgents(ctx, squad.ID)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(listed) != 1 || listed[0].AIModelID != model.ID || listed[0].FallbackAIModelID != fallback.ID {
		t.Fatalf("list agents binding wrong: %+v", listed)
	}

	agent.AIModelID = fallback.ID
	agent.FallbackAIModelID = ""
	updated, err := store.UpdateAgent(ctx, agent)
	if err != nil {
		t.Fatalf("update agent: %v", err)
	}
	if updated.AIModelID != fallback.ID || updated.FallbackAIModelID != "" {
		t.Fatalf("update binding wrong: %+v", updated)
	}
}

func TestMemoryAgentEqualPrimaryFallbackRejected(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, squad, _, model := seedAIModelFixture(t, store)

	_, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:           squad.ID,
		Name:              "equal-agent",
		AIModelID:         model.ID,
		FallbackAIModelID: model.ID,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("create equal primary/fallback err = %v, want ErrConflict", err)
	}

	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:   squad.ID,
		Name:      "later-equal-agent",
		AIModelID: model.ID,
	})
	if err != nil {
		t.Fatalf(createAgentErrFmt, err)
	}
	agent.FallbackAIModelID = model.ID
	if _, err := store.UpdateAgent(ctx, agent); !errors.Is(err, ErrConflict) {
		t.Fatalf("update to equal primary/fallback err = %v, want ErrConflict", err)
	}
}

func TestMemoryAIModelReturnedValuesAreDeepCopies(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_, _, _, model := seedAIModelFixture(t, store)

	first, err := store.GetAIModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	first.Pricing[0] = 'X' // mutate the returned slice
	second, err := store.GetAIModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if second.Pricing[0] == 'X' {
		t.Fatal("memory store aliased Pricing across reads")
	}
}

// --- Postgres parity tests (skip without a test database) ---
// Do not add t.Parallel(): global sweeps (see postgres_parity_test.go).

func TestPostgresAIModelRoundTrip(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	user, _, provider, model := seedAIModelFixture(t, store)
	t.Cleanup(pgcleanup(t, store, user, provider, model))

	got, err := store.GetAIModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProviderID != provider.ID || got.ModelName != model.ModelName || got.ContextWindow != 128000 ||
		!got.SupportsTools || got.LongContextThresholdTokens != 272000 || got.Status != domain.ResourceActive {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if string(got.Pricing) != string(model.Pricing) {
		t.Fatalf("pricing round-trip: got %s want %s", got.Pricing, model.Pricing)
	}

	got.DisplayName = "PG Renamed"
	updated, err := store.UpdateAIModel(ctx, got)
	if err != nil || updated.DisplayName != "PG Renamed" || updated.UpdatedAt.IsZero() {
		t.Fatalf("update: %v %+v", err, updated)
	}

	if err := store.DeprecateAIModel(ctx, model.ID); err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	active, err := store.ListAIModels(ctx, domain.ResourceActive)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range active {
		if m.ID == model.ID {
			t.Fatal("deprecated model still active in postgres")
		}
	}

	if err := store.DeleteAIModel(ctx, model.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetAIModel(ctx, model.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
}

func TestPostgresAIModelUniqueViolation(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	user, _, provider, model := seedAIModelFixture(t, store)
	t.Cleanup(pgcleanup(t, store, user, provider, model))

	_, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID:  provider.ID,
		DisplayName: "Dup",
		ModelName:   model.ModelName,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("unique violation err = %v, want ErrConflict", err)
	}
}

func TestPostgresProviderDeleteRestrictedByAIModels(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	user, _, provider, model := seedAIModelFixture(t, store)
	t.Cleanup(pgcleanup(t, store, user, provider, model))

	if err := store.DeleteLLMProvider(ctx, provider.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("provider delete restricted err = %v, want ErrConflict", err)
	}
	if err := store.DeleteAIModel(ctx, model.ID); err != nil {
		t.Fatalf(deleteModelErrFmt, err)
	}
	if err := store.DeleteLLMProvider(ctx, provider.ID); err != nil {
		t.Fatalf("provider delete after model removed: %v", err)
	}
}

func TestPostgresUserModelGrants(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	user, _, _, model := seedAIModelFixture(t, store)
	other, err := store.UpsertUser(ctx, &domain.User{
		OIDCSubject: "pg-other-" + model.ModelName, Email: "pg-other-" + model.ModelName + emailSuffix,
	})
	if err != nil {
		t.Fatalf("upsert other: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = store.pool.Exec(ctx, `DELETE FROM user_model_grants WHERE grantee_user_id = ANY($1::uuid[]) OR granted_by = ANY($1::uuid[])`,
			[]string{user.ID, other.ID})
		_, _ = store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, other.ID)
		pgcleanup(t, store, user, nil, model)()
	})

	if _, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
		GranteeUserID: user.ID, AIModelID: model.ID, GrantedBy: user.ID,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
		GranteeUserID: user.ID, AIModelID: model.ID, GrantedBy: other.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate grant err = %v, want ErrConflict", err)
	}
	if _, err := store.GrantModelToUser(ctx, &domain.UserModelGrant{
		GranteeUserID: other.ID, AIModelID: model.ID, GrantedBy: user.ID,
	}); err != nil {
		t.Fatalf("grant other: %v", err)
	}

	holders, err := store.ListUsersGrantedModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("list holders: %v", err)
	}
	if len(holders) != 2 {
		t.Fatalf("holders = %+v, want 2", holders)
	}
	mine, err := store.ListUserModelGrants(ctx, user.ID)
	if err != nil {
		t.Fatalf("list mine: %v", err)
	}
	if len(mine) != 1 || mine[0].AIModelID != model.ID {
		t.Fatalf("mine = %+v", mine)
	}

	if err := store.RevokeModelFromUser(ctx, user.ID, model.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := store.RevokeModelFromUser(ctx, user.ID, model.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke err = %v, want ErrNotFound", err)
	}
	holders, err = store.ListUsersGrantedModel(ctx, model.ID)
	if err != nil {
		t.Fatalf("list holders after revoke: %v", err)
	}
	if len(holders) != 1 || holders[0].GranteeUserID != other.ID {
		t.Fatalf("holders after revoke = %+v, want only the other user", holders)
	}
}

func TestPostgresAgentModelBindingRoundTrip(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	user, squad, provider, model := seedAIModelFixture(t, store)
	fallback, err := store.CreateAIModel(ctx, &domain.AIModel{
		ProviderID: model.ProviderID, DisplayName: "PG Fallback", ModelName: "pg-fallback-" + model.ModelName,
	})
	if err != nil {
		t.Fatalf("create fallback: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(ctx, `DELETE FROM agents WHERE squad_id = $1`, squad.ID)
		pgcleanup(t, store, user, provider, model)()
		_ = store.DeleteAIModel(context.Background(), fallback.ID)
	})

	agent, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:           squad.ID,
		Name:              "pg-binding-agent",
		AIModelID:         model.ID,
		FallbackAIModelID: fallback.ID,
	})
	if err != nil {
		t.Fatalf(createAgentErrFmt, err)
	}
	got, err := store.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.AIModelID != model.ID || got.FallbackAIModelID != fallback.ID {
		t.Fatalf("postgres binding not persisted: %+v", got)
	}

	agent.FallbackAIModelID = ""
	updated, err := store.UpdateAgent(ctx, agent)
	if err != nil {
		t.Fatalf("update agent: %v", err)
	}
	if updated.FallbackAIModelID != "" || updated.AIModelID != model.ID {
		t.Fatalf("update binding wrong: %+v", updated)
	}

	// Delete restriction: model still bound via primary.
	if err := store.DeleteAIModel(ctx, model.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete bound model err = %v, want ErrConflict", err)
	}
}

func TestPostgresAgentEqualPrimaryFallbackRejected(t *testing.T) {
	store := postgresTestStore(t)
	ctx := context.Background()
	user, squad, provider, model := seedAIModelFixture(t, store)
	t.Cleanup(func() {
		_, _ = store.pool.Exec(ctx, `DELETE FROM agents WHERE squad_id = $1`, squad.ID)
		pgcleanup(t, store, user, provider, model)()
	})

	_, err := store.CreateAgent(ctx, &domain.Agent{
		SquadID:           squad.ID,
		Name:              "pg-equal-agent",
		AIModelID:         model.ID,
		FallbackAIModelID: model.ID,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("equal primary/fallback err = %v, want ErrConflict", err)
	}

	// The DB CHECK itself must reject the same pair even when bypassing the
	// store-layer guard.
	_, err = store.pool.Exec(ctx, `
		INSERT INTO agents (squad_id, name, ai_model_id, fallback_ai_model_id)
		VALUES ($1, 'raw-equal-agent', $2, $2)
	`, squad.ID, model.ID)
	if err == nil {
		t.Fatal("DB CHECK agents_fallback_nequals_primary did not fire")
	}
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23514" {
		t.Fatalf("raw insert err = %v, want check_violation 23514", err)
	}
}

// pgDelete runs one cleanup DELETE, logging (not failing) on error.
// Extracted from pgcleanup for cognitive complexity (S-126 / S3776).
func pgDelete(t *testing.T, store *PostgresStore, ctx context.Context, query string, id any, label string) {
	t.Helper()
	if _, err := store.pool.Exec(ctx, query, id); err != nil {
		t.Logf("cleanup %s %v: %v", label, id, err)
	}
}

// pgcleanup removes the fixture rows (grants, model, provider, squad, user).
func pgcleanup(t *testing.T, store *PostgresStore, user *domain.User, provider *domain.LLMProvider, model *domain.AIModel) func() {
	return func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if model != nil {
			pgDelete(t, store, ctx, `DELETE FROM user_model_grants WHERE ai_model_id = $1`, model.ID, "grants for model")
			pgDelete(t, store, ctx, `DELETE FROM ai_models WHERE id = $1`, model.ID, "model")
		}
		if provider != nil {
			pgDelete(t, store, ctx, `DELETE FROM providers WHERE id = $1`, provider.ID, "provider")
		}
		if user != nil {
			// Squads reference the owner; drop them before the user.
			pgDelete(t, store, ctx, `DELETE FROM squads WHERE owner_id = $1`, user.ID, "squads for")
			pgDelete(t, store, ctx, `DELETE FROM users WHERE id = $1`, user.ID, "user")
		}
	}
}
