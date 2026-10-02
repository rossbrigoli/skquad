// Package backfill (S-212) re-embeds agent memories whose
// embedding_model differs from the currently configured model.
//
// Idempotent by construction: the selection predicate is
// `embedding_model IS DISTINCT FROM <current>` and each row is only
// rewritten after a successful embed, so a re-run after completion is
// a no-op. A model swap re-embeds every row once. Rows that fail to
// embed keep their old model value and are retried on the next run;
// the progress guard stops the loop when a full batch makes no
// progress so a dead embedder cannot spin forever.
package backfill

import (
	"context"
	"log"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// RowStore is the storage surface the backfill needs.
// *storage.PostgresStore satisfies it.
type RowStore interface {
	ListMemoriesNeedingEmbedding(ctx context.Context, currentModel string, limit int) ([]*domain.AgentMemory, error)
	SetAgentMemoryEmbedding(ctx context.Context, id string, embedding []float64, model string) error
}

// Embedder generates the vectors. *embeddings.Client satisfies it.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// Stats reports the outcome of one backfill run.
type Stats struct {
	Scanned  int
	Embedded int
	Failed   int
}

// Run drains the pending-embed queue for currentModel. It returns when
// no rows remain, the batch stops making progress, or ctx is done.
func Run(ctx context.Context, store RowStore, embedder Embedder, currentModel string, batchSize int) (Stats, error) {
	if batchSize <= 0 {
		batchSize = 32
	}
	stats := Stats{}
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		rows, err := store.ListMemoriesNeedingEmbedding(ctx, currentModel, batchSize)
		if err != nil {
			return stats, err
		}
		if len(rows) == 0 {
			return stats, nil
		}
		embeddedBefore := stats.Embedded
		for _, row := range rows {
			stats.Scanned++
			vec, err := embedder.Embed(ctx, row.Content)
			if err != nil {
				stats.Failed++
				log.Printf("embed-backfill: embed failed memory=%s: %v", row.ID, err)
				continue
			}
			if err := store.SetAgentMemoryEmbedding(ctx, row.ID, vec, currentModel); err != nil {
				stats.Failed++
				log.Printf("embed-backfill: store failed memory=%s: %v", row.ID, err)
				continue
			}
			stats.Embedded++
		}
		if len(rows) < batchSize {
			return stats, nil
		}
		if stats.Embedded == embeddedBefore {
			log.Printf("embed-backfill: no progress in batch (all %d rows failed) — stopping", len(rows))
			return stats, nil
		}
	}
}
