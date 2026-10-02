// S-212: control-plane-side registration of the embedder model in the
// LiteLLM gateway.
//
// The embedder (llama.cpp + Qwen3-Embedding-0.6B, Vulkan backend per
// the vendor-neutral scope) is a platform-internal OpenAI-compatible
// endpoint. The control plane registers it in the gateway at startup —
// the same /model/new + rollout-reload mechanism used for chat models
// (gateway_models.go) — so agents and the control plane both reach it
// through the unified gateway path. Registration is idempotent:
// existing deployment → /model/update in place; no reload is issued
// when nothing changed.
package httpapi

import (
	"context"
	"log"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

// embedderPlaceholderAPIKey satisfies litellm's non-empty api_key
// requirement for the unauthenticated in-cluster embedder. It is not
// a credential and grants nothing outside the pod network.
const embedderPlaceholderAPIKey = "skquad-internal" // #nosec G101 -- non-secret placeholder; embedder has no auth

// RegisterEmbedderGatewayModel starts the idempotent embedder-model
// registration in the background. No-op unless memory embeddings are
// enabled with a model and gateway admin credentials configured.
func RegisterEmbedderGatewayModel(ctx context.Context, cfg *config.Config) {
	if cfg == nil || !cfg.MemoryEmbeddingsEnabled || cfg.MemoryEmbeddingModel == "" {
		return
	}
	if cfg.LiteLLMAdminURL == "" || cfg.LiteLLMMasterKey == "" {
		log.Printf("embedder gateway registration disabled: LiteLLM admin URL/master key not configured")
		return
	}
	gw, err := newLiteLLMGatewayClient(cfg.LiteLLMAdminURL, cfg.LiteLLMMasterKey)
	if err != nil {
		log.Printf("embedder gateway registration disabled: %v", err)
		return
	}
	spec := GatewayModelSpec{
		ModelName:    cfg.MemoryEmbeddingModel,
		LitellmModel: "openai/" + cfg.MemoryEmbeddingModel,
		APIBase:      embedderServiceBaseURL(cfg),
		// The embedder itself is unauthenticated in-cluster; litellm
		// requires a non-empty api_key field. Placeholder, not a secret.
		APIKey: embedderPlaceholderAPIKey, // #nosec G101 -- non-secret placeholder, embedder has no auth
	}
	reloader := embedderReloaderOrNil(cfg)

	go func() {
		// The gateway may still be booting (migrations, pod
		// scheduling). Retry with backoff for a bounded window; never
		// block serving.
		for attempt := 0; attempt < 30; attempt++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
			created, err := registerEmbedderOnce(ctx, gw, reloader, spec)
			if err != nil {
				log.Printf("embedder gateway registration attempt %d failed: %v", attempt+1, err)
				continue
			}
			if created {
				log.Printf("embedder model %q registered in gateway (router reload issued)", spec.ModelName)
			} else {
				log.Printf("embedder model %q already registered — verified, no reload needed", spec.ModelName)
			}
			return
		}
		log.Printf("embedder gateway registration gave up after 30 attempts; memory_search will 502 until the model is registered")
	}()
}

// registerEmbedderOnce performs one idempotent registration pass and
// reports whether a new deployment was created (reload needed).
func registerEmbedderOnce(ctx context.Context, gw *liteLLMGatewayClient, reloader GatewayReloader, spec GatewayModelSpec) (bool, error) {
	deployments, err := gw.ListModelDeployments(ctx)
	if err != nil {
		return false, err
	}
	for _, d := range deployments {
		if d.ModelName == spec.ModelName {
			if err := gw.UpdateModelDeployment(ctx, d.DeploymentID, spec); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	if _, err := gw.DeployModel(ctx, spec); err != nil {
		return false, err
	}
	// The litellm router does not pick up DB-persisted models without
	// a restart (same contract as chat models, S-GWREG).
	if reloader != nil {
		if err := reloader.ReloadGateway(ctx); err != nil {
			log.Printf("embedder gateway reload FAILED (model persisted but router stale until next restart): %v", err)
		}
	} else {
		log.Printf("embedder gateway reload skipped: reloader not configured — restart skquad-llm-gateway manually to pick up the embedder model")
	}
	return true, nil
}

// embedderReloaderOrNil builds the gateway reloader from in-cluster
// config when available; nil otherwise (dev mode).
func embedderReloaderOrNil(cfg *config.Config) GatewayReloader {
	built, err := newGatewayReloader(cfg)
	if err != nil || built == nil {
		return nil
	}
	return built
}

// embedderServiceBaseURL resolves the in-cluster embedder endpoint
// from the config override (tests / non-standard namespaces).
func embedderServiceBaseURL(cfg *config.Config) string {
	if cfg.MemoryEmbedderURL != "" {
		return cfg.MemoryEmbedderURL
	}
	return "http://skquad-embedder.skquad.svc.cluster.local:8080/v1"
}
