// S-309: bulk memory-import endpoint.
//
// Owner-authenticated bulk write into agent_memory. Until now the only
// write path was agent task-completion (persist_memory, pending_review
// trust); this lets an agent owner migrate curated memory (e.g. a
// curated OpenClaw MEMORY.md) directly, as trusted_user / migrated /
// approved rows.
//
// Contract:
//   POST /api/v1/agents/{agentID}/memory/import
//   body: {"batch_id": "<required>", "items": [{"content": "...", "tags": ["..."]}]}
//   resp: {"batch_id": "...", "imported": N, "skipped": N, "failed": N}
//
// Limits: max 100 items per request; each item capped at
// maxAgentMemoryContentChars (4000) runes, same as every other memory
// write path. Validation is fail-fast: any invalid item rejects the
// whole batch before anything is stored.
//
// Idempotency: rows are keyed by (agent_id, batch_id,
// content_sha256). Re-sending the same batch with identical content
// skips every row (imported=0, skipped=N) — safe to retry after a
// partial network failure.
//
// Embeddings: each item goes through the same best-effort
// embedMemoryText path as task-completion memory — when the embedder
// is unavailable the item is still stored without a vector and the
// backfill job catches it later.

package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// maxMemoryImportBatchItems bounds one import request.
const maxMemoryImportBatchItems = 100

// memoryImportItem is one candidate long-term memory.
type memoryImportItem struct {
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

// memoryImportRequest is the bulk import envelope.
type memoryImportRequest struct {
	BatchID string             `json:"batch_id"`
	Items   []memoryImportItem `json:"items"`
}

// importAgentMemory handles POST /agents/{agentID}/memory/import.
// Owner-only (loadOwnedAgent): squad owners and nobody else — not even
// platform admins — may write trusted memory into someone's agent.
func (s *Server) importAgentMemory(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadOwnedAgent(w, r)
	if !ok {
		return
	}
	var req memoryImportRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	batchID := strings.TrimSpace(req.BatchID)
	if batchID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "batch_id is required")
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "at least one memory item is required")
		return
	}
	if len(req.Items) > maxMemoryImportBatchItems {
		writeError(w, http.StatusBadRequest, "batch_too_large",
			fmt.Sprintf("a batch may contain at most %d items", maxMemoryImportBatchItems))
		return
	}

	memories, err := buildImportMemories(target, batchID, req.Items)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_item", err.Error())
		return
	}
	s.embedImportedMemories(r, memories)

	imported, err := s.store.ImportAgentMemory(r.Context(), memories)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	skipped := len(memories) - imported
	failed := 0

	auditMeta, _ := json.Marshal(map[string]any{
		"batch_id":  batchID,
		"requested": len(req.Items),
		"imported":  imported,
		"skipped":   skipped,
		"failed":    failed,
	})
	s.recordUserAudit(r, "memory.import", "agent", target.ID, target.SquadID, auditMeta)

	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": batchID,
		"imported": imported,
		"skipped":  skipped,
		"failed":   failed,
	})
}

// buildImportMemories validates every item and turns the batch into
// domain rows with trusted provenance and idempotency metadata. Any
// invalid item fails the whole batch (fail-fast, no partial writes).
func buildImportMemories(target *domain.Agent, batchID string, items []memoryImportItem) ([]*domain.AgentMemory, error) {
	memories := make([]*domain.AgentMemory, 0, len(items))
	for i, item := range items {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			return nil, fmt.Errorf("item %d: content is required", i)
		}
		if utf8.RuneCountInString(content) > maxAgentMemoryContentChars {
			return nil, fmt.Errorf("item %d: content exceeds %d characters", i, maxAgentMemoryContentChars)
		}
		sum := sha256.Sum256([]byte(content))
		contentSHA := hex.EncodeToString(sum[:])
		tags := item.Tags
		if tags == nil {
			tags = []string{}
		}
		metadata, err := json.Marshal(map[string]any{
			"kind":           "memory_import",
			"batch_id":       batchID,
			"content_sha256": contentSHA,
			"tags":           tags,
		})
		if err != nil {
			return nil, fmt.Errorf("item %d: failed to encode metadata", i)
		}
		memories = append(memories, &domain.AgentMemory{
			AgentID:      target.ID,
			SquadID:      target.SquadID,
			Content:      content,
			TrustLevel:   "trusted_user",
			Provenance:   "migrated",
			ReviewStatus: "approved",
			Metadata:     metadata,
		})
	}
	return memories, nil
}

// embedImportedMemories fills in write-time embeddings for the batch.
// Best-effort per the S-212 contract: a nil/failed embedder stores
// items without vectors and the backfill job re-embeds later. Split
// out of buildImportMemories so validation stays embedder-free.
func (s *Server) embedImportedMemories(r *http.Request, memories []*domain.AgentMemory) {
	for _, memory := range memories {
		embedding, embeddingModel := s.embedMemoryText(r.Context(), memory.Content)
		memory.Embedding = embedding
		memory.EmbeddingModel = embeddingModel
	}
}
