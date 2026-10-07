package controller

// TG-9 slice B tests: the netpol-guard blocking init-container that gates
// agent workload startup on confirmed egress enforcement (startup-race
// mitigation for drill finding 1).

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

func guardTestAgent() *skquadv1.Agent {
	return &skquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-guard-test", Namespace: "skquad-system"},
		Spec: skquadv1.AgentSpec{
			AgentID:       "aaaaaaaa-1111-2222-3333-444444444444",
			SquadID:       "squad-guard-test",
			Role:          "worker",
			IdleTimeout:   "300s",
			DesiredActive: true,
		},
	}
}

func guardEnvLookup(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func assertGuardEnv(t *testing.T, env []corev1.EnvVar, name, want string) {
	t.Helper()
	got, ok := guardEnvLookup(env, name)
	if !ok || got != want {
		t.Fatalf("guard env %s = %q (present=%v), want %q", name, got, ok, want)
	}
}

func applyGuardSpec(t *testing.T) *appsv1.Deployment {
	t.Helper()
	agent := guardTestAgent()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploymentNameFor(agent), Namespace: agent.Namespace},
	}
	r := &AgentReconciler{}
	r.applyAgentDeploymentSpec(agent, deployment)
	return deployment
}

func TestNetpolGuardDefaultEnabled(t *testing.T) {
	// Deterministic defaults regardless of ambient env on the test host.
	for _, k := range []string{envNetpolGuardEnabled, envNetpolGuardCanaryURL, envNetpolGuardProbeIntervalMS, envNetpolGuardMaxWaitSeconds, envNetpolGuardRequiredProbes, envNetpolGuardImage} {
		t.Setenv(k, "") // envOrDefault treats "" as unset
	}
	deployment := applyGuardSpec(t)
	initContainers := deployment.Spec.Template.Spec.InitContainers
	if len(initContainers) != 1 {
		t.Fatalf("init containers = %d, want 1 (guard defaults ON)", len(initContainers))
	}
	c := initContainers[0]
	if c.Name != netpolGuardContainerName {
		t.Fatalf("init container name = %q, want %q", c.Name, netpolGuardContainerName)
	}
	if c.Image != defaultNetpolGuardImage {
		t.Fatalf("guard image = %q, want %q", c.Image, defaultNetpolGuardImage)
	}
	if len(c.Command) != 3 || c.Command[0] != "/bin/sh" || c.Command[1] != "-c" || !strings.Contains(c.Command[2], "netpol-guard:") {
		t.Fatalf("guard command = %#v, want /bin/sh -c <guard script>", c.Command)
	}
	// Default knob values must be present in the container env.
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_CANARY_URL", "https://example.com")
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_PROBE_INTERVAL_SECONDS", "0.250")
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_MAX_WAIT_SECONDS", "30")
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_REQUIRED_BLOCKED_PROBES", "3")
	if c.SecurityContext == nil || c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot {
		t.Fatal("guard container must run as non-root")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("guard container must have a read-only root filesystem")
	}
	// curlimages/curl needs a numeric runAsUser pin (non-numeric curl_user
	// breaks runAsNonRoot verification — live TG-9 drill finding).
	if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 1000 {
		t.Fatal("guard container must pin runAsUser=1000 (curl_user numeric uid)")
	}
	if c.SecurityContext.RunAsGroup == nil || *c.SecurityContext.RunAsGroup != 1000 {
		t.Fatal("guard container must pin runAsGroup=1000")
	}
	// The guard must sit ahead of the agent container (init before app).
	if len(deployment.Spec.Template.Spec.Containers) != 1 || deployment.Spec.Template.Spec.Containers[0].Name != agentContainerName {
		t.Fatal("agent container missing after guard wiring")
	}
}

func TestNetpolGuardDisabled(t *testing.T) {
	t.Setenv(envNetpolGuardEnabled, "false")
	deployment := applyGuardSpec(t)
	if len(deployment.Spec.Template.Spec.InitContainers) != 0 {
		t.Fatalf("guard must be absent when disabled, got %d init container(s)", len(deployment.Spec.Template.Spec.InitContainers))
	}
	// Explicit clearing: a previously-guarded deployment gets the guard
	// removed on the next apply (CreateOrUpdate reuses the object).
	deployment.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: netpolGuardContainerName}}
	r := &AgentReconciler{}
	r.applyAgentDeploymentSpec(guardTestAgent(), deployment)
	if len(deployment.Spec.Template.Spec.InitContainers) != 0 {
		t.Fatal("disabling the guard must clear a previously-installed init container")
	}
}

func TestNetpolGuardKnobOverrides(t *testing.T) {
	t.Setenv(envNetpolGuardCanaryURL, "https://canary.invalid")
	t.Setenv(envNetpolGuardProbeIntervalMS, "500")
	t.Setenv(envNetpolGuardMaxWaitSeconds, "60")
	t.Setenv(envNetpolGuardRequiredProbes, "5")
	t.Setenv(envNetpolGuardImage, "curlimages/curl:8.10.1")
	c := netpolGuardContainer()
	if c == nil {
		t.Fatal("guard container expected")
	}
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_CANARY_URL", "https://canary.invalid")
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_PROBE_INTERVAL_SECONDS", "0.500")
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_MAX_WAIT_SECONDS", "60")
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_REQUIRED_BLOCKED_PROBES", "5")
}

func TestNetpolGuardInvalidKnobsFallBack(t *testing.T) {
	// Garbage values must restore defaults, never disable or widen.
	t.Setenv(envNetpolGuardProbeIntervalMS, "-5")
	t.Setenv(envNetpolGuardMaxWaitSeconds, "abc")
	t.Setenv(envNetpolGuardRequiredProbes, "0")
	c := netpolGuardContainer()
	if c == nil {
		t.Fatal("guard container expected even with invalid knobs")
	}
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_PROBE_INTERVAL_SECONDS", "0.250")
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_MAX_WAIT_SECONDS", "30")
	assertGuardEnv(t, c.Env, "NETPOL_GUARD_REQUIRED_BLOCKED_PROBES", "3")
}

func TestNetpolGuardScriptSemantics(t *testing.T) {
	// Contract checks on the embedded probe script (deliverable 3).
	if !strings.Contains(netpolGuardScript, "--max-time 2") {
		t.Fatal("probe must cap each attempt at 2s")
	}
	if !strings.Contains(netpolGuardScript, "blocked=0") {
		t.Fatal("a reached canary must reset the consecutive counter")
	}
	if !strings.Contains(netpolGuardScript, "FAIL-CLOSED") {
		t.Fatal("script must fail closed on max-wait expiry")
	}
	// The script must NOT use curl -f/--fail: an HTTP 4xx/5xx response
	// still means the internet was REACHED and must reset the counter.
	if strings.Contains(netpolGuardScript, "curl -f ") || strings.Contains(netpolGuardScript, "--fail") {
		t.Fatal("guard must not use curl --fail: HTTP error responses count as REACHED")
	}
}
