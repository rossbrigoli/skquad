// Command k8s-mcp is a small, deliberately-scoped in-cluster MCP server
// exposing Kubernetes read tools and allowlisted ArgoCD gitops tools to
// skquad agents (MIG-7 / S-313).
//
// It authenticates callers with a bearer token (skquad mcp resource
// auth_kind=bearer) and authenticates to the Kubernetes API with its own
// least-privilege ServiceAccount — the kubeconfig never touches the agent
// pod. See k8s-mcp/README.md for the tool catalog and scoping model.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rossbrigoli/skquad/k8s-mcp/internal/config"
	"github.com/rossbrigoli/skquad/k8s-mcp/internal/k8s"
	"github.com/rossbrigoli/skquad/k8s-mcp/internal/mcpserver"
	"github.com/rossbrigoli/skquad/k8s-mcp/internal/tools"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("k8s-mcp: %v", err)
	}
	// Fail fast on unreadable cluster credentials rather than serving a
	// server whose every tool call would fail.
	if _, err := os.ReadFile(cfg.SATokenPath); err != nil {
		log.Fatalf("k8s-mcp: service account token not readable at %s: %v", cfg.SATokenPath, err)
	}
	client, err := k8s.New(cfg.APIURL, cfg.CAPath, cfg.SATokenPath, cfg.Timeout)
	if err != nil {
		log.Fatalf("k8s-mcp: %v", err)
	}

	reg := tools.NewRegistry(cfg, client)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mcpserver.New(cfg.AuthToken, reg).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("k8s-mcp listening on %s (namespaces=%v, argocdApps=%d allowlisted)", cfg.Addr, cfg.AllowedNamespaces, len(cfg.ArgoAllowedApps))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("k8s-mcp: %v", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		log.Printf("k8s-mcp: shut down cleanly")
	}
}
