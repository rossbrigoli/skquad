// S-170 tests: creating an agent with a primary model auto-provisions
// the runtime identity in the same request (no manual "Provision
// identity" step), and any provisioning failure rolls the agent row back
// so no agent is ever born without the identity it needs.

package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func TestCreateAgentAutoProvisionsIdentity(t *testing.T) {
	f := newGatewayFixture(t)

	var agent domain.Agent
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squadID+"/agents", map[string]any{
		"name":        "born-bound",
		"ai_model_id": f.modelA.ID,
	}, http.StatusCreated, &agent)
	require.Equal(t, f.modelA.ID, agent.AIModelID)

	identity, err := f.store.GetAgentIdentity(context.Background(), agent.ID)
	require.NoError(t, err, "identity must exist right after creation")
	require.NotEmpty(t, identity.CredentialHash)
	require.NotEmpty(t, identity.GatewayKeyToken)
	require.Equal(t, domain.GatewayKeyActive, identity.GatewayKeyStatus)

	// The gateway saw a key-generation call for the new agent.
	f.gateway.mu.Lock()
	defer f.gateway.mu.Unlock()
	require.NotEmpty(t, f.gateway.generated, "auto-provision must call /key/generate")
	last := f.gateway.generated[len(f.gateway.generated)-1]
	require.Equal(t, "skquad-agent-"+agent.ID, last["key_alias"])
}

func TestCreateAgentUngrantedModelRejectedNoRow(t *testing.T) {
	f := newGatewayFixture(t)

	// Model validation happens BEFORE the row is created: no half-born agent.
	requireErrorCode(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squadID+"/agents", map[string]any{
		"name":        "no-grant-agent",
		"ai_model_id": f.modelNoGrant.ID,
	}, http.StatusForbidden, "model_not_granted")

	_, err := f.store.GetAgentByNameForOwner(context.Background(), f.ownerID, "no-grant-agent")
	require.ErrorIs(t, err, storage.ErrNotFound, "rejected create must leave no agent row")
}

func TestCreateAgentUnknownModelRejectedNoRow(t *testing.T) {
	f := newGatewayFixture(t)

	requireErrorCode(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squadID+"/agents", map[string]any{
		"name":        "ghost-model-agent",
		"ai_model_id": "00000000-0000-4000-8000-000000000000",
	}, http.StatusBadRequest, "model_not_found")

	_, err := f.store.GetAgentByNameForOwner(context.Background(), f.ownerID, "ghost-model-agent")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestCreateAgentProvisionFailureRollsBack(t *testing.T) {
	f := newGatewayFixture(t)

	f.gateway.mu.Lock()
	f.gateway.failGenerate = true
	f.gateway.mu.Unlock()

	// Gateway is down: provisioning the virtual key fails → 502 and the
	// agent row must be rolled back.
	requireErrorCode(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squadID+"/agents", map[string]any{
		"name":        "rollback-me",
		"ai_model_id": f.modelA.ID,
	}, http.StatusBadGateway, "llm_gateway_unavailable")

	_, err := f.store.GetAgentByNameForOwner(context.Background(), f.ownerID, "rollback-me")
	require.ErrorIs(t, err, storage.ErrNotFound, "provisioning failure must roll back the agent row")

	// The name is free again: retry after the gateway recovers succeeds.
	f.gateway.mu.Lock()
	f.gateway.failGenerate = false
	f.gateway.mu.Unlock()

	var agent domain.Agent
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squadID+"/agents", map[string]any{
		"name":        "rollback-me",
		"ai_model_id": f.modelA.ID,
	}, http.StatusCreated, &agent)
	_, err = f.store.GetAgentIdentity(context.Background(), agent.ID)
	require.NoError(t, err, "retry after gateway recovery must produce a fully provisioned agent")
}

func TestCreateAgentWithoutModelGetsNoIdentity(t *testing.T) {
	f := newGatewayFixture(t)

	// Omitting the model keeps the old behaviour: agent exists, identity
	// waits for a later bind + provision (legacy retry door).
	var agent domain.Agent
	doJSON(t, f.handler, http.MethodPost, pathSquadsPrefix+f.squadID+"/agents", map[string]any{
		"name": "unbound-at-birth",
	}, http.StatusCreated, &agent)
	require.Empty(t, agent.AIModelID)

	_, err := f.store.GetAgentIdentity(context.Background(), agent.ID)
	require.Error(t, err, "no identity without a model at creation")
}
