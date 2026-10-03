// S-212 / ADR-0013 — Embedder runtime selection (cuda | vulkan | cpu).
//
// This layer decides WHICH embedder image variant runs and WHAT pod
// shape (GPU resource, toleration, affinity, /dev/dri mount) it needs,
// from two inputs:
//
//   1. admin override  — a platform admin's explicit choice, delivered
//      via the skquad-embedder-config ConfigMap (control-plane writes
//      it on PUT /admin/settings; the operator has no DB access).
//   2. auto-detection  — a scan of node .status.allocatable for GPU
//      resources, in vendor priority: NVIDIA -> cuda, AMD/Intel ->
//      vulkan, none -> cpu.
//
// Precedence: an explicit admin runtime (cuda/vulkan/cpu) wins; `auto`
// (or an unset/blank override) defers to detection.
//
// The runtime then maps to a concrete shape via runtimeShape(): which
// image tag variant to use, which extended GPU resource to request (if
// any), and whether /dev/dri must be mounted (vulkan).

package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Runtime is the embedder compute backend selection.
type Runtime string

const (
	// RuntimeAuto defers to node auto-detection.
	RuntimeAuto Runtime = "auto"
	// RuntimeCUDA uses the llama.cpp CUDA image (NVIDIA GPUs).
	RuntimeCUDA Runtime = "cuda"
	// RuntimeVulkan uses the llama.cpp Vulkan image (AMD/Intel GPUs,
	// or any GPU exposing a DRM render node).
	RuntimeVulkan Runtime = "vulkan"
	// RuntimeCPU uses the llama.cpp CPU image (no GPU).
	RuntimeCPU Runtime = "cpu"
)

// ValidRuntime reports whether s is an accepted runtime value.
func ValidRuntime(s string) bool {
	switch Runtime(strings.TrimSpace(strings.ToLower(s))) {
	case RuntimeAuto, RuntimeCUDA, RuntimeVulkan, RuntimeCPU:
		return true
	default:
		return false
	}
}

// normalizeRuntime lowercases/trims and maps anything unrecognized to
// RuntimeAuto so a bad value never wedges the reconciler.
func normalizeRuntime(s string) Runtime {
	r := Runtime(strings.TrimSpace(strings.ToLower(s)))
	if ValidRuntime(string(r)) {
		return r
	}
	return RuntimeAuto
}

// vendorResourceNames groups the vendor-neutral GPU resource list by
// which runtime each resource implies. Detection walks NVIDIA first
// (cuda), then AMD/Intel (vulkan).
type vendorResources struct {
	nvidia []string
	amd    []string
	intel  []string
}

// classify maps the configured GPU resource names to vendor buckets.
// Unknown resource names are treated as vulkan-capable (vendor-neutral
// DRM path) so a custom resource still accelerates via Vulkan.
func classifyGPUResources(names []string) vendorResources {
	vr := vendorResources{}
	for _, n := range names {
		lower := strings.ToLower(n)
		switch {
		case strings.Contains(lower, "nvidia"):
			vr.nvidia = append(vr.nvidia, n)
		case strings.Contains(lower, "amd") || strings.Contains(lower, "kfd"):
			vr.amd = append(vr.amd, n)
		case strings.Contains(lower, "intel") || strings.Contains(lower, "i915"):
			vr.intel = append(vr.intel, n)
		default:
			vr.amd = append(vr.amd, n) // vendor-neutral -> vulkan path
		}
	}
	return vr
}

// detectRuntime scans nodes for GPU resources and returns the runtime
// the cluster hardware supports, plus the winning resource name and the
// matching node names (empty resource => cpu). NVIDIA wins over
// AMD/Intel (cuda is the preferred accelerated path).
func detectRuntime(nodes []corev1.Node, names []string) (Runtime, corev1.ResourceName, []string) {
	vr := classifyGPUResources(names)
	// NVIDIA first -> cuda.
	if res, matched := firstResourceOnNodes(nodes, vr.nvidia); res != "" {
		return RuntimeCUDA, res, matched
	}
	// AMD + Intel next -> vulkan.
	amdIntel := append(append([]string{}, vr.amd...), vr.intel...)
	if res, matched := firstResourceOnNodes(nodes, amdIntel); res != "" {
		return RuntimeVulkan, res, matched
	}
	return RuntimeCPU, "", nil
}

// firstResourceOnNodes returns the first resource name (in the given
// order) that at least one node advertises with allocatable >= 1, plus
// the nodes that advertise it.
func firstResourceOnNodes(nodes []corev1.Node, names []string) (corev1.ResourceName, []string) {
	one := resource.MustParse("1")
	for _, name := range names {
		rn := corev1.ResourceName(name)
		matched := make([]string, 0)
		for _, node := range nodes {
			if qty, ok := node.Status.Allocatable[rn]; ok && qty.Cmp(one) >= 0 {
				matched = append(matched, node.Name)
			}
		}
		if len(matched) > 0 {
			return rn, matched
		}
	}
	return "", nil
}

// runtimeShape is the concrete pod shape a runtime requires.
type runtimeShape struct {
	// Runtime is the resolved runtime (never auto).
	Runtime Runtime
	// GPUResource is the extended resource to request ("" for cpu).
	GPUResource corev1.ResourceName
	// Nodes are the candidate nodes for affinity (empty => no affinity).
	Nodes []string
	// NeedsDRI is true when /dev/dri must be mounted (vulkan).
	NeedsDRI bool
	// RuntimeClass is the Kubernetes RuntimeClass name for the pod
	// ("nvidia" for the CUDA runtime; "" = cluster default). Required
	// on k3s for driver injection into NVIDIA GPU containers.
	RuntimeClass string
}

// shapeForRuntime builds the pod shape for a resolved runtime given the
// detected resource + nodes. For a forced runtime whose GPU is absent,
// GPUResource stays empty and Nodes empty (the caller decides pend vs
// cpu fallback per ADR-0013 open-question #2).
func shapeForRuntime(rt Runtime, res corev1.ResourceName, nodes []string) runtimeShape {
	switch rt {
	case RuntimeCUDA:
		return runtimeShape{Runtime: RuntimeCUDA, GPUResource: res, Nodes: nodes, NeedsDRI: false}
	case RuntimeVulkan:
		return runtimeShape{Runtime: RuntimeVulkan, GPUResource: res, Nodes: nodes, NeedsDRI: true}
	default:
		return runtimeShape{Runtime: RuntimeCPU}
	}
}
