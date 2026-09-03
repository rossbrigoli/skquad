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

	var store httpapi.Store
	var closeStore func()
	if cfg.DatabaseURL == "" {
		store = storage.NewMemoryStore()
		closeStore = func() {}
		slog.Info("using in-memory control-plane store")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		pgStore, err := storage.NewPostgresStore(ctx, cfg.DatabaseURL)
		if err != nil {
			slog.Error("connect postgres store", "error", err)
			os.Exit(1)
		}
		store = pgStore
		closeStore = pgStore.Close
		slog.Info("using postgres control-plane store")
	}
	defer closeStore()

	var oidcAuth httpapi.OIDCAuthenticator
	if cfg.AuthMode == config.AuthOIDC {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		authenticator, err := auth.NewOIDCAuthenticator(ctx, cfg.IssuerURL, cfg.Audience)
		if err != nil {
			slog.Error("configure oidc auth", "error", err)
			os.Exit(1)
		}
		oidcAuth = authenticator
	}

	var crWriter httpapi.CRWriter
	if cfg.K8sEnabled {
		writer, err := kube.NewCRWriter(cfg)
		if err != nil {
			slog.Error("configure kubernetes CR writer", "error", err)
			os.Exit(1)
		}
		crWriter = writer
		if outboxStore, ok := store.(storage.KubernetesOutboxStore); ok {
			go kube.RunOutboxWorker(context.Background(), outboxStore, writer)
			slog.Info("started kubernetes outbox worker")
		}
		slog.Info("using kubernetes CR writer", "namespace", cfg.K8sNamespace, "group_version", cfg.K8sGroupVersion)
	}

	// The execution reaper runs on every store (dev parity) and on every
	// replica: its store update is conditional and idempotent.
	go httpapi.RunExecutionReaper(context.Background(), store, cfg.ReaperInterval, cfg.ReaperGrace)
	slog.Info("started task execution reaper", "interval", cfg.ReaperInterval, "grace", cfg.ReaperGrace)

	handler := httpapi.NewWithDependencies(cfg, store, oidcAuth, crWriter)

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
