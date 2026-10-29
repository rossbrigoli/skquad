// S-309: bulk memory import — MemoryStore implementation.
//
// Mirrors the Postgres semantics: items are keyed for idempotency by
// (agent_id, metadata.batch_id, metadata.content_sha256). Re-sending
// the same triple skips instead of duplicating. The whole batch runs
// under one lock, so it is atomic with respect to other store callers.

package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// ImportAgentMemory (S-309) inserts pre-built memory items, skipping
// any whose (agent_id, batch_id, content_sha256) triple already
// exists. Returns the count of rows actually inserted.
func (m *MemoryStore) ImportAgentMemory(_ context.Context, memories []*domain.AgentMemory) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Validate everything first, then insert: an invalid item fails the
	// whole batch with no partial writes (matches the Postgres
	// transaction semantics).
	keys := make([][2]string, 0, len(memories))
	for _, memory := range memories {
		if memory == nil {
			return 0, ErrInvalidInput
		}
		if _, ok := m.agents[memory.AgentID]; !ok {
			return 0, ErrNotFound
		}
		if memory.SquadID != "" {
			if _, ok := m.squads[memory.SquadID]; !ok {
				return 0, ErrNotFound
			}
		}
		batchID, sha := importIdempotencyKeys(memory.Metadata)
		if batchID == "" || sha == "" {
			return 0, ErrInvalidInput
		}
		keys = append(keys, [2]string{batchID, sha})
	}
	imported := 0
	for i, memory := range memories {
		if m.importedMemoryExists(memory.AgentID, keys[i][0], keys[i][1]) {
			continue
		}
		created := cloneAgentMemory(memory)
		created.ID = uuid.NewString()
		if len(created.Metadata) == 0 {
			created.Metadata = []byte(`{}`)
		}
		applyMemoryDefaults(created)
		created.CreatedAt = time.Now().UTC()
		m.agentMemory[created.ID] = created
		imported++
	}
	return imported, nil
}

// importIdempotencyKeys extracts the batch_id / content_sha256 pair
// from an import item's metadata. Missing or non-string values come
// back empty, which callers treat as invalid input.
func importIdempotencyKeys(metadata json.RawMessage) (string, string) {
	if len(metadata) == 0 {
		return "", ""
	}
	var fields map[string]any
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return "", ""
	}
	batchID, _ := fields["batch_id"].(string)
	sha, _ := fields["content_sha256"].(string)
	return batchID, sha
}

// importedMemoryExists scans stored memories for a matching import
// triple. Linear scan is fine at in-memory store scale.
func (m *MemoryStore) importedMemoryExists(agentID, batchID, sha string) bool {
	for _, item := range m.agentMemory {
		if item.AgentID != agentID {
			continue
		}
		itemBatch, itemSHA := importIdempotencyKeys(item.Metadata)
		if itemBatch == batchID && itemSHA == sha {
			return true
		}
	}
	return false
}
