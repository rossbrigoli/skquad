package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func runtimeImages() map[Runtime]string {
	return map[Runtime]string{
		RuntimeCUDA:   "repo/skquad-embedder:1.0-cuda",
		RuntimeVulkan: "repo/skquad-embedder:1.0-vulkan",
		RuntimeCPU:    "repo/skquad-embedder:1.0-cpu",
	}
}

func amdNode(name string, qty string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{}},
	}
	if qty != "" {
		n.Status.Allocatable[corev1.ResourceName("amd.com/gpu")] = resource.MustParse(qty)
	}
	return n
}

func runtimeConfigMap(runtime string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "skquad-embedder-config", Namespace: "skquad"},
		Data:       map[string]string{"runtime": runtime},
	}
}

func newRuntimeReconciler(t *testing.T, objs ...runtime.Object) *EmbedderGPUReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &EmbedderGPUReconciler{Client: client, Cfg: EmbedderGPUConfig{
		Namespace: "skquad", DeploymentName: "skquad-embedder",
		GPUResourceNames:     ParseGPUResourceNames(""),
		RuntimeConfigMapName: "skquad-embedder-config",
		RuntimeConfigMapKey:  "runtime",
		ImageByRuntime:       runtimeImages(),
	}}
}

func getEmbedder(t *testing.T, r *EmbedderGPUReconciler) *appsv1.Deployment {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: "skquad", Name: "skquad-embedder"}, dep); err != nil {
		t.Fatal(err)
	}
	return dep
}

func TestReconcileRuntime_AutoNvidia(t *testing.T) {
	r := newRuntimeReconciler(t, baseEmbedder(), node("nv", "1", nil))
	rt, changed, err := r.ReconcileRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt != RuntimeCUDA {
		t.Fatalf("auto with nvidia -> cuda, got %q", rt)
	}
	if !changed {
		t.Fatal("expected deployment changed")
	}
	dep := getEmbedder(t, r)
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "repo/skquad-embedder:1.0-cuda" {
		t.Fatalf("image not patched to cuda: %s", c.Image)
	}
	if q := c.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; q.Value() != 1 {
		t.Fatalf("nvidia gpu not requested: %v", c.Resources.Requests)
	}
	if hasDRIMount(&dep.Spec.Template.Spec) {
		t.Fatal("cuda must not mount /dev/dri")
	}
}

func TestReconcileRuntime_OverrideCPU(t *testing.T) {
	r := newRuntimeReconciler(t, baseEmbedder(), node("nv", "1", nil), runtimeConfigMap("cpu"))
	rt, _, err := r.ReconcileRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt != RuntimeCPU {
		t.Fatalf("override cpu should win over nvidia detection, got %q", rt)
	}
	dep := getEmbedder(t, r)
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "repo/skquad-embedder:1.0-cpu" {
		t.Fatalf("image not cpu: %s", c.Image)
	}
	if _, ok := c.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; ok {
		t.Fatal("cpu runtime must strip gpu request")
	}
}

func TestReconcileRuntime_AutoAmdVulkan(t *testing.T) {
	r := newRuntimeReconciler(t, baseEmbedder(), amdNode("amd", "2"))
	rt, _, err := r.ReconcileRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt != RuntimeVulkan {
		t.Fatalf("auto with amd -> vulkan, got %q", rt)
	}
	dep := getEmbedder(t, r)
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "repo/skquad-embedder:1.0-vulkan" {
		t.Fatalf("image not vulkan: %s", c.Image)
	}
	if q := c.Resources.Requests[corev1.ResourceName("amd.com/gpu")]; q.Value() != 1 {
		t.Fatalf("amd gpu not requested (1 unit): %v", c.Resources.Requests)
	}
	if !hasDRIMount(&dep.Spec.Template.Spec) {
		t.Fatal("vulkan must mount /dev/dri")
	}
}

func TestReconcileRuntime_ForceCudaNoNvidia_Pends(t *testing.T) {
	// Admin forces cuda but no nvidia node exists -> request nvidia anyway (pend).
	r := newRuntimeReconciler(t, baseEmbedder(), amdNode("amd", "1"), runtimeConfigMap("cuda"))
	rt, _, err := r.ReconcileRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt != RuntimeCUDA {
		t.Fatalf("forced cuda, got %q", rt)
	}
	dep := getEmbedder(t, r)
	c := dep.Spec.Template.Spec.Containers[0]
	if q := c.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; q.Value() != 1 {
		t.Fatalf("forced cuda must request nvidia gpu even without a node: %v", c.Resources.Requests)
	}
}

func TestReconcileRuntime_Idempotent(t *testing.T) {
	r := newRuntimeReconciler(t, baseEmbedder(), node("nv", "1", nil))
	if _, changed, err := r.ReconcileRuntime(context.Background()); err != nil || !changed {
		t.Fatalf("first pass should change: changed=%v err=%v", changed, err)
	}
	if _, changed, err := r.ReconcileRuntime(context.Background()); err != nil || changed {
		t.Fatalf("second pass must be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestApplyDRIMount_Toggle(t *testing.T) {
	pod := &corev1.PodSpec{Containers: []corev1.Container{{Name: "embedder"}}}
	applyDRIMount(pod, true)
	if !hasDRIMount(pod) {
		t.Fatal("expected dri mounted")
	}
	// Toggle off removes it cleanly (no duplicates).
	applyDRIMount(pod, false)
	if hasDRIMount(pod) {
		t.Fatal("expected dri removed")
	}
	// Toggle on twice -> still exactly one volume + one mount.
	applyDRIMount(pod, true)
	applyDRIMount(pod, true)
	driVols := 0
	for _, v := range pod.Volumes {
		if v.Name == "dev-dri" {
			driVols++
		}
	}
	if driVols != 1 {
		t.Fatalf("expected exactly 1 dri volume, got %d", driVols)
	}
}

func TestReconcileRuntime_CUDARuntimeClass(t *testing.T) {
	r := newRuntimeReconciler(t, baseEmbedder(), node("nv", "1", nil))
	r.Cfg.CUDARuntimeClass = "nvidia"
	rt, changed, err := r.ReconcileRuntime(context.Background())
	if err != nil || rt != RuntimeCUDA || !changed {
		t.Fatalf("rt=%q changed=%v err=%v", rt, changed, err)
	}
	dep := getEmbedder(t, r)
	if dep.Spec.Template.Spec.RuntimeClassName == nil || *dep.Spec.Template.Spec.RuntimeClassName != "nvidia" {
		t.Fatalf("cuda runtime must set runtimeClassName=nvidia, got %v", dep.Spec.Template.Spec.RuntimeClassName)
	}
	// Idempotent: second pass reports no change.
	if _, changed, err := r.ReconcileRuntime(context.Background()); err != nil || changed {
		t.Fatalf("expected idempotent, changed=%v err=%v", changed, err)
	}
	// Switch to CPU override: runtime class must be cleared.
	if err := r.Client.Create(context.Background(), runtimeConfigMap("cpu")); err != nil {
		t.Fatal(err)
	}
	rt, changed, err = r.ReconcileRuntime(context.Background())
	if err != nil || rt != RuntimeCPU || !changed {
		t.Fatalf("rt=%q changed=%v err=%v", rt, changed, err)
	}
	dep = getEmbedder(t, r)
	if dep.Spec.Template.Spec.RuntimeClassName != nil {
		t.Fatalf("cpu runtime must clear runtimeClassName, got %q", *dep.Spec.Template.Spec.RuntimeClassName)
	}
}
