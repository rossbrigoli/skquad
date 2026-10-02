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

func baseEmbedder() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "skquad-embedder", Namespace: "skquad"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "embedder",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1536Mi")},
						},
					}},
				},
			},
		},
	}
}

func node(name string, gpus string, labels map[string]string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{}},
	}
	if gpus != "" {
		n.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")] = resource.MustParse(gpus)
	}
	return n
}

func newReconciler(t *testing.T, mode string, labelKey string, objs ...runtime.Object) *EmbedderGPUReconciler {
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
		Namespace: "skquad", DeploymentName: "skquad-embedder", Mode: mode, GPUNodeLabelKey: labelKey,
		GPUResourceNames: ParseGPUResourceNames(""),
	}}
}

func getDep(t *testing.T, r *EmbedderGPUReconciler) *appsv1.Deployment {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: "skquad", Name: "skquad-embedder"}, dep); err != nil {
		t.Fatal(err)
	}
	return dep
}

func TestAutoModeWithGPUNodePatchesGPU(t *testing.T) {
	r := newReconciler(t, EmbedderModeAuto, "",
		baseEmbedder(),
		node("cpu1", "", nil),
		node("gpu1", "1", nil),
	)
	changed, sel, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sel.Resource != "nvidia.com/gpu" || len(sel.Nodes) != 1 || sel.Nodes[0] != "gpu1" {
		t.Fatalf("changed=%v sel=%+v", changed, sel)
	}
	dep := getDep(t, r)
	c := dep.Spec.Template.Spec.Containers[0]
	if q := c.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; q.Value() != 1 {
		t.Fatalf("gpu request = %v", q.String())
	}
	if q := c.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]; q.Value() != 1 {
		t.Fatalf("gpu limit = %v", q.String())
	}
	aff := dep.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(aff.NodeSelectorTerms) != 1 {
		t.Fatalf("terms = %d", len(aff.NodeSelectorTerms))
	}
	expr := aff.NodeSelectorTerms[0].MatchExpressions[0]
	if expr.Key != "kubernetes.io/hostname" || len(expr.Values) != 1 || expr.Values[0] != "gpu1" {
		t.Fatalf("expr = %+v", expr)
	}
}

func TestAutoModeNoGPUStaysCPU(t *testing.T) {
	r := newReconciler(t, EmbedderModeAuto, "", baseEmbedder(), node("cpu1", "", nil))
	changed, sel, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed || sel.active() {
		t.Fatalf("changed=%v sel=%+v, want no active selection", changed, sel)
	}
	dep := getDep(t, r)
	if _, ok := dep.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; ok {
		t.Fatal("unexpected gpu request in cpu-only cluster")
	}
	if dep.Spec.Template.Spec.Affinity != nil {
		t.Fatal("unexpected affinity in cpu-only cluster")
	}
}

func TestAutoModeGPUAppearsThenDisappears(t *testing.T) {
	r := newReconciler(t, EmbedderModeAuto, "", baseEmbedder(), node("gpu1", "1", nil))
	if _, _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// GPU node goes away → back to CPU.
	r2 := newReconciler(t, EmbedderModeAuto, "", getDep(t, r), node("cpu1", "", nil))
	changed, sel, err := r2.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sel.active() {
		t.Fatalf("changed=%v sel=%+v", changed, sel)
	}
	dep := getDep(t, r2)
	if _, ok := dep.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; ok {
		t.Fatal("gpu request survived node removal")
	}
	if aff := dep.Spec.Template.Spec.Affinity; aff != nil &&
		aff.NodeAffinity != nil &&
		aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Fatal("gpu affinity survived node removal")
	}
}

func TestCPUModeStripsGPU(t *testing.T) {
	r := newReconciler(t, EmbedderModeCPU, "", baseEmbedder(), node("gpu1", "1", nil))
	changed, _, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("cpu mode on a cpu-shaped deployment should not change anything")
	}
	// Now flip a GPU-shaped deployment under cpu mode.
	rGPU := newReconciler(t, EmbedderModeGPU, "", baseEmbedder(), node("gpu1", "1", nil))
	if _, _, err := rGPU.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rCPU := newReconciler(t, EmbedderModeCPU, "", getDep(t, rGPU), node("gpu1", "1", nil))
	changed, _, err = rCPU.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("cpu mode must strip the gpu spec")
	}
	dep := getDep(t, rCPU)
	if _, ok := dep.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; ok {
		t.Fatal("gpu request survived cpu mode")
	}
	if _, ok := dep.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]; ok {
		t.Fatal("gpu limit survived cpu mode")
	}
}

func TestGPUModeForcesGPUWithoutGPUNode(t *testing.T) {
	r := newReconciler(t, EmbedderModeGPU, "", baseEmbedder(), node("cpu1", "", nil))
	changed, sel, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sel.active() {
		t.Fatalf("changed=%v sel=%+v", changed, sel)
	}
	dep := getDep(t, r)
	if q := dep.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; q.Value() != 1 {
		t.Fatal("gpu mode must request a gpu even with none present")
	}
}

func TestLabelKeyMode(t *testing.T) {
	r := newReconciler(t, EmbedderModeAuto, "skquad.io/gpu",
		baseEmbedder(),
		node("gpu1", "1", map[string]string{"skquad.io/gpu": "true"}),
		node("gpu2", "1", map[string]string{"skquad.io/gpu": "false"}),
	)
	changed, sel, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sel.Resource != "nvidia.com/gpu" || len(sel.Nodes) != 1 || sel.Nodes[0] != "gpu1" {
		t.Fatalf("changed=%v sel=%+v", changed, sel)
	}
	dep := getDep(t, r)
	aff := dep.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	expr := aff.NodeSelectorTerms[0].MatchExpressions[0]
	if expr.Key != "skquad.io/gpu" || expr.Values[0] != "true" {
		t.Fatalf("expr = %+v", expr)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	r := newReconciler(t, EmbedderModeAuto, "", baseEmbedder(), node("gpu1", "1", nil))
	if _, _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed, _, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second pass must be a no-op")
	}
}

func TestMissingDeploymentReportsNotFound(t *testing.T) {
	r := newReconciler(t, EmbedderModeAuto, "", node("gpu1", "1", nil))
	_, _, err := r.ReconcileOnce(context.Background())
	if err == nil {
		t.Fatal("want not-found error")
	}
}

func TestVendorNeutralAMDSelection(t *testing.T) {
	// amd1 advertises amd.com/gpu; auto mode must select it.
	amdNode := node("amd1", "", nil)
	amdNode.Status.Allocatable[corev1.ResourceName("amd.com/gpu")] = resource.MustParse("1")
	r2 := newReconciler(t, EmbedderModeAuto, "", baseEmbedder(), node("cpu1", "", nil), amdNode)
	changed, sel, err := r2.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sel.Resource != "amd.com/gpu" || sel.Nodes[0] != "amd1" {
		t.Fatalf("changed=%v sel=%+v", changed, sel)
	}
	dep := getDep(t, r2)
	if q := dep.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceName("amd.com/gpu")]; q.Value() != 1 {
		t.Fatal("amd.com/gpu request missing")
	}
}

func TestCPUModeStripsAllVendors(t *testing.T) {
	// Start from an AMD-shaped deployment, flip to cpu mode.
	amdNode := node("amd1", "", nil)
	amdNode.Status.Allocatable[corev1.ResourceName("amd.com/gpu")] = resource.MustParse("1")
	rGPU := newReconciler(t, EmbedderModeGPU, "", baseEmbedder(), amdNode)
	if _, _, err := rGPU.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// gpu mode forces the FIRST configured resource (nvidia) — verify,
	// then strip under cpu mode.
	rCPU := newReconciler(t, EmbedderModeCPU, "", getDep(t, rGPU), amdNode)
	changed, _, err := rCPU.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("cpu mode must strip the gpu spec")
	}
	dep := getDep(t, rCPU)
	c := dep.Spec.Template.Spec.Containers[0]
	for _, res := range ParseGPUResourceNames("") {
		if _, ok := c.Resources.Requests[corev1.ResourceName(res)]; ok {
			t.Fatalf("%s survived cpu mode", res)
		}
	}
}

func TestCustomResourceList(t *testing.T) {
	fpga := node("fpga1", "", nil)
	fpga.Status.Allocatable[corev1.ResourceName("example.com/fpga")] = resource.MustParse("2")
	r2 := newReconciler(t, EmbedderModeAuto, "", baseEmbedder(), fpga)
	r2.Cfg.GPUResourceNames = []string{"example.com/fpga"}
	changed, sel, err := r2.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sel.Resource != "example.com/fpga" {
		t.Fatalf("changed=%v sel=%+v", changed, sel)
	}
}

func TestCPUModeStripsConfiguredCustomResource(t *testing.T) {
	fpga := node("fpga1", "", nil)
	fpga.Status.Allocatable[corev1.ResourceName("example.com/fpga")] = resource.MustParse("2")

	rGPU := newReconciler(t, EmbedderModeAuto, "", baseEmbedder(), fpga)
	rGPU.Cfg.GPUResourceNames = []string{"example.com/fpga"}
	if _, _, err := rGPU.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	rCPU := newReconciler(t, EmbedderModeCPU, "", getDep(t, rGPU), fpga)
	rCPU.Cfg.GPUResourceNames = []string{"example.com/fpga"}
	changed, _, err := rCPU.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("cpu mode must strip configured custom resources")
	}
	dep := getDep(t, rCPU)
	c := dep.Spec.Template.Spec.Containers[0]
	if _, ok := c.Resources.Requests[corev1.ResourceName("example.com/fpga")]; ok {
		t.Fatal("custom gpu request survived cpu mode")
	}
	if _, ok := c.Resources.Limits[corev1.ResourceName("example.com/fpga")]; ok {
		t.Fatal("custom gpu limit survived cpu mode")
	}
}

// S-212 fix: a pod requesting the GPU resource must tolerate the
// GPU-node taint (nvidia.com/gpu=true:NoSchedule) or it can never
// schedule on the very node it was pinned to.
func TestGPURequestAddsToleration(t *testing.T) {
	r := newReconciler(t, EmbedderModeAuto, "",
		baseEmbedder(),
		node("gpu1", "1", nil),
	)
	if _, _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	dep := getDep(t, r)
	found := false
	for _, tol := range dep.Spec.Template.Spec.Tolerations {
		if tol.Key == "nvidia.com/gpu" {
			found = true
			if tol.Operator != corev1.TolerationOpExists {
				t.Fatalf("toleration operator = %v, want Exists", tol.Operator)
			}
			if tol.Effect != "" {
				t.Fatalf("toleration effect = %v, want empty (all effects)", tol.Effect)
			}
		}
	}
	if !found {
		t.Fatalf("missing gpu toleration: %+v", dep.Spec.Template.Spec.Tolerations)
	}
}

// Forced gpu mode (no node yet) must also carry the toleration so the
// pod schedules the moment a matching tainted node appears.
func TestGPUModeForcedAddsToleration(t *testing.T) {
	r := newReconciler(t, EmbedderModeGPU, "", baseEmbedder(), node("cpu1", "", nil))
	if _, _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	dep := getDep(t, r)
	found := false
	for _, tol := range dep.Spec.Template.Spec.Tolerations {
		if tol.Key == "nvidia.com/gpu" && tol.Operator == corev1.TolerationOpExists {
			found = true
		}
	}
	if !found {
		t.Fatalf("forced-gpu pod missing toleration: %+v", dep.Spec.Template.Spec.Tolerations)
	}
}

// CPU fallback strips the controller-added GPU toleration but keeps
// unrelated tolerations (node lifecycle, chart-specific).
func TestCPUFallbackStripsGPUTolerationKeepsOthers(t *testing.T) {
	base := baseEmbedder()
	base.Spec.Template.Spec.Tolerations = []corev1.Toleration{
		{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
		{Key: "workload", Value: "special", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule},
	}
	r := newReconciler(t, EmbedderModeAuto, "", base, node("gpu1", "1", nil))
	if _, _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	mid := getDep(t, r)
	gpuTols := 0
	for _, tol := range mid.Spec.Template.Spec.Tolerations {
		if tol.Key == "nvidia.com/gpu" {
			gpuTols++
		}
	}
	if gpuTols != 1 {
		t.Fatalf("after gpu bind: gpu tolerations = %d, want 1", gpuTols)
	}
	// GPU node disappears → CPU fallback strips the gpu toleration only.
	r2 := newReconciler(t, EmbedderModeAuto, "", mid, node("cpu1", "", nil))
	if _, _, err := r2.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	dep := getDep(t, r2)
	for _, tol := range dep.Spec.Template.Spec.Tolerations {
		if tol.Key == "nvidia.com/gpu" {
			t.Fatalf("gpu toleration survived cpu fallback: %+v", dep.Spec.Template.Spec.Tolerations)
		}
	}
	if len(dep.Spec.Template.Spec.Tolerations) != 2 {
		t.Fatalf("non-gpu tolerations not preserved: %+v", dep.Spec.Template.Spec.Tolerations)
	}
}

// A chart-authored toleration for the same GPU key must not be
// duplicated on repeated reconciles.
func TestChartGPUTolerationNotDuplicated(t *testing.T) {
	base := baseEmbedder()
	base.Spec.Template.Spec.Tolerations = []corev1.Toleration{
		{Key: "nvidia.com/gpu", Value: "true", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule},
	}
	r := newReconciler(t, EmbedderModeAuto, "", base, node("gpu1", "1", nil))
	if _, _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	dep := getDep(t, r)
	gpuTols := 0
	for _, tol := range dep.Spec.Template.Spec.Tolerations {
		if tol.Key == "nvidia.com/gpu" {
			gpuTols++
		}
	}
	if gpuTols != 1 {
		t.Fatalf("gpu tolerations = %d, want 1 (chart's own kept, no duplicate)", gpuTols)
	}
	// Second reconcile must be a no-op (idempotence with tolerations).
	r2 := newReconciler(t, EmbedderModeAuto, "", dep, node("gpu1", "1", nil))
	changed, _, err := r2.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second reconcile changed a already-converged spec")
	}
}
