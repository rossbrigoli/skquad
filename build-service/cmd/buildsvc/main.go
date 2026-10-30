// Command buildsvc is the skquad build service: a small in-cluster HTTP
// API that turns governed build requests into kaniko Jobs (MIG-8 /
// S-314). See package docs and build-service/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rossbrigoli/skquad/build-service/internal/audit"
	"github.com/rossbrigoli/skquad/build-service/internal/authn"
	"github.com/rossbrigoli/skquad/build-service/internal/config"
	"github.com/rossbrigoli/skquad/build-service/internal/cppolicy"
	"github.com/rossbrigoli/skquad/build-service/internal/gitcheck"
	"github.com/rossbrigoli/skquad/build-service/internal/govern"
	"github.com/rossbrigoli/skquad/build-service/internal/httpapi"
	"github.com/rossbrigoli/skquad/build-service/internal/runner"
)

func main() {
	configPath := flag.String("config", "/etc/build-service/config.yaml", "path to service config YAML")
	flag.Parse()

	if err := run(*configPath); err != nil {
		log.Fatalf("build-service: %v", err)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	store, err := authn.LoadStore(cfg.PrincipalsFile)
	if err != nil {
		return err
	}

	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config required: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		return err
	}

	k8sRunner := runner.NewK8sRunner(clientset, cfg)

	var policyLookup cppolicy.Lookup
	if cfg.CPPolicyURL != "" {
		policyLookup = cppolicy.NewClient(
			cfg.CPPolicyURL,
			time.Duration(cfg.CPPolicyTTLSeconds)*time.Second,
			time.Duration(cfg.CPPolicyTimeoutSeconds)*time.Second,
		)
	} else {
		log.Printf("WARNING: cp_policy_url unset — live git-grant-scope check DISABLED (static allowlists only)")
	}

	deps := govern.Deps{
		Policy:                    policyLookup,
		Prober:                    gitcheck.NewHTTPProber(15 * time.Second),
		Active:                    k8sRunner,
		GitHostsAllow:             cfg.GitHostsAllow,
		DestinationsAllow:         cfg.DestinationsAllow,
		MaxConcurrentPerPrincipal: cfg.Limits.MaxConcurrentPerPrincipal,
		MaxPerHourPerPrincipal:    cfg.Limits.MaxPerHourPerPrincipal,
	}
	srv := httpapi.NewServer(httpapi.Deps{
		Store:     store,
		Validator: govern.NewValidator(deps),
		Runner:    k8sRunner,
		Audit:     audit.NewLogger(),
	})

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("build-service listening on %s (namespace=%s)", cfg.Listen, cfg.Namespace)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
