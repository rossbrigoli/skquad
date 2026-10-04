// S-212 — agent-facing semantic memory search.
//
// GET /api/v1/agents/me/memory/search?q=<text>&limit=<k>
//
// authenticateAgent-scoped: the query embeds through the LiteLLM
// gateway (single embedding path, unified auth/metering) and the store
// hard-scopes results to the calling agent's own rows. Trust gating
// lives in the store query (see SearchAgentMemory): rejected memories
// are never recalled; pending_review/approved rows across all trust
// levels are.
//
// Failure semantics:
//   - feature disabled / embedder unconfigured → 503 memory_search_disabled
//   - embedder unreachable at query time     → 502 embedder_unavailable
//   - empty q                               → 400
package httpapi

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	defaultMemorySearchLimit = 5
	memorySearchLimitCap     = 20
	memorySearchTimeout      = 30 * time.Second
)

// memorySearchResultView is the runtime-facing ranked hit.
type memorySearchResultView struct {
	ID           string  `json:"id"`
	Content      string  `json:"content"`
	Score        float64 `json:"score"`
	TrustLevel   string  `json:"trust_level"`
	ReviewStatus string  `json:"review_status"`
	Provenance   string  `json:"provenance"`
	CreatedAt    string  `json:"created_at"`
}

// searchMyAgentMemory handles GET /agents/me/memory/search.
func (s *Server) searchMyAgentMemory(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())

	cfg, err := s.store.GetBuiltinTool(r.Context(), domain.BuiltinToolMemorySearch)
	if err != nil && !isNotFound(err) {
		writeStorageError(w, err)
		return
	}
	if err != nil || cfg == nil || !cfg.Enabled {
		writeError(w, http.StatusForbidden, "tool_disabled", "memory_search is disabled by platform policy")
		return
	}
	if !s.cfg.MemoryEmbeddingsEnabled || s.cfg.MemoryEmbeddingModel == "" || s.embeddings == nil {
		writeError(w, http.StatusServiceUnavailable, "memory_search_disabled",
			"semantic memory search is not enabled on this platform (SKQUAD_MEMORY_EMBEDDINGS_ENABLED)")
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	limit, ok := resolveMemorySearchLimit(w, cfg, r.URL.Query().Get("limit"))
	if !ok {
		return
	}

	timeout := memorySearchTimeout
	if policyTimeout := policyInt(cfg.Policy, "timeoutSeconds"); policyTimeout > 0 {
		timeout = time.Duration(policyTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	queryEmbedding, err := s.embeddings.Embed(ctx, q)
	if err != nil {
		log.Printf("memory_search: embed query failed agent=%s: %v", principal.Agent.ID, err)
		writeError(w, http.StatusBadGateway, "embedder_unavailable", "embedding gateway request failed")
		return
	}

	hits, err := s.store.SearchAgentMemory(ctx, principal.Agent.ID, principal.Agent.SquadID, queryEmbedding, limit)
	if err != nil {
		writeStorageError(w, err)
		return
	}

	results := make([]memorySearchResultView, 0, len(hits))
	for _, hit := range hits {
		m := hit.Memory
		results = append(results, memorySearchResultView{
			ID:           m.ID,
			Content:      m.Content,
			Score:        hit.Score,
			TrustLevel:   m.TrustLevel,
			ReviewStatus: m.ReviewStatus,
			Provenance:   m.Provenance,
			CreatedAt:    m.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "model": s.cfg.MemoryEmbeddingModel})
}

// resolveMemorySearchLimit parses the ?limit= parameter and clamps it
// to the tool policy's maxResults (S-189 split out of
// searchMyAgentMemory).
func resolveMemorySearchLimit(w http.ResponseWriter, cfg *domain.BuiltinToolConfig, raw string) (int, bool) {
	limit := defaultMemorySearchLimit
	if raw = strings.TrimSpace(raw); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > memorySearchLimitCap {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 20")
			return 0, false
		}
		limit = parsed
	}
	if policyMax := policyInt(cfg.Policy, "maxResults"); policyMax > 0 && policyMax < limit {
		limit = policyMax
	}
	return limit, true
}

// isNotFound is a tiny helper so the disabled-check reads linearly
// without importing errors at yet another call site.
func isNotFound(err error) bool {
	return err == storage.ErrNotFound
}
