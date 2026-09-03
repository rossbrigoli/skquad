package main

import (
	"flag"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

func TestFlagDefaults(t *testing.T) {
	cfg := &config{}
	fs := flag.NewFlagSet("skquad-operator-test", flag.ContinueOnError)
	registerFlags(fs, cfg, newZapOptionsForTest(t))

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse default flags: %v", err)
	}
	if cfg.metricsAddr != ":8080" {
		t.Errorf("metrics-bind-address default = %q, want :8080", cfg.metricsAddr)
	}
	if cfg.probeAddr != ":8081" {
		t.Errorf("health-probe-bind-address default = %q, want :8081", cfg.probeAddr)
	}
	if cfg.leaderElection {
		t.Error("leader-elect must default to false; a single-replica operator should not require a lease")
	}
}

func TestFlagOverrides(t *testing.T) {
	cfg := &config{}
	fs := flag.NewFlagSet("skquad-operator-test", flag.ContinueOnError)
	registerFlags(fs, cfg, newZapOptionsForTest(t))

	args := []string{
		"-metrics-bind-address=127.0.0.1:18080",
		"-health-probe-bind-address=127.0.0.1:18081",
		"-leader-elect=true",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse overrides %v: %v", args, err)
	}
	if cfg.metricsAddr != "127.0.0.1:18080" {
		t.Errorf("metrics-bind-address = %q, want 127.0.0.1:18080", cfg.metricsAddr)
	}
	if cfg.probeAddr != "127.0.0.1:18081" {
		t.Errorf("health-probe-bind-address = %q, want 127.0.0.1:18081", cfg.probeAddr)
	}
	if !cfg.leaderElection {
		t.Error("leader-elect override not applied")
	}
}

// TestZapFlagsRegistered asserts the logging flags are wired to the same flag
// set. If they drift, `-zap-log-level` in the Deployment silently does nothing
// and operators cannot turn up verbosity during an incident.
func TestZapFlagsRegistered(t *testing.T) {
	cfg := &config{}
	fs := flag.NewFlagSet("skquad-operator-test", flag.ContinueOnError)
	registerFlags(fs, cfg, newZapOptionsForTest(t))

	for _, name := range []string{"zap-log-level", "zap-devel"} {
		if fs.Lookup(name) == nil {
			t.Errorf("flag -%s is not registered", name)
		}
	}
	for _, name := range []string{"metrics-bind-address", "health-probe-bind-address", "leader-elect"} {
		if fs.Lookup(name) == nil {
			t.Errorf("flag -%s is not registered", name)
		}
	}
}

func TestManagerOptions(t *testing.T) {
	cfg := config{metricsAddr: "127.0.0.1:9999", probeAddr: "127.0.0.1:9998", leaderElection: true}
	opts := managerOptions(cfg)

	if opts.Scheme != scheme {
		t.Error("manager options did not use the package scheme")
	}
	if got := opts.Metrics.BindAddress; got != cfg.metricsAddr {
		t.Errorf("Metrics.BindAddress = %q, want %q", got, cfg.metricsAddr)
	}
	if opts.HealthProbeBindAddress != cfg.probeAddr {
		t.Errorf("HealthProbeBindAddress = %q, want %q", opts.HealthProbeBindAddress, cfg.probeAddr)
	}
	if !opts.LeaderElection {
		t.Error("LeaderElection not propagated")
	}
	if opts.LeaderElectionID != "skquad-operator.skquad.io" {
		t.Errorf("LeaderElectionID = %q, want skquad-operator.skquad.io", opts.LeaderElectionID)
	}
}

func newZapOptionsForTest(t *testing.T) *zap.Options {
	t.Helper()
	opts := zap.Options{Development: true}
	return &opts
}

// TestSchemeHasRequiredKinds guards the init() registration. A kind missing from
// the scheme does not fail at startup; it fails on the first reconcile with a
// confusing "no kind is registered" error, so assert it here instead.
func TestSchemeHasRequiredKinds(t *testing.T) {
	required := []schema.GroupVersionKind{
		{Group: skquadv1.Group, Version: skquadv1.Version, Kind: "Squad"},
		{Group: skquadv1.Group, Version: skquadv1.Version, Kind: "SquadList"},
		{Group: skquadv1.Group, Version: skquadv1.Version, Kind: "Agent"},
		{Group: skquadv1.Group, Version: skquadv1.Version, Kind: "AgentList"},
		corev1.SchemeGroupVersion.WithKind("Namespace"),
		corev1.SchemeGroupVersion.WithKind("Secret"),
		corev1.SchemeGroupVersion.WithKind("ServiceAccount"),
		corev1.SchemeGroupVersion.WithKind("ResourceQuota"),
		appsv1.SchemeGroupVersion.WithKind("Deployment"),
		networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
		rbacv1.SchemeGroupVersion.WithKind("Role"),
		rbacv1.SchemeGroupVersion.WithKind("RoleBinding"),
	}
	for _, gvk := range required {
		if !scheme.Recognizes(gvk) {
			t.Errorf("manager scheme does not recognize %s; reconciles will fail at runtime", gvk)
		}
	}
}

func TestEnvOrDefault(t *testing.T) {
	const key = "SKQUAD_TEST_ENV_OR_DEFAULT"
	t.Cleanup(func() { _ = os.Unsetenv(key) })

	if err := os.Setenv(key, "set-value"); err != nil {
		t.Fatalf("set env: %v", err)
	}
	if got := envOrDefault(key, "fallback"); got != "set-value" {
		t.Errorf("envOrDefault with set env = %q, want set-value", got)
	}

	// An empty value must fall back: the Helm chart renders optional overrides as
	// empty strings, and an empty ServiceAccount name would break RBAC.
	if err := os.Setenv(key, ""); err != nil {
		t.Fatalf("set env: %v", err)
	}
	if got := envOrDefault(key, "fallback"); got != "fallback" {
		t.Errorf("envOrDefault with empty env = %q, want fallback", got)
	}

	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset env: %v", err)
	}
	if got := envOrDefault(key, "fallback"); got != "fallback" {
		t.Errorf("envOrDefault with unset env = %q, want fallback", got)
	}
}

// TestAPIServerServiceAccountEnvName pins the environment variable name the Helm
// chart sets on the operator Deployment.
func TestAPIServerServiceAccountEnvName(t *testing.T) {
	if envAPIServerServiceAccount != "SKQUAD_API_SERVER_SERVICE_ACCOUNT_NAME" {
		t.Errorf("env var name = %q; the chart and operator must agree", envAPIServerServiceAccount)
	}
}
