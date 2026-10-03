package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func gpuNode(name string, alloc map[string]string) corev1.Node {
	n := corev1.Node{}
	n.Name = name
	n.Status.Allocatable = corev1.ResourceList{}
	for k, v := range alloc {
		n.Status.Allocatable[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return n
}

func TestNormalizeRuntime(t *testing.T) {
	cases := map[string]Runtime{
		"cuda":   RuntimeCUDA,
		" CUDA ": RuntimeCUDA,
		"Vulkan": RuntimeVulkan,
		"cpu":    RuntimeCPU,
		"auto":   RuntimeAuto,
		"":       RuntimeAuto,
		"bogus":  RuntimeAuto,
	}
	for in, want := range cases {
		if got := normalizeRuntime(in); got != want {
			t.Errorf("normalizeRuntime(%q)=%q want %q", in, got, want)
		}
	}
}

func TestValidRuntime(t *testing.T) {
	for _, ok := range []string{"auto", "cuda", "vulkan", "cpu"} {
		if !ValidRuntime(ok) {
			t.Errorf("ValidRuntime(%q) should be true", ok)
		}
	}
	for _, bad := range []string{"", "metal", "tpu", "CUDA2"} {
		if ValidRuntime(bad) {
			t.Errorf("ValidRuntime(%q) should be false", bad)
		}
	}
}

func TestDetectRuntime_NvidiaWins(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("amd-node", map[string]string{"amd.com/gpu": "1"}),
		gpuNode("nv-node", map[string]string{"nvidia.com/gpu": "1"}),
	}
	rt, res, matched := detectRuntime(nodes, ParseGPUResourceNames(DefaultGPUResourceNames))
	if rt != RuntimeCUDA {
		t.Fatalf("expected cuda (NVIDIA wins), got %q", rt)
	}
	if res != "nvidia.com/gpu" {
		t.Fatalf("expected nvidia.com/gpu, got %q", res)
	}
	if len(matched) != 1 || matched[0] != "nv-node" {
		t.Fatalf("expected [nv-node], got %v", matched)
	}
}

func TestDetectRuntime_AMDOnly_Vulkan(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("amd-node", map[string]string{"amd.com/gpu": "2"}),
	}
	rt, res, matched := detectRuntime(nodes, ParseGPUResourceNames(DefaultGPUResourceNames))
	if rt != RuntimeVulkan || res != "amd.com/gpu" || matched[0] != "amd-node" {
		t.Fatalf("expected vulkan/amd.com/gpu/amd-node, got %q/%q/%v", rt, res, matched)
	}
}

func TestDetectRuntime_IntelOnly_Vulkan(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("intel-node", map[string]string{"gpu.intel.com/i915": "1"}),
	}
	rt, res, _ := detectRuntime(nodes, ParseGPUResourceNames(DefaultGPUResourceNames))
	if rt != RuntimeVulkan || res != "gpu.intel.com/i915" {
		t.Fatalf("expected vulkan/i915, got %q/%q", rt, res)
	}
}

func TestDetectRuntime_NoGPU_CPU(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("plain", map[string]string{"cpu": "8"}),
	}
	rt, res, matched := detectRuntime(nodes, ParseGPUResourceNames(DefaultGPUResourceNames))
	if rt != RuntimeCPU || res != "" || matched != nil {
		t.Fatalf("expected cpu/empty/nil, got %q/%q/%v", rt, res, matched)
	}
}

func TestDetectRuntime_ZeroQtyNotMatched(t *testing.T) {
	nodes := []corev1.Node{
		gpuNode("nv-zero", map[string]string{"nvidia.com/gpu": "0"}),
	}
	rt, _, _ := detectRuntime(nodes, ParseGPUResourceNames(DefaultGPUResourceNames))
	if rt != RuntimeCPU {
		t.Fatalf("zero-allocatable GPU must not match; got %q", rt)
	}
}

func TestShapeForRuntime(t *testing.T) {
	cuda := shapeForRuntime(RuntimeCUDA, "nvidia.com/gpu", []string{"n1"})
	if cuda.GPUResource != "nvidia.com/gpu" || cuda.NeedsDRI {
		t.Fatalf("cuda shape wrong: %+v", cuda)
	}
	vk := shapeForRuntime(RuntimeVulkan, "amd.com/gpu", []string{"a1"})
	if vk.GPUResource != "amd.com/gpu" || !vk.NeedsDRI {
		t.Fatalf("vulkan shape wrong (needs dri): %+v", vk)
	}
	cpu := shapeForRuntime(RuntimeCPU, "", nil)
	if cpu.GPUResource != "" || cpu.NeedsDRI || len(cpu.Nodes) != 0 {
		t.Fatalf("cpu shape wrong: %+v", cpu)
	}
}

func TestClassifyGPUResources_UnknownGoesVulkan(t *testing.T) {
	vr := classifyGPUResources([]string{"custom.example.com/gpu"})
	if len(vr.nvidia) != 0 {
		t.Fatalf("unknown resource must not be nvidia: %+v", vr)
	}
	if len(vr.amd) != 1 {
		t.Fatalf("unknown resource should fall to vulkan(amd) bucket: %+v", vr)
	}
}
