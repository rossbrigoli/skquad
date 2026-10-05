// Command gateway runs the skquad Tool Gateway: the governed egress plane
// (docs/tool-gateway.md §5.2). TG-1 scope: skeleton pipeline, policy cache
// client, echo driver, fail-closed readiness, kill switch.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/boundary"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/config"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	webdriver "github.com/rossbrigoli/skquad/tool-gateway/internal/drivers/web"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/httpapi"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	policyClient := policy.NewClient(cfg.CPBaseURL, cfg.PolicyTTL, cfg.PolicyTimeout)

	var auditSink audit.Emitter
	switch cfg.AuditSink {
	case "stdout":
		auditSink = audit.NewStdoutEmitter(os.Stdout)
	default:
		log.Fatalf("unknown audit sink %q (TG-1 supports: stdout)", cfg.AuditSink)
	}

	enabled := &atomic.Bool{}
	enabled.Store(cfg.Enabled)

	srv := httpapi.New(httpapi.Deps{
		Policy:       policyClient,
		PolicyClient: policyClient,
		Boundary:     boundary.NewVerifier(cfg.BoundaryVerifier),
		Audit:        auditSink,
		Enabled:      enabled,
		Drivers: map[string]drivers.Driver{
			"echo": drivers.Echo{},
			"web":  webdriver.New(),
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Keep the readiness signal fresh: gateway is NOT ready until the CP
	// policy path has answered at least once (fail-closed launch, §5.2).
	httpapi.StartProbeLoop(ctx, policyClient, cfg.ProbeAgent, cfg.PolicyProbeGap)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("tool-gateway listening on %s (enabled=%t, cp=%s, boundary=%s)",
		cfg.Addr, cfg.Enabled, cfg.CPBaseURL, cfg.BoundaryVerifier)
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}
