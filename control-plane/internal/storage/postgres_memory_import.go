// S-309: bulk memory import — Postgres implementation.
//
// Idempotency is enforced at the database level by the partial unique
// index idx_memory_import_idempotency (migration 0069) on
// (agent_id, metadata->>'batch_id', metadata->>'content_sha256') for
// rows carrying both keys. INSERT ... ON CONFLICT DO NOTHING makes a
// duplicate import a no-op; RowsAffected tells us what was actually
// inserted. The whole batch runs in one transaction: an oversize or
// invalid item rolls everything back.

package storage

import (
	"context"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// ImportAgentMemory inserts owner-supplied memory items idempotently
// and returns the number of rows actually inserted. See the
// AgentMemoryStore interface contract for the dedupe key.
func (p *PostgresStore) ImportAgentMemory(ctx context.Context, memories []*domain.AgentMemory) (int, error) {
	if len(memories) == 0 {
		return 0, nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, mapPgErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	imported := 0
	for _, memory := range memories {
		if memory == nil {
			return 0, ErrInvalidInput
		}
		batchID, sha := importIdempotencyKeys(memory.Metadata)
		if batchID == "" || sha == "" {
			return 0, ErrInvalidInput
		}
		applyMemoryDefaults(memory)
		tag, err := tx.Exec(ctx, `
			INSERT INTO agent_memory (
				agent_id, squad_id, content, raw_content, trust_level, provenance,
				review_status, embedding, embedding_model, source_task_id, metadata
			)
			VALUES (
				$1, nullif($2, '')::uuid, $3, $4, $5, $6, $7,
				nullif($8, '')::vector, $9, nullif($10, '')::uuid, $11
			)
			ON CONFLICT (agent_id, (metadata->>'batch_id'), (metadata->>'content_sha256'))
				WHERE metadata ? 'batch_id' AND metadata ? 'content_sha256'
				DO NOTHING
		`, memory.AgentID, memory.SquadID, memory.Content, memory.RawContent,
			memory.TrustLevel, memory.Provenance, memory.ReviewStatus,
			vectorLiteral(memory.Embedding), memory.EmbeddingModel, memory.SourceTaskID,
			defaultJSON(memory.Metadata, "{}"))
		if err != nil {
			return 0, mapPgErr(err)
		}
		imported += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, mapPgErr(err)
	}
	return imported, nil
}
