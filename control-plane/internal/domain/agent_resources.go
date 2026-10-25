package domain

// S-287 — platform-configurable agent pod resources.
//
// The four resource values (CPU request/limit, memory request/limit)
// applied to every agent pod live in the platform_settings key/value
// store (no migration needed — migration 0026 created the table).
// The control-plane validates them here, persists them, and mirrors
// them into the operator-consumed ConfigMap (skquad-agent-resources)
// so the operator injects them into agent Deployment pod specs.
//
// Built-in defaults match the operator's historical env-var defaults
// (SKQUAD_AGENT_CPU_REQUEST=100m, SKQUAD_AGENT_CPU_LIMIT=500m,
// SKQUAD_AGENT_MEMORY_REQUEST=128Mi, SKQUAD_AGENT_MEMORY_LIMIT=512Mi)
// so enabling this feature changes nothing until an admin edits it.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Platform settings keys for agent pod resources (S-287).
const (
	PlatformSettingAgentCPURequest    = "agent_cpu_request"
	PlatformSettingAgentCPULimit      = "agent_cpu_limit"
	PlatformSettingAgentMemoryRequest = "agent_memory_request"
	PlatformSettingAgentMemoryLimit   = "agent_memory_limit"
)

// Built-in defaults for agent pod resources (S-287). These mirror the
// operator's env-var defaults so the platform behaves identically when
// no admin override is stored.
const (
	DefaultAgentCPURequest    = "100m"
	DefaultAgentCPULimit      = "500m"
	DefaultAgentMemoryRequest = "128Mi"
	DefaultAgentMemoryLimit   = "512Mi"
)

// cpuQuantityRe matches Kubernetes CPU quantities: a plain decimal
// (cores, e.g. "1", "0.5") or a milli-core value ("100m"). go.mod
// carries no k8s.io/apimachinery here, so this is the deliberately
// scoped stand-in for resource.ParseQuantity (same posture as
// storageQuantityRe in storage_size.go).
var cpuQuantityRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(m?)$`)

// ParseCPUQuantity parses a Kubernetes CPU quantity ("100m", "0.5",
// "2") and returns millicores. It rejects malformed input and zero or
// negative values — a zero request/limit is not a useful platform
// setting (k8s treats 0 as "unbounded-ish/absent", which is not what
// an admin means here).
func ParseCPUQuantity(raw string) (int64, error) {
	m := cpuQuantityRe.FindStringSubmatch(raw)
	if m == nil {
		return 0, fmt.Errorf("invalid CPU quantity %q: expected a value like 100m, 0.5 or 2", raw)
	}
	cores, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid CPU quantity %q: %w", raw, err)
	}
	millis := cores
	if m[2] == "" {
		millis = cores * 1000
	}
	if millis <= 0 {
		return 0, fmt.Errorf("CPU quantity %q must be positive", raw)
	}
	if millis > 1<<40 {
		return 0, fmt.Errorf("CPU quantity %q is implausibly large", raw)
	}
	return int64(millis + 0.5), nil
}

// AgentResourceSettings is the resolved (validated) set of agent pod
// resource values as strings in Kubernetes quantity format.
type AgentResourceSettings struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// Defaults returns the built-in agent resource settings.
func DefaultAgentResources() AgentResourceSettings {
	return AgentResourceSettings{
		CPURequest:    DefaultAgentCPURequest,
		CPULimit:      DefaultAgentCPULimit,
		MemoryRequest: DefaultAgentMemoryRequest,
		MemoryLimit:   DefaultAgentMemoryLimit,
	}
}

// ValidateAgentResources validates a full set of agent resource
// values (raw quantity strings): all four required, CPU values parse
// as CPU quantities, memory values parse as storage quantities, and
// each request is <= its limit. Returns the normalised set or an error
// describing the first problem.
func ValidateAgentResources(cpuRequest, cpuLimit, memRequest, memLimit string) (AgentResourceSettings, error) {
	set := AgentResourceSettings{
		CPURequest:    cpuRequest,
		CPULimit:      cpuLimit,
		MemoryRequest: memRequest,
		MemoryLimit:   memLimit,
	}
	cpuReqMillis, err := ParseCPUQuantity(cpuRequest)
	if err != nil {
		return set, fmt.Errorf("cpu_request: %w", err)
	}
	cpuLimMillis, err := ParseCPUQuantity(cpuLimit)
	if err != nil {
		return set, fmt.Errorf("cpu_limit: %w", err)
	}
	if cpuReqMillis > cpuLimMillis {
		return set, fmt.Errorf("cpu_request %q exceeds cpu_limit %q", cpuRequest, cpuLimit)
	}
	memReqBytes, err := ParseStorageSize(memRequest)
	if err != nil {
		return set, fmt.Errorf("memory_request: %w", err)
	}
	memLimBytes, err := ParseStorageSize(memLimit)
	if err != nil {
		return set, fmt.Errorf("memory_limit: %w", err)
	}
	if memReqBytes > memLimBytes {
		return set, fmt.Errorf("memory_request %q exceeds memory_limit %q", memRequest, memLimit)
	}
	return set, nil
}

// ResolveAgentResources reads the four platform settings through the
// provided getter (s.store.GetPlatformSetting in the API layer),
// falling back to the built-in defaults for any unset or blank value.
// Stored values were validated on write; the operator re-validates on
// read, so a corrupted row degrades to defaults rather than wedging
// agent scheduling.
func ResolveAgentResources(ctx context.Context, get func(context.Context, string) (string, bool, error)) AgentResourceSettings {
	pick := func(key, fallback string) string {
		if raw, found, err := get(ctx, key); err == nil && found {
			if v := strings.TrimSpace(raw); v != "" {
				return v
			}
		}
		return fallback
	}
	d := DefaultAgentResources()
	return AgentResourceSettings{
		CPURequest:    pick(PlatformSettingAgentCPURequest, d.CPURequest),
		CPULimit:      pick(PlatformSettingAgentCPULimit, d.CPULimit),
		MemoryRequest: pick(PlatformSettingAgentMemoryRequest, d.MemoryRequest),
		MemoryLimit:   pick(PlatformSettingAgentMemoryLimit, d.MemoryLimit),
	}
}
