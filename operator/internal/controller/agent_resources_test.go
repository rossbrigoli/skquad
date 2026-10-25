package controller

// S-287: platform-configurable agent pod resources — operator-side
// resolution tests (ConfigMap override → env → built-in default) and
// the ConfigMap watch mapper.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	skquadv1 "github.com/rossbrigoli/skquad/operator/internal/api/v1"
)

func resourcesOverrideMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "skquad-agent-resources", Namespace: "skquad"},
		Data:       data,
	}
}

func resourcesReconciler(t *testing.T, objects ...client.Object) *AgentReconciler {
	t.Helper()
	_, r := newAgentTestEnv(t, objects...)
	r.ResourcesConfigMapName = "skquad-agent-resources"
	r.ResourcesConfigMapNamespace = "skquad"
	return r
}

func requireQty(t *testing.T, q resource.Quantity, want string) {
	t.Helper()
	w := resource.MustParse(want)
	if q.Cmp(w) != 0 {
		t.Fatalf("quantity = %s, want %s", q.String(), want)
	}
}

func TestAgentResourcesDefaultWithoutConfigMap(t *testing.T) {
	r := resourcesReconciler(t)
	res := r.agentResourceRequirements(context.Background())
	requireQty(t, res.Requests[corev1.ResourceCPU], "100m")
	requireQty(t, res.Requests[corev1.ResourceMemory], "128Mi")
	requireQty(t, res.Limits[corev1.ResourceCPU], "500m")
	requireQty(t, res.Limits[corev1.ResourceMemory], "512Mi")
}

func TestAgentResourcesConfigMapOverride(t *testing.T) {
	r := resourcesReconciler(t, resourcesOverrideMap(map[string]string{
		"cpu_request":    "250m",
		"cpu_limit":      "1",
		"memory_request": "256Mi",
		"memory_limit":   "1Gi",
	}))
	res := r.agentResourceRequirements(context.Background())
	requireQty(t, res.Requests[corev1.ResourceCPU], "250m")
	requireQty(t, res.Requests[corev1.ResourceMemory], "256Mi")
	requireQty(t, res.Limits[corev1.ResourceCPU], "1")
	requireQty(t, res.Limits[corev1.ResourceMemory], "1Gi")
}

func TestAgentResourcesInvalidConfigMapValuesFallBack(t *testing.T) {
	r := resourcesReconciler(t, resourcesOverrideMap(map[string]string{
		"cpu_request":    "banana",
		"cpu_limit":      "0",
		"memory_request": "128Mi",
		"memory_limit":   "",
	}))
	res := r.agentResourceRequirements(context.Background())
	requireQty(t, res.Requests[corev1.ResourceCPU], "100m")
	requireQty(t, res.Limits[corev1.ResourceCPU], "500m")
	requireQty(t, res.Requests[corev1.ResourceMemory], "128Mi")
	requireQty(t, res.Limits[corev1.ResourceMemory], "512Mi")
}

func TestAgentResourcesDisabledConfigMapName(t *testing.T) {
	_, r := newAgentTestEnv(t, resourcesOverrideMap(map[string]string{"cpu_request": "999m"}))
	// Name unset → no override lookup at all.
	res := r.agentResourceRequirements(context.Background())
	requireQty(t, res.Requests[corev1.ResourceCPU], "100m")
}

func TestMapAgentResourcesConfigMapEnqueuesAllAgents(t *testing.T) {
	agents := []client.Object{
		&skquadv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "skquad"}},
		&skquadv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a2", Namespace: "skquad"}},
	}
	r := resourcesReconciler(t, agents...)

	reqs := r.mapAgentResourcesConfigMap(context.Background(), resourcesOverrideMap(nil))
	if len(reqs) != 2 {
		t.Fatalf("expected 2 enqueue requests, got %d", len(reqs))
	}

	// A differently-named ConfigMap must not trigger anything.
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "skquad"}}
	if got := r.mapAgentResourcesConfigMap(context.Background(), other); got != nil {
		t.Fatalf("expected nil for unrelated ConfigMap, got %v", got)
	}

	// Feature disabled → nil even for the right name.
	_, disabled := newAgentTestEnv(t)
	if got := disabled.mapAgentResourcesConfigMap(context.Background(), resourcesOverrideMap(nil)); got != nil {
		t.Fatalf("expected nil when feature disabled, got %v", got)
	}
}
