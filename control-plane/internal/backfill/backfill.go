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
	"strings"

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
	// Skipped counts rows that can NEVER be embedded with the current
	// model (e.g. content exceeds the model's context window). These are
	// permanent, not transient: counting them as Failed made the PostSync
	// backfill hook fail forever on a single oversized memory, blocking
	// every deploy (S-301 follow-up). Skipped rows are logged and do not
	// fail the run.
	Skipped int
}

// isPermanentEmbedError reports whether an embed failure is a property
// of the row (too large for the model) rather than the embedder being
// down. Such rows will fail identically on every retry, so they must not
// block the run. Matched on the gateway's error text because the client
// surfaces the upstream LiteLLM/OpenAI error verbatim.
func isPermanentEmbedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"contextwindowexceeded",
		"exceed_context_size_error",
		"exceeds the available context size",
		"maximum context length",
		"input is too long",
		"context_length_exceeded",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
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
			embedRow(ctx, store, embedder, currentModel, row, &stats)
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

// embedRow embeds and persists one memory row, folding the outcome into
// stats (S-189 split out of Run). Failures are counted and logged; the
// batch continues.
func embedRow(ctx context.Context, store RowStore, embedder Embedder, currentModel string, row *domain.AgentMemory, stats *Stats) {
	stats.Scanned++
	vec, err := embedder.Embed(ctx, row.Content)
	if err != nil {
		if isPermanentEmbedError(err) {
			stats.Skipped++
			log.Printf("embed-backfill: skipping oversized memory=%s (exceeds model context window): %v", row.ID, err)
			return
		}
		stats.Failed++
		log.Printf("embed-backfill: embed failed memory=%s: %v", row.ID, err)
		return
	}
	if err := store.SetAgentMemoryEmbedding(ctx, row.ID, vec, currentModel); err != nil {
		stats.Failed++
		log.Printf("embed-backfill: store failed memory=%s: %v", row.ID, err)
		return
	}
	stats.Embedded++
}
