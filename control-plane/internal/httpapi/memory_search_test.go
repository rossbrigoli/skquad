// S-212 tests — agent-facing semantic memory search endpoint.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// fakeEmbeddings returns a deterministic vector derived from the text's
// first byte so tests can rank memories by construction.
type fakeEmbeddings struct {
	calls int
	err   error
}

func (f *fakeEmbeddings) Embed(_ context.Context, text string) ([]float64, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	seed := byte('a')
	if len(text) > 0 {
		seed = text[0]
	}
	return []float64{float64(seed), 1, 2, 3}, nil
}

// memorySearchHandler builds a dev handler with embeddings enabled and
// the memory_search builtin ENABLED (its seeded default).
func memorySearchHandler(t *testing.T, embedder EmbeddingsClient) (http.Handler, *storage.MemoryStore, *fakeCRWriter) {
	t.Helper()
	cfg := testConfig()
	cfg.MemoryEmbeddingsEnabled = true
	cfg.MemoryEmbeddingModel = "qwen3-embed-0.6b"
	store := storage.NewMemoryStore()
	fw := &fakeCRWriter{}
	handler := newServer(cfg, store, serverDeps{crWriter: fw, injectedEmbeddings: embedder})
	return handler, store, fw
}

func seedMemory(t *testing.T, store *storage.MemoryStore, agentID, squadID, content string, embedding []float64, reviewStatus string) *domain.AgentMemory {
	t.Helper()
	mem, err := store.CreateAgentMemory(context.Background(), &domain.AgentMemory{
		AgentID:      agentID,
		SquadID:      squadID,
		Content:      content,
		TrustLevel:   "distilled",
		Provenance:   "task_completion",
		ReviewStatus: reviewStatus,
		Embedding:    embedding,
	})
	require.NoError(t, err)
	return mem
}

func TestMemorySearchReturnsRankedResults(t *testing.T) {
	handler, store, fw := memorySearchHandler(t, &fakeEmbeddings{})
	agentID, token := agentWithCredential(t, handler, fw, "rag-squad", "ragger")

	// 'm'-prefixed vectors rank closest to a query starting with 'm'.
	close := seedMemory(t, store, agentID, "", "etcd tuning notes", []float64{109, 1, 2, 3}, "approved")
	far := seedMemory(t, store, agentID, "", "unrelated cooking notes", []float64{1, 1, 2, 3}, "approved")

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=memory+about+etcd", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Results []memorySearchResultView `json:"results"`
		Model   string                   `json:"model"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "qwen3-embed-0.6b", resp.Model)
	require.Len(t, resp.Results, 2)
	require.Equal(t, close.ID, resp.Results[0].ID)
	require.Equal(t, far.ID, resp.Results[1].ID)
	require.Greater(t, resp.Results[0].Score, resp.Results[1].Score)
	require.Greater(t, resp.Results[0].Score, 0.9)
}

func TestMemorySearchScopesToOwnAgent(t *testing.T) {
	handler, store, fw := memorySearchHandler(t, &fakeEmbeddings{})
	agentID, token := agentWithCredential(t, handler, fw, "scope-squad", "scoper")
	otherID, otherToken := agentWithCredential(t, handler, fw, "other-squad", "other")

	seedMemory(t, store, otherID, "", "secret of the other agent", []float64{109, 1, 2, 3}, "approved")
	mine := seedMemory(t, store, agentID, "", "my own memory", []float64{109, 1, 2, 3}, "approved")

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=mmmm", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Results []memorySearchResultView `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	require.Equal(t, mine.ID, resp.Results[0].ID)

	// The other agent sees only its own row.
	rec2 := doAgentRequest(t, handler, otherID, otherToken, http.MethodGet, "/api/v1/agents/me/memory/search?q=mmmm", nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	require.NotEqual(t, mine.ID, resp.Results[0].ID)
}

func TestMemorySearchExcludesRejected(t *testing.T) {
	handler, store, fw := memorySearchHandler(t, &fakeEmbeddings{})
	agentID, token := agentWithCredential(t, handler, fw, "trust-squad", "truster")

	seedMemory(t, store, agentID, "", "rejected memory", []float64{109, 1, 2, 3}, "rejected")
	approved := seedMemory(t, store, agentID, "", "approved memory", []float64{108, 1, 2, 3}, "approved")
	pending := seedMemory(t, store, agentID, "", "pending review memory", []float64{107, 1, 2, 3}, "pending_review")

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=mmmm", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Results []memorySearchResultView `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := []string{resp.Results[0].ID, resp.Results[1].ID}
	require.ElementsMatch(t, []string{approved.ID, pending.ID}, ids)
}

func TestMemorySearchExcludesRowsWithoutEmbedding(t *testing.T) {
	handler, store, fw := memorySearchHandler(t, &fakeEmbeddings{})
	agentID, token := agentWithCredential(t, handler, fw, "noembed-squad", "ne")
	seedMemory(t, store, agentID, "", "no vector here", nil, "approved")

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=anything", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Results []memorySearchResultView `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.Results)
}

func TestMemorySearchValidation(t *testing.T) {
	handler, _, fw := memorySearchHandler(t, &fakeEmbeddings{})
	agentID, token := agentWithCredential(t, handler, fw, "val-squad", "val")

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=x&limit=99", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=x&limit=0", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMemorySearchDisabledFeature(t *testing.T) {
	cfg := testConfig() // embeddings disabled
	fw := &fakeCRWriter{}
	handler := newServer(cfg, storage.NewMemoryStore(), serverDeps{crWriter: fw})
	agentID, token := agentWithCredential(t, handler, fw, "off-squad", "off")

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=x", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var body map[string]map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "memory_search_disabled", body["error"]["code"])
}

func TestMemorySearchToolDisabledByPolicy(t *testing.T) {
	handler, store, fw := memorySearchHandler(t, &fakeEmbeddings{})
	agentID, token := agentWithCredential(t, handler, fw, "policy-squad", "pol")
	ctx := context.Background()
	_, err := store.UpdateBuiltinTool(ctx, domain.BuiltinToolMemorySearch, boolPtr(false), nil, "admin")
	require.NoError(t, err)

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=x", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	var body map[string]map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "tool_disabled", body["error"]["code"])
}

func TestMemorySearchEmbedderFailure(t *testing.T) {
	handler, _, fw := memorySearchHandler(t, &fakeEmbeddings{err: context.DeadlineExceeded})
	agentID, token := agentWithCredential(t, handler, fw, "fail-squad", "fail")

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=x", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestMemorySearchUnauthenticated(t *testing.T) {
	handler, _, _ := memorySearchHandler(t, &fakeEmbeddings{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/me/memory/search?q=x", nil)
	rec := serve(handler, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMemorySearchPolicyMaxResultsCaps(t *testing.T) {
	handler, store, fw := memorySearchHandler(t, &fakeEmbeddings{})
	agentID, token := agentWithCredential(t, handler, fw, "cap-squad", "cap")
	for i := 0; i < 5; i++ {
		seedMemory(t, store, agentID, "", "mem", []float64{float64(100 - i), 1, 2, 3}, "approved")
	}
	ctx := context.Background()
	_, err := store.UpdateBuiltinTool(ctx, domain.BuiltinToolMemorySearch, nil, json.RawMessage(`{"maxResults": 2}`), "admin")
	require.NoError(t, err)

	rec := doAgentRequest(t, handler, agentID, token, http.MethodGet, "/api/v1/agents/me/memory/search?q=mmmm&limit=5", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Results []memorySearchResultView `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 2)
}

// boolPtr avoids importing the standard helper if none exists.
func boolPtr(b bool) *bool { return &b }

var _ = config.AuthDev
