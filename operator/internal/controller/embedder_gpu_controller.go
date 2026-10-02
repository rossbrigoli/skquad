// S-212 — GPU-aware reconciliation of the skquad-embedder Deployment.
//
// The embedder (llama.cpp serving Qwen3-Embedding-0.6B on the Vulkan
// backend) runs CPU-only by default. Vendor-neutral by design: the
// operator scans node allocatable for a configurable list of GPU
// resource names (default: nvidia.com/gpu, amd.com/gpu,
// gpu.intel.com/i915) and binds the embedder to whichever resource
// is present — requesting 1 unit of that resource plus a
// requiredDuringScheduling nodeAffinity to the nodes advertising it.
//
// Modes:
//   auto — GPU iff a supported resource exists on some node, else CPU.
//   gpu  — force the GPU spec (explicit operator intent; pods stay
//          Pending until a matching node appears). Uses the first
//          resource from the configured list.
//   cpu  — force CPU-only; strip any GPU artifacts.
//
// The controller is a plain Runnable polling Nodes on an interval
// (nodes change rarely; get/list is the minimal permission set).
// Reconcile is idempotent: no patch when the spec already matches.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultGPUResourceNames is the vendor-neutral detection list:
	// NVIDIA (device plugin), AMD (kfd device plugin), Intel (i915).
	DefaultGPUResourceNames = "nvidia.com/gpu,amd.com/gpu,gpu.intel.com/i915"

	// EmbedderModeAuto picks GPU iff one exists, CPU otherwise.
	EmbedderModeAuto = "auto"
	// EmbedderModeGPU always requests a GPU.
	EmbedderModeGPU = "gpu"
	// EmbedderModeCPU never requests a GPU.
	EmbedderModeCPU = "cpu"
)

// EmbedderGPUConfig configures the reconciler.
type EmbedderGPUConfig struct {
	// Namespace + DeploymentName locate the embedder Deployment
	// (chart-rendered, e.g. skquad / skquad-embedder).
	Namespace      string
	DeploymentName string
	// Mode is auto|gpu|cpu.
	Mode string
	// GPUResourceNames is the ordered list of extended GPU resource
	// names to scan for (vendor-neutral). In auto mode the first
	// resource with a matching node wins; in gpu mode the first entry
	// is forced.
	GPUResourceNames []string
	// GPUNodeLabelKey (optional) restricts GPU-node selection to nodes
	// carrying this label with value "true" (e.g. skquad.io/gpu=true).
	// Empty = any node advertising the resource.
	GPUNodeLabelKey string
}

// ParseGPUResourceNames splits a comma-separated list, dropping blanks;
// empty input falls back to the vendor-neutral default list.
func ParseGPUResourceNames(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		out = strings.Split(DefaultGPUResourceNames, ",")
	}
	return out
}

// EmbedderGPUReconciler patches the embedder Deployment toward the
// GPU/CPU shape implied by cluster state + configured mode.
type EmbedderGPUReconciler struct {
	Client            client.Client
	Cfg               EmbedderGPUConfig
	loggedCPUFallback bool
}

// gpuSelection is the outcome of scanning the cluster: which resource
// to request and on which nodes.
type gpuSelection struct {
	Resource corev1.ResourceName
	Nodes    []string
}

func (s gpuSelection) active() bool { return s.Resource != "" && len(s.Nodes) > 0 }

// selectGPU scans nodes for the configured resource list and returns
// the winning selection (zero value = no GPU available).
func (r *EmbedderGPUReconciler) selectGPU(ctx context.Context) (gpuSelection, error) {
	nodeList := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodeList); err != nil {
		return gpuSelection{}, fmt.Errorf("list nodes: %w", err)
	}
	for _, res := range r.Cfg.GPUResourceNames {
		name := corev1.ResourceName(res)
		matches := make([]string, 0)
		for _, node := range nodeList.Items {
			qty, ok := node.Status.Allocatable[name]
			if !ok || qty.Cmp(resource.MustParse("1")) < 0 {
				continue
			}
			if r.Cfg.GPUNodeLabelKey != "" {
				if v, labeled := node.Labels[r.Cfg.GPUNodeLabelKey]; !labeled || v != "true" {
					continue
				}
			}
			matches = append(matches, node.Name)
		}
		if len(matches) > 0 {
			return gpuSelection{Resource: name, Nodes: matches}, nil
		}
	}
	return gpuSelection{}, nil
}

// ReconcileOnce performs one reconcile pass. Exported for tests and
// for the periodic runnable.
func (r *EmbedderGPUReconciler) ReconcileOnce(ctx context.Context) (changed bool, selected gpuSelection, err error) {
	sel, err := r.selectGPU(ctx)
	if err != nil {
		return false, sel, err
	}

	want := sel
	switch r.Cfg.Mode {
	case EmbedderModeGPU:
		if !sel.active() {
			// Forced GPU with no matching node: request the first
			// configured resource anyway (explicit intent; pods pend).
			want = gpuSelection{Resource: corev1.ResourceName(r.Cfg.GPUResourceNames[0])}
		}
	case EmbedderModeCPU:
		want = gpuSelection{}
	default: // auto
		want = sel
	}

	key := types.NamespacedName{Namespace: r.Cfg.Namespace, Name: r.Cfg.DeploymentName}
	var changedOut bool
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		dep := &appsv1.Deployment{}
		if err := r.Client.Get(ctx, key, dep); err != nil {
			return err
		}
		if applyGPUSpec(dep, want, r.Cfg.GPUNodeLabelKey, r.Cfg.GPUResourceNames) {
			changedOut = true
			return r.Client.Update(ctx, dep)
		}
		return nil
	})
	if err != nil {
		return false, sel, err
	}
	return changedOut, want, nil
}

// applyGPUSpec mutates dep toward the desired GPU shape and reports
// whether anything changed.
func applyGPUSpec(dep *appsv1.Deployment, want gpuSelection, labelKey string, managedResources []string) bool {
	pod := &dep.Spec.Template.Spec
	before, _ := json.Marshal(gpuProjection(pod, managedResources))

	if want.Resource != "" {
		setGPURequest(pod, want.Resource)
		if len(want.Nodes) > 0 {
			ensureNodeAffinity(pod, labelKey, want.Resource, want.Nodes)
		} else {
			stripGPUNodeAffinity(pod, labelKey)
		}
	} else {
		stripAllGPURequests(pod, managedResources)
		stripGPUNodeAffinity(pod, labelKey)
	}

	after, _ := json.Marshal(gpuProjection(pod, managedResources))
	return string(before) != string(after)
}

// gpuProjection extracts only the GPU-relevant parts of the pod spec so
// the changed-detection ignores unrelated fields.
func gpuProjection(pod *corev1.PodSpec, managedResources []string) map[string]any {
	proj := map[string]any{"affinity": pod.Affinity}
	requests := map[string]string{}
	limits := map[string]string{}
	if len(managedResources) == 0 {
		managedResources = ParseGPUResourceNames("")
	}
	for _, c := range pod.Containers {
		for _, res := range managedResources {
			name := corev1.ResourceName(res)
			if q, ok := c.Resources.Requests[name]; ok {
				requests[c.Name+":"+res] = q.String()
			}
			if q, ok := c.Resources.Limits[name]; ok {
				limits[c.Name+":"+res] = q.String()
			}
		}
	}
	proj["gpuRequests"] = requests
	proj["gpuLimits"] = limits
	return proj
}

func setGPURequest(pod *corev1.PodSpec, res corev1.ResourceName) {
	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Requests[res] = resource.MustParse("1")
		c.Resources.Limits[res] = resource.MustParse("1")
	}
}

func stripAllGPURequests(pod *corev1.PodSpec, managedResources []string) {
	if len(managedResources) == 0 {
		managedResources = ParseGPUResourceNames("")
	}
	for i := range pod.Containers {
		c := &pod.Containers[i]
		for _, res := range managedResources {
			delete(c.Resources.Requests, corev1.ResourceName(res))
			delete(c.Resources.Limits, corev1.ResourceName(res))
		}
	}
}

func ensureNodeAffinity(pod *corev1.PodSpec, labelKey string, res corev1.ResourceName, nodes []string) {
	if pod.Affinity == nil {
		pod.Affinity = &corev1.Affinity{}
	}
	if pod.Affinity.NodeAffinity == nil {
		pod.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	terms := []corev1.NodeSelectorTerm{}
	if pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		// Keep any non-GPU terms the chart authored.
		for _, t := range pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if !termReferencesGPU(t, labelKey) {
				terms = append(terms, t)
			}
		}
	}
	gpuTerm := corev1.NodeSelectorTerm{}
	if labelKey != "" {
		gpuTerm.MatchExpressions = append(gpuTerm.MatchExpressions, corev1.NodeSelectorRequirement{
			Key:      labelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"true"},
		})
	} else {
		gpuTerm.MatchExpressions = append(gpuTerm.MatchExpressions, corev1.NodeSelectorRequirement{
			Key:      "kubernetes.io/hostname",
			Operator: corev1.NodeSelectorOpIn,
			Values:   nodes,
		})
	}
	terms = append(terms, gpuTerm)
	pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
		NodeSelectorTerms: terms,
	}
}

func stripGPUNodeAffinity(pod *corev1.PodSpec, labelKey string) {
	if pod.Affinity == nil || pod.Affinity.NodeAffinity == nil ||
		pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return
	}
	remaining := []corev1.NodeSelectorTerm{}
	for _, t := range pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if !termReferencesGPU(t, labelKey) {
			remaining = append(remaining, t)
		}
	}
	if len(remaining) == 0 {
		pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
	} else {
		pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = remaining
	}
}

// termReferencesGPU reports whether a selector term was authored by
// this controller (hostname-in list or the GPU node label).
func termReferencesGPU(t corev1.NodeSelectorTerm, labelKey string) bool {
	for _, expr := range t.MatchExpressions {
		if labelKey != "" && expr.Key == labelKey {
			return true
		}
		if labelKey == "" && expr.Key == "kubernetes.io/hostname" && expr.Operator == corev1.NodeSelectorOpIn {
			return true
		}
	}
	return false
}

// Start implements manager.Runnable: reconcile immediately, then on the
// configured interval. Missing embedder Deployment is not fatal (the
// chart may not have created it yet); it is retried next tick.
func (r *EmbedderGPUReconciler) Start(ctx context.Context) error {
	intervalSeconds := 60
	if v := envOrDefault("SKQUAD_EMBEDDER_GPU_RECONCILE_INTERVAL_SECONDS", "60"); true {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			intervalSeconds = parsed
		}
	}
	interval := time.Duration(intervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		changed, sel, err := r.ReconcileOnce(ctx)
		switch {
		case err != nil && !apierrors.IsNotFound(err):
			ctrl.Log.Error(err, "embedder GPU reconcile failed", "mode", r.Cfg.Mode)
		case err != nil:
			ctrl.Log.Info("embedder Deployment not found yet; will retry", "deployment", r.Cfg.DeploymentName)
		case changed:
			ctrl.Log.Info("embedder GPU spec reconciled",
				"resource", sel.Resource, "nodes", sel.Nodes, "mode", r.Cfg.Mode,
				"deployment", r.Cfg.Namespace+"/"+r.Cfg.DeploymentName)
		case r.Cfg.Mode == EmbedderModeAuto && !sel.active() && !r.loggedCPUFallback:
			ctrl.Log.Info("embedder GPU auto-detect found no GPU resources; using CPU fallback",
				"resources", r.Cfg.GPUResourceNames,
				"deployment", r.Cfg.Namespace+"/"+r.Cfg.DeploymentName)
			r.loggedCPUFallback = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
