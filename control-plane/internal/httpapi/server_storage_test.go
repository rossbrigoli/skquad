package httpapi

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

// storageTestConfig mirrors testConfig() with the S-138 platform storage
// knobs set explicitly so tests do not depend on env defaults.
func storageTestConfig() *config.Config {
	cfg := testConfig()
	cfg.DefaultAgentStorageSize = "2Gi"
	cfg.MaxAgentStorage = "10Gi"
	return cfg
}

func createStorageTestSquad(t *testing.T, handler http.Handler) domain.Squad {
	t.Helper()
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{
		"name":    "Storage Squad",
		"mission": "durable workspaces",
	}, http.StatusCreated, &squad)
	require.NotEmpty(t, squad.ID)
	return squad
}

func TestCreateAgentStorageFields(t *testing.T) {
	t.Parallel()

	handler := New(storageTestConfig(), storage.NewMemoryStore())
	squad := createStorageTestSquad(t, handler)

	var enabled domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name":           "durable-1",
		"storage_enabled": true,
		"storage_size":    "5Gi",
	}, http.StatusCreated, &enabled)
	require.True(t, enabled.StorageEnabled)
	require.Equal(t, "5Gi", enabled.StorageSize)

	// Enabled without a size falls back to the platform default.
	var defaulted domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name":           "durable-2",
		"storage_enabled": true,
	}, http.StatusCreated, &defaulted)
	require.True(t, defaulted.StorageEnabled)
	require.Equal(t, "2Gi", defaulted.StorageSize)

	// No storage fields: storage stays off, exactly the legacy behaviour.
	var plain domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "plain-1",
	}, http.StatusCreated, &plain)
	require.False(t, plain.StorageEnabled)
	require.Empty(t, plain.StorageSize)
}

func TestCreateAgentStorageValidation(t *testing.T) {
	t.Parallel()

	handler := New(storageTestConfig(), storage.NewMemoryStore())
	squad := createStorageTestSquad(t, handler)

	invalid := []struct {
		name  string
		size  string
		field string
	}{
		{"malformed", "5GB", "storage_size"},
		{"lowercase suffix", "2gi", "storage_size"},
		{"over max", "11Gi", "storage_size"},
		{"zero", "0", "storage_size"},
		{"negative", "-1Gi", "storage_size"},
	}
	for _, tc := range invalid {
		var body map[string]map[string]string
		doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
			"name":           "bad-" + tc.name,
			"storage_enabled": true,
			"storage_size":    tc.size,
		}, http.StatusBadRequest, &body)
		require.Equal(t, "bad_request", body["error"]["code"], tc.name)
		require.Contains(t, body["error"]["message"], tc.field, tc.name)
	}
}

func TestUpdateAgentStorageFields(t *testing.T) {
	t.Parallel()

	handler := New(storageTestConfig(), storage.NewMemoryStore())
	squad := createStorageTestSquad(t, handler)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "mutable-1",
	}, http.StatusCreated, &agent)
	require.False(t, agent.StorageEnabled)

	var patched domain.Agent
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"storage_enabled": true,
		"storage_size":    "3Gi",
	}, http.StatusOK, &patched)
	require.True(t, patched.StorageEnabled)
	require.Equal(t, "3Gi", patched.StorageSize)

	// Enabling with no size present keeps the existing size; enabling on an
	// agent that never had one applies the platform default.
	var reEnabled domain.Agent
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"storage_enabled": true,
	}, http.StatusOK, &reEnabled)
	require.True(t, reEnabled.StorageEnabled)
	require.Equal(t, "3Gi", reEnabled.StorageSize)

	// Disabling keeps the size recorded (harmless, and re-enable is cheap).
	var disabled domain.Agent
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"storage_enabled": false,
	}, http.StatusOK, &disabled)
	require.False(t, disabled.StorageEnabled)
	require.Equal(t, "3Gi", disabled.StorageSize)

	// Over-max and malformed sizes are rejected on update too.
	var body map[string]map[string]string
	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"storage_size": "40Ti",
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])
	require.Contains(t, body["error"]["message"], "storage_size")

	doJSON(t, handler, http.MethodPatch, pathAgentsPrefix+agent.ID, map[string]any{
		"storage_size": "lots",
	}, http.StatusBadRequest, &body)
	require.Equal(t, "bad_request", body["error"]["code"])
}

func TestCreateAgentStoragePersistsThroughOutbox(t *testing.T) {
	t.Parallel()

	store := storage.NewMemoryStore()
	handler := New(storageTestConfig(), store)
	squad := createStorageTestSquad(t, handler)

	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name":            "outbox-1",
		"storage_enabled": true,
		"storage_size":    "7Gi",
	}, http.StatusCreated, &agent)

	// The queued upsert payload must carry the storage fields so the CR
	// writer can render spec.storage.
	events, err := store.ListKubernetesOutbox(context.Background(), domain.KubernetesOutboxPending, 10)
	require.NoError(t, err)
	var payload *domain.KubernetesOutboxPayload
	for _, event := range events {
		if event.AggregateType != domain.KubernetesAggregateAgent || event.AggregateID != agent.ID {
			continue
		}
		var p domain.KubernetesOutboxPayload
		require.NoError(t, json.Unmarshal(event.Payload, &p))
		payload = &p
	}
	require.NotNil(t, payload, "no pending agent upsert event found")
	require.NotNil(t, payload.Agent)
	require.True(t, payload.Agent.StorageEnabled)
	require.Equal(t, "7Gi", payload.Agent.StorageSize)
}
