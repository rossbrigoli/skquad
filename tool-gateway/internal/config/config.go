// Package config loads tool-gateway configuration from environment variables.
// Conventions mirror control-plane/internal/config: every knob is a
// SKQUAD_TOOL_GATEWAY_* env var with a safe default.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all tool-gateway settings.
type Config struct {
	// HTTP
	Addr string // SKQUAD_TOOL_GATEWAY_ADDR, default ":8080"

	// Control plane
	CPBaseURL string // SKQUAD_CP_BASE_URL, e.g. http://skquad-api-server.skquad-system.svc.cluster.local:8080
	// ProbeAgent is the agent id used by the background reachability probe
	// against the CP policy API (SKQUAD_TOOL_GATEWAY_PROBE_AGENT). Any HTTP
	// response (even 401/404) counts as "CP reachable".
	ProbeAgent string

	// Policy cache
	PolicyTTL      time.Duration // SKQUAD_TOOL_GATEWAY_POLICY_TTL, default 30s
	PolicyProbeGap time.Duration // SKQUAD_TOOL_GATEWAY_PROBE_GAP, default 10s
	PolicyTimeout  time.Duration // SKQUAD_TOOL_GATEWAY_POLICY_TIMEOUT, default 5s

	// Kill switch (design §5.2 / plan TG-1). When false the gateway refuses
	// all non-health traffic with 503 gateway_disabled.
	Enabled bool // SKQUAD_TOOL_GATEWAY_ENABLED, default true

	// Boundary verification (fail-closed launch, design §5.2).
	// "static" = stub verifier that reports the fence as applied (TG-1
	// placeholder until the operator wires the real netpol-applied hook).
	// "none" = verifier always reports not-applied; readiness never passes.
	BoundaryVerifier string // SKQUAD_TOOL_GATEWAY_BOUNDARY_VERIFIER, default "static"

	// Audit sink. TG-1 ships the stdout structured-JSON sink; a CP/pipe
	// sink can be added behind the audit.Emitter interface later.
	AuditSink string // SKQUAD_TOOL_GATEWAY_AUDIT_SINK, default "stdout"

	// InternalToken is the shared secret for trusted-internal callers
	// INTO the gateway (CP → gateway, e.g. the TG-5 slice B1 MCP
	// enumerate endpoint). Presented via X-Skquad-Internal-Token or
	// Authorization: Bearer; constant-time compared. Empty (default)
	// disables internal endpoints fail-closed (503).
	InternalToken string // SKQUAD_GATEWAY_INTERNAL_TOKEN
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	cfg := &Config{
		Addr:             envString("SKQUAD_TOOL_GATEWAY_ADDR", ":8080"),
		CPBaseURL:        strings.TrimRight(envString("SKQUAD_CP_BASE_URL", "http://skquad-api-server.skquad-system.svc.cluster.local:8080"), "/"),
		ProbeAgent:       envString("SKQUAD_TOOL_GATEWAY_PROBE_AGENT", "probe"),
		PolicyTTL:        envDuration("SKQUAD_TOOL_GATEWAY_POLICY_TTL", 30*time.Second),
		PolicyProbeGap:   envDuration("SKQUAD_TOOL_GATEWAY_PROBE_GAP", 10*time.Second),
		PolicyTimeout:    envDuration("SKQUAD_TOOL_GATEWAY_POLICY_TIMEOUT", 5*time.Second),
		Enabled:          envBool("SKQUAD_TOOL_GATEWAY_ENABLED", true),
		BoundaryVerifier: envString("SKQUAD_TOOL_GATEWAY_BOUNDARY_VERIFIER", "static"),
		AuditSink:        envString("SKQUAD_TOOL_GATEWAY_AUDIT_SINK", "stdout"),
		InternalToken:    strings.TrimSpace(os.Getenv("SKQUAD_GATEWAY_INTERNAL_TOKEN")),
	}
	if cfg.CPBaseURL == "" {
		return nil, fmt.Errorf("SKQUAD_CP_BASE_URL is required")
	}
	switch cfg.BoundaryVerifier {
	case "static", "none":
	default:
		return nil, fmt.Errorf("SKQUAD_TOOL_GATEWAY_BOUNDARY_VERIFIER must be one of: static, none")
	}
	return cfg, nil
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
