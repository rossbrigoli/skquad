// Package main is the entrypoint for the skquad control-plane API server.
//
// It serves the REST API (see docs/api-design.md), performs OIDC authN and
// user RBAC, and creates the Squad/Agent custom resources that the operator
// reconciles.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/auth"
	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/httpapi"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	store, closeStore := openStore(cfg)
	defer closeStore()

	oidcAuth := setupOIDC(cfg)
	crWriter := setupCRWriter(cfg, store)

	// The execution reaper runs on every store (dev parity) and on every
	// replica: its store update is conditional and idempotent.
	go httpapi.RunExecutionReaper(context.Background(), store, cfg.ReaperInterval, cfg.ReaperGrace)
	slog.Info("started task execution reaper", "interval", cfg.ReaperInterval, "grace", cfg.ReaperGrace)

	// S-173: consult timeout/SLA. Posts synthetic consult_timeout replies
	// for agent consults that went unanswered past their deadline. Idempotent
	// per consult, safe on every replica.
	go httpapi.RunConsultTimeoutSweeper(context.Background(), store, cfg.ConsultSweepInterval)
	slog.Info("started consult timeout sweeper", "interval", cfg.ConsultSweepInterval, "default_deadline", cfg.ConsultTimeout)

	// S-197: stuck-task scanner. Alerts squad owners when an in-progress
	// task shows no thread activity and no heartbeat progress for the
	// threshold (default 24h). Deduped per task via the notifications
	// table; safe on every replica.
	go httpapi.RunStuckTaskScanner(context.Background(), store, cfg.StuckScanInterval, cfg.TaskStuckThreshold)
	slog.Info("started stuck task scanner", "interval", cfg.StuckScanInterval, "threshold", cfg.TaskStuckThreshold)

	// S-198: notification retention sweep. Purges READ notifications
	// older than the retention window (default 90 days) plus the
	// 'inbox.deleted' audit rows written by the inbox delete endpoint.
	// Unread notifications and inbox messages are never auto-removed
	// (S-193). Idempotent; safe on every replica.
	go httpapi.RunNotificationRetention(context.Background(), store, cfg.NotificationSweepInterval, cfg.NotificationRetention)
	slog.Info("started notification retention sweep", "interval", cfg.NotificationSweepInterval, "retention", cfg.NotificationRetention)

	providerKeys := setupProviderKeys(cfg, store)

	handler := httpapi.NewWithDependencies(cfg, store, oidcAuth, crWriter, providerKeys)
	// S-212: ensure the embedder model is registered in the LiteLLM
	// gateway (idempotent; retries in the background while the gateway
	// boots). No-op unless memory embeddings are enabled.
	httpapi.RegisterEmbedderGatewayModel(context.Background(), cfg)

	server := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
	}

	slog.Info("starting skquad control-plane API", "addr", cfg.Addr, "auth_mode", cfg.AuthMode)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serve api", "error", err)
		os.Exit(1)
	}
}

// openStore returns the configured message store (in-memory for dev, or
// Postgres when SKQUAD_DATABASE_URL is set) plus its release function.
func openStore(cfg *config.Config) (httpapi.Store, func()) {
	if cfg.DatabaseURL == "" {
		slog.Info("using in-memory control-plane store")
		return storage.NewMemoryStore(), func() {
			// Intentionally empty: the in-memory store holds no external
			// resources, so there is nothing to release on shutdown.
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pgStore, err := storage.NewPostgresStore(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("connect postgres store", "error", err)
		os.Exit(1)
	}
	slog.Info("using postgres control-plane store")
	return pgStore, pgStore.Close
}

// setupOIDC builds the OIDC authenticator when auth mode is OIDC,
// otherwise returns nil (dev/no-auth mode).
func setupOIDC(cfg *config.Config) httpapi.OIDCAuthenticator {
	if cfg.AuthMode != config.AuthOIDC {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	authenticator, err := auth.NewOIDCAuthenticator(ctx, cfg.IssuerURL, cfg.Audience)
	if err != nil {
		slog.Error("configure oidc auth", "error", err)
		os.Exit(1)
	}
	return authenticator
}

// setupCRWriter builds the Kubernetes CR writer when K8s is enabled and
// starts the outbox worker when the store supports it.
func setupCRWriter(cfg *config.Config, store httpapi.Store) httpapi.CRWriter {
	if !cfg.K8sEnabled {
		return nil
	}
	writer, err := kube.NewCRWriter(cfg)
	if err != nil {
		slog.Error("configure kubernetes CR writer", "error", err)
		os.Exit(1)
	}
	if outboxStore, ok := store.(storage.KubernetesOutboxStore); ok {
		go kube.RunOutboxWorker(context.Background(), outboxStore, writer)
		slog.Info("started kubernetes outbox worker")
	}
	slog.Info("using kubernetes CR writer", "namespace", cfg.K8sNamespace, "group_version", cfg.K8sGroupVersion)
	return writer
}

// setupProviderKeys builds the managed provider-key secret store when K8s
// is enabled and wraps any pre-existing literal keys (S-155).
func setupProviderKeys(cfg *config.Config, store httpapi.Store) httpapi.ProviderKeyStore {
	if !cfg.K8sEnabled {
		return nil
	}
	keys, err := kube.NewSecretStore(cfg)
	if err != nil {
		slog.Error("configure provider key secret store", "error", err)
		os.Exit(1)
	}
	// S-155: wrap any pre-existing literal provider keys into managed
	// Secrets before serving.
	if wrapped, err := httpapi.MigrateLegacyProviderKeys(context.Background(), store, keys); err != nil {
		slog.Error("provider key migration", "error", err)
	} else if wrapped > 0 {
		slog.Info("wrapped legacy literal provider keys into managed Secrets", "count", wrapped)
	}
	return keys
}
