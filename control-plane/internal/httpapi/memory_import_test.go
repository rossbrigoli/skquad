// S-309 tests — bulk memory-import endpoint (owner-auth, trusted
// provenance, batch idempotency).

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

const pathMemoryImport = "/memory/import"

// importHandler builds a dev-mode handler with embeddings enabled and
// the given embedder, plus a squad+agent owned by the dev principal.
func importHandler(t *testing.T, embedder EmbeddingsClient) (http.Handler, *storage.MemoryStore, string, string) {
	t.Helper()
	handler, store, fw := memorySearchHandler(t, embedder)
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, pathSquads, map[string]any{"name": "Import Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{
		"name": "Importer", "role": "worker", "system_prompt": "hi",
	}, http.StatusCreated, &agent)
	_ = fw
	return handler, store, agent.ID, squad.ID
}

func importBody(batchID string, contents ...string) map[string]any {
	items := make([]map[string]any, 0, len(contents))
	for _, c := range contents {
		items = append(items, map[string]any{"content": c, "tags": []string{"migrated"}})
	}
	return map[string]any{"batch_id": batchID, "items": items}
}

func shaOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestMemoryImportStoresTrustedApprovedRows(t *testing.T) {
	handler, store, agentID, squadID := importHandler(t, &fakeEmbeddings{})

	var res map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		importBody("batch-1", "Ross prefers Adelaide timezones", "Project uses k3s v1.35"),
		http.StatusOK, &res)
	require.Equal(t, "batch-1", res["batch_id"])
	require.Equal(t, float64(2), res["imported"])
	require.Equal(t, float64(0), res["skipped"])
	require.Equal(t, float64(0), res["failed"])

	mem, err := store.ListAgentMemory(context.Background(), agentID, squadID, nil, 10)
	require.NoError(t, err)
	require.Len(t, mem, 2)
	byContent := map[string]*domain.AgentMemory{}
	for _, m := range mem {
		byContent[m.Content] = m
	}
	item := byContent["Ross prefers Adelaide timezones"]
	require.NotNil(t, item)
	require.Equal(t, "trusted_user", item.TrustLevel)
	require.Equal(t, "migrated", item.Provenance)
	require.Equal(t, "approved", item.ReviewStatus)
	require.NotEmpty(t, item.Embedding) // embedder was available

	var meta map[string]any
	require.NoError(t, json.Unmarshal(item.Metadata, &meta))
	require.Equal(t, "memory_import", meta["kind"])
	require.Equal(t, "batch-1", meta["batch_id"])
	require.Equal(t, shaOf("Ross prefers Adelaide timezones"), meta["content_sha256"])
	require.Equal(t, []any{"migrated"}, meta["tags"])
}

func TestMemoryImportResendIsIdempotent(t *testing.T) {
	handler, store, agentID, squadID := importHandler(t, &fakeEmbeddings{})

	body := importBody("batch-1", "alpha memory", "beta memory")
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport, body,
		http.StatusOK, &map[string]any{})

	// Exact resend: everything skipped, nothing duplicated.
	var res map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport, body,
		http.StatusOK, &res)
	require.Equal(t, float64(0), res["imported"])
	require.Equal(t, float64(2), res["skipped"])

	// Same batch id, one old + one new item: only the new one imports.
	var res2 map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		importBody("batch-1", "alpha memory", "gamma memory"),
		http.StatusOK, &res2)
	require.Equal(t, float64(1), res2["imported"])
	require.Equal(t, float64(1), res2["skipped"])

	mem, err := store.ListAgentMemory(context.Background(), agentID, squadID, nil, 50)
	require.NoError(t, err)
	require.Len(t, mem, 3) // alpha, beta, gamma — no duplicates
}

func TestMemoryImportDuplicateWithinBatch(t *testing.T) {
	handler, store, agentID, squadID := importHandler(t, &fakeEmbeddings{})

	var res map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		importBody("batch-dup", "same content twice", "same content twice"),
		http.StatusOK, &res)
	require.Equal(t, float64(1), res["imported"])
	require.Equal(t, float64(1), res["skipped"])

	mem, err := store.ListAgentMemory(context.Background(), agentID, squadID, nil, 50)
	require.NoError(t, err)
	require.Len(t, mem, 1)
}

func TestMemoryImportOversizeItemRejected(t *testing.T) {
	handler, store, agentID, squadID := importHandler(t, &fakeEmbeddings{})

	oversize := strings.Repeat("x", maxAgentMemoryContentChars+1)
	rec := doJSONRaw(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		importBody("batch-big", "fine item", oversize))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "exceeds")

	// Fail-fast: the valid item in the rejected batch was NOT stored.
	mem, err := store.ListAgentMemory(context.Background(), agentID, squadID, nil, 50)
	require.NoError(t, err)
	require.Empty(t, mem)

	// Exactly at the cap is fine.
	atCap := strings.Repeat("y", maxAgentMemoryContentChars)
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		importBody("batch-cap", atCap), http.StatusOK, &map[string]any{})
	mem, err = store.ListAgentMemory(context.Background(), agentID, squadID, nil, 50)
	require.NoError(t, err)
	require.Len(t, mem, 1)
}

func TestMemoryImportBatchLimits(t *testing.T) {
	handler, _, agentID, _ := importHandler(t, &fakeEmbeddings{})

	// Missing batch_id.
	items := []map[string]any{{"content": "orphan memory"}}
	rec := doJSONRaw(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		map[string]any{"items": items})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "batch_id")

	// Empty items.
	rec = doJSONRaw(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		map[string]any{"batch_id": "batch-empty", "items": []map[string]any{}})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// More than the per-request cap.
	tooMany := make([]map[string]any, 0, maxMemoryImportBatchItems+1)
	for i := 0; i <= maxMemoryImportBatchItems; i++ {
		tooMany = append(tooMany, map[string]any{"content": fmt.Sprintf("memory %d", i)})
	}
	rec = doJSONRaw(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		map[string]any{"batch_id": "batch-oversized", "items": tooMany})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "at most")
}

func TestMemoryImportNonOwnerForbidden(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	store := storage.NewMemoryStore()
	handler := NewWithOIDCAuthenticator(cfg, store, headerOIDC{
		authOwner:  {Email: "owner@example.com", Name: "Owner"},
		authViewer: {Email: "viewer@example.com", Name: "Viewer"},
	})

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{"name": "Private Squad"},
		http.StatusCreated, &squad)
	var agent domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents,
		map[string]any{"name": "Private Agent", "role": "worker", "system_prompt": "hi"},
		http.StatusCreated, &agent)

	// Even a talk grant does not make you the owner: import stays owner-only.
	var viewer domain.User
	doJSONAuth(t, handler, authViewer, http.MethodGet, pathAuthMe, nil, http.StatusOK, &viewer)
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAccessGrants,
		map[string]any{"grantee_type": "user", "grantee_id": viewer.ID, "permissions": "talk"},
		http.StatusCreated, &domain.AccessGrant{})

	var denied map[string]map[string]string
	doJSONAuth(t, handler, authViewer, http.MethodPost, pathAgentsPrefix+agent.ID+pathMemoryImport,
		importBody("batch-intruder", "should not land"), http.StatusForbidden, &denied)
	require.Equal(t, "forbidden", denied["error"]["code"])

	// Owner path still works on the same agent.
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathAgentsPrefix+agent.ID+pathMemoryImport,
		importBody("batch-owner", "owner memory"), http.StatusOK, &map[string]any{})
}

func TestMemoryImportEmbedderUnavailableStoresWithoutVector(t *testing.T) {
	handler, store, agentID, squadID := importHandler(t, &fakeEmbeddings{err: http.ErrServerClosed})

	var res map[string]any
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		importBody("batch-noembed", "stored anyway"), http.StatusOK, &res)
	require.Equal(t, float64(1), res["imported"])

	mem, err := store.ListAgentMemory(context.Background(), agentID, squadID, nil, 10)
	require.NoError(t, err)
	require.Len(t, mem, 1)
	require.Empty(t, mem[0].Embedding)
	require.Empty(t, mem[0].EmbeddingModel)
	require.Equal(t, "trusted_user", mem[0].TrustLevel)
}

func TestMemoryImportUnknownAgent(t *testing.T) {
	handler, _, _, _ := importHandler(t, &fakeEmbeddings{})
	rec := doJSONRaw(t, handler, http.MethodPost, pathAgentsPrefix+"00000000-0000-0000-0000-000000000000"+pathMemoryImport,
		importBody("batch-ghost", "nowhere to go"))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMemoryImportAuditsBatch(t *testing.T) {
	handler, store, agentID, squadID := importHandler(t, &fakeEmbeddings{})
	doJSON(t, handler, http.MethodPost, pathAgentsPrefix+agentID+pathMemoryImport,
		importBody("batch-audit", "audited memory"), http.StatusOK, &map[string]any{})

	entries, err := store.ListAudit(context.Background(), squadID, 50)
	require.NoError(t, err)
	var found bool
	for _, e := range entries {
		if e.Action != "memory.import" {
			continue
		}
		found = true
		require.Equal(t, "user", e.ActorType)
		require.Equal(t, "agent", e.ResourceType)
		require.Equal(t, agentID, e.ResourceID)
		var meta map[string]any
		require.NoError(t, json.Unmarshal(e.Metadata, &meta))
		require.Equal(t, "batch-audit", meta["batch_id"])
		require.Equal(t, float64(1), meta["imported"])
		require.Equal(t, float64(0), meta["skipped"])
	}
	require.True(t, found, "expected a memory.import audit entry")
}
