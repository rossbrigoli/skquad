// Command embed-backfill (S-212) re-embeds agent memories whose
// embedding_model differs from the currently configured model.
//
// Idempotent: a re-run after completion is a no-op (exit 0,
// embedded=0). The control-plane image ships this binary at
// /usr/local/bin/embed-backfill; the chart runs it as a Helm
// post-upgrade/post-install Job (embedder.backfillOnUpgrade), and it
// can also be run manually:
//   kubectl -n <ns> exec deploy/skquad-control-plane -- embed-backfill
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/backfill"
	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/embeddings"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func main() {
	batchSize := flag.Int("batch", 32, "rows per fetch")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("embed-backfill requires SKQUAD_DATABASE_URL (postgres)")
	}
	if !cfg.MemoryEmbeddingsEnabled || cfg.MemoryEmbeddingModel == "" {
		log.Println("embed-backfill: embeddings disabled or no model configured — nothing to do")
		return
	}
	gatewayBase := cfg.LiteLLMAdminURL
	if gatewayBase == "" {
		gatewayBase = cfg.LLMGatewayURL
	}
	if gatewayBase == "" || cfg.LiteLLMMasterKey == "" {
		log.Fatal("embed-backfill requires gateway URL (SKQUAD_LITELLM_ADMIN_URL or SKQUAD_LLM_GATEWAY_URL) + master key")
	}
	client, err := embeddings.NewClient(gatewayBase, cfg.LiteLLMMasterKey, cfg.MemoryEmbeddingModel)
	if err != nil {
		log.Fatalf("embeddings client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	store, err := storage.NewPostgresStore(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer store.Close()

	stats, err := backfill.Run(ctx, store, client, cfg.MemoryEmbeddingModel, *batchSize)
	fmt.Printf("embed-backfill: model=%s scanned=%d embedded=%d failed=%d\n",
		cfg.MemoryEmbeddingModel, stats.Scanned, stats.Embedded, stats.Failed)
	if err != nil {
		log.Fatalf("embed-backfill: %v", err)
	}
	if stats.Failed > 0 {
		os.Exit(1)
	}
}
