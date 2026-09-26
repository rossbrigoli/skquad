// Package main is the entrypoint for the skquad operator.
//
// It reconciles the Squad and Agent custom resources: squad namespaces + base
// resources, agent Deployments (scale-to-zero), secrets, and network policies.
// See docs/deployment-operator.md.
package main

import (
	"context"
	"flag"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
	"github.com/rossbrigoli/skquad/operator/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(skquadv1.AddToScheme(scheme))
}

// leaderElectionID identifies this operator in the leader election lease. It is
// a contract: a second manager using a different id would not contend for the
// same lease, and both would reconcile the same custom resources.
const leaderElectionID = "skquad-operator.skquad.io"

// config holds the manager settings that come from the command line.
type config struct {
	metricsAddr    string
	probeAddr      string
	leaderElection bool
}

// registerFlags binds the manager's flags to a given flag set. Production uses
// the process flag set; tests pass an isolated one so registration can be
// asserted without polluting global state.
func registerFlags(fs *flag.FlagSet, cfg *config, zapOpts *zap.Options) {
	fs.StringVar(&cfg.metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	fs.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&cfg.leaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	zapOpts.BindFlags(fs)
}

// managerOptions translates parsed configuration into controller-runtime manager
// options.
func managerOptions(cfg config) ctrl.Options {
	return ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: cfg.metricsAddr,
		},
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElection,
		LeaderElectionID:       leaderElectionID,
		// These resources are only ever touched by fixed, known names, which
		// RBAC scopes via resourceNames. resourceNames does not apply to
		// list/watch, so caching them would demand cluster-wide list
		// permissions; the manager client reads them directly instead.
		//
		// Secrets MUST stay in this list: RBAC withholds list/watch on
		// secrets by design (ADR-0009), so a cached Secret read spins up a
		// cluster-scoped informer that can never sync and blocks the
		// reconcile worker forever (incident 2026-09-24: agent-139e6315
		// never received its credential mounts because the first
		// evaluateAgentReadiness Secret Get hung the single agent-controller
		// worker). Keep this in sync with the resourceNames-scoped types in
		// charts/skquad/templates/operator-rbac.yaml.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{
					&corev1.Secret{},
					&corev1.ServiceAccount{},
					&rbacv1.Role{},
					&rbacv1.RoleBinding{},
					&networkingv1.NetworkPolicy{},
					&corev1.ResourceQuota{},
				},
			},
		},
	}
}

func main() {
	cfg := &config{}
	opts := zap.Options{Development: true}
	registerFlags(flag.CommandLine, cfg, &opts)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	managerOpts := managerOptions(*cfg)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOpts)
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.SquadReconciler{
		Client:                      mgr.GetClient(),
		Scheme:                      mgr.GetScheme(),
		APIServerServiceAccountName: envOrDefault(envAPIServerServiceAccount, "skquad-api-server"),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create Squad controller")
		os.Exit(1)
	}
	if err := (&controller.AgentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("skquad-agent-controller"),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create Agent controller")
		os.Exit(1)
	}

	// S-139: orphaned workspace PVC GC. Runs once at startup and then on
	// a fixed interval inside the manager (leader-elected: only the leader
	// sweeps). Scoped by construction — labeled workspace PVCs inside
	// squad namespaces only (see internal/controller/workspace_gc.go).
	gc := &controller.OrphanPVCGC{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("skquad-workspace-gc"),
	}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		interval := time.Duration(envIntOrDefault("SKQUAD_ORPHAN_PVC_SWEEP_INTERVAL_MINUTES", 15)) * time.Minute
		sweep := func() {
			report, err := gc.SweepOrphanWorkspacePVCs(ctx)
			if err != nil {
				ctrl.Log.Error(err, "orphan workspace PVC sweep failed")
				return
			}
			if report.Scanned > 0 || report.Cleaned > 0 || report.Retained > 0 {
				ctrl.Log.Info("orphan workspace PVC sweep complete",
					"scanned", report.Scanned,
					"cleaned", report.Cleaned,
					"retained", report.Retained,
					"skippedInUse", report.SkippedInUse)
			}
		}
		sweep()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				sweep()
			}
		}
	})); err != nil {
		ctrl.Log.Error(err, "unable to add orphan workspace PVC GC runnable")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting skquad operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "manager exited")
		os.Exit(1)
	}
}

// envAPIServerServiceAccount overrides the ServiceAccount the Squad reconciler
// grants to the API server inside squad namespaces.
const envAPIServerServiceAccount = "SKQUAD_API_SERVER_SERVICE_ACCOUNT_NAME"

func envOrDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}
