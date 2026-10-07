package httpapi

// TG-10 (S-256): ssh tool surface for fetch-at-wake discovery.
//
// docs/tool-gateway.md §6.6 defines the agent-facing tools
// `ssh_exec(resource_id, host, command, timeout_seconds?)` and the
// interactive session family `ssh_session_open / ssh_session_send /
// ssh_session_events / ssh_session_close`. Like rest_call, these are NOT
// builtins: they appear in the typed discovery response
// (GET /agents/me/resources) as resource-scoped `tools` entries ONLY
// when the agent holds an active ssh grant — the fetch-at-wake contract.
//
// The effective bounds advertised here mirror the gateway's ssh policy
// folding (tool-gateway/internal/drivers/ssh/policy.go): hosts_allow is
// default-deny (grant ∩ ceiling), hosts_deny unions, cert TTL and
// timeouts take the tightest value. What the agent sees is what the
// gateway enforces. Command allow/deny patterns are surfaced so agents
// can pre-plan, but the deny-pattern → confirmation-gate decision is
// made at dispatch time (TG-8 gate), never here.

import (
	"encoding/json"
)

const (
	sshExecToolName = "ssh_exec"
	// sshDefaultCertTTLMinutes / sshDefaultExecTimeoutSeconds mirror the
	// gateway driver defaults (drivers/ssh defaults). Kept local so
	// discovery never imports the gateway module.
	sshDefaultCertTTLMinutes  = 15
	sshDefaultExecTimeoutSecs = 60
	sshMaxExecTimeoutSecs     = 300
	sshDefaultMaxSessions     = 2
)

// sshToolSchema builds the `tools` JSON array surfaced on an ssh resource
// entry in agentRuntimeResource. Parse failures degrade to omitting the
// tools rather than breaking discovery — the gateway still fails closed.
func sshToolSchema(resourceName, resourceID string, ceilingRaw, constraintsRaw json.RawMessage) json.RawMessage {
	ceiling := parseSSHPolicyLayer(ceilingRaw)
	constraints := parseSSHPolicyLayer(constraintsRaw)

	hostsAllow := intersectStringLists(ceiling.hostsAllow, constraints.hostsAllow)
	hostsDeny := unionStrings(ceiling.hostsDeny, constraints.hostsDeny)
	commandDeny := unionStrings(ceiling.commandDeny, constraints.commandDeny)

	commandAllow := []string{}
	switch {
	case constraints.commandAllow != nil:
		commandAllow = *constraints.commandAllow
	case ceiling.commandAllow != nil:
		commandAllow = *ceiling.commandAllow
	}

	certTTL := sshDefaultCertTTLMinutes
	for _, v := range []int{ceiling.certTTLMinutes, constraints.certTTLMinutes} {
		if v > 0 && v < certTTL {
			certTTL = v
		}
	}
	execTimeout := sshDefaultExecTimeoutSecs
	for _, v := range []int{ceiling.execTimeoutSeconds, constraints.execTimeoutSeconds} {
		if v > 0 && v < execTimeout {
			execTimeout = v
		}
	}
	maxSessions := sshDefaultMaxSessions
	for _, v := range []int{ceiling.maxConcurrentSessions, constraints.maxConcurrentSessions} {
		if v > 0 && v < maxSessions {
			maxSessions = v
		}
	}

	gatewayNote := "Calls execute in the quarantined terminal-service; SSH credentials (CA-signed, TTL " + itoa(certTTL) + "m) never enter your sandbox. " +
		"Every command is audited BEFORE execution. Commands matching denied patterns pause for owner confirmation (Inbox). " +
		"No scp, no port-forwarding, no raw tunnels."

	tools := []map[string]any{
		{
			"name":        sshExecToolName,
			"description": "Run one command on an allowlisted host through the skquad tool gateway. " + gatewayNote,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"resource_id": map[string]any{"type": "string", "const": resourceID},
					"host": map[string]any{
						"type":        "string",
						"description": "Target host. Must match hosts_allow and no hosts_deny pattern.",
					},
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command to execute on the host.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": itoa(execTimeout) + "s max; the effective exec timeout is min(requested, ceiling).",
					},
				},
				"required":             []string{"resource_id", "host", "command"},
				"additionalProperties": false,
			},
			"constraints": map[string]any{
				"hosts_allow":          hostsAllow,
				"hosts_deny":           hostsDeny,
				"command_allow":        commandAllow,
				"command_deny":         commandDeny,
				"cert_ttl_minutes":     certTTL,
				"exec_timeout_seconds": execTimeout,
			},
		},
		{
			"name":        "ssh_session_open",
			"description": "Open an interactive PTY session on an allowlisted host (streamed via the gateway). " + gatewayNote + " Max " + itoa(maxSessions) + " concurrent sessions per grant.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"resource_id": map[string]any{"type": "string", "const": resourceID},
					"host":        map[string]any{"type": "string", "description": "Target host (allowlist-checked)."},
				},
				"required":             []string{"resource_id", "host"},
				"additionalProperties": false,
			},
		},
		{
			"name":        "ssh_session_send",
			"description": "Send stdin to an open ssh session (base64).",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"resource_id": map[string]any{"type": "string", "const": resourceID},
					"session_id":  map[string]any{"type": "string", "description": "Session id returned by ssh_session_open."},
					"stdin_b64":   map[string]any{"type": "string"},
				},
				"required":             []string{"resource_id", "session_id", "stdin_b64"},
				"additionalProperties": false,
			},
		},
		{
			"name":        "ssh_session_events",
			"description": "Poll incremental session output since cursor; returns {cursor, events, closed}.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"resource_id": map[string]any{"type": "string", "const": resourceID},
					"session_id":  map[string]any{"type": "string"},
					"cursor":      map[string]any{"type": "integer"},
				},
				"required":             []string{"resource_id", "session_id"},
				"additionalProperties": false,
			},
		},
		{
			"name":        "ssh_session_close",
			"description": "Close an ssh session; finalizes the recording.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"resource_id": map[string]any{"type": "string", "const": resourceID},
					"session_id":  map[string]any{"type": "string"},
				},
				"required":             []string{"resource_id", "session_id"},
				"additionalProperties": false,
			},
		},
	}

	out, err := json.Marshal(tools)
	if err != nil {
		return nil
	}
	return out
}

// sshPolicyLayer is a minimal parse of one ssh policy layer (ceiling or
// grant constraints). Pointer slices distinguish unset from empty.
type sshPolicyLayer struct {
	hostsAllow            *[]string
	hostsDeny             []string
	commandAllow          *[]string
	commandDeny           []string
	certTTLMinutes        int
	maxConcurrentSessions int
	execTimeoutSeconds    int
}

func parseSSHPolicyLayer(raw json.RawMessage) sshPolicyLayer {
	var layer sshPolicyLayer
	if len(raw) == 0 || string(raw) == "null" {
		return layer
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return layer
	}
	layer.hostsAllow = stringListPointer(obj, "hosts_allow")
	layer.hostsDeny = stringList(obj, "hosts_deny")
	layer.commandAllow = stringListPointer(obj, "command_allow")
	layer.commandDeny = stringList(obj, "command_deny")
	layer.certTTLMinutes = intField(obj, "cert_ttl_minutes")
	layer.maxConcurrentSessions = intField(obj, "max_concurrent_sessions")
	layer.execTimeoutSeconds = intField(obj, "exec_timeout_seconds")
	return layer
}

// intersectStringLists returns nil when either side is unset (no
// restriction from that layer); otherwise the intersection.
func intersectStringLists(a, b *[]string) []string {
	if a == nil && b == nil {
		return []string{}
	}
	if a == nil {
		return append([]string{}, *b...)
	}
	if b == nil {
		return append([]string{}, *a...)
	}
	bset := make(map[string]bool, len(*b))
	for _, s := range *b {
		bset[s] = true
	}
	out := []string{}
	for _, s := range *a {
		if bset[s] {
			out = append(out, s)
		}
	}
	return out
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := []string{}
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func itoa(n int) string {
	b, err := json.Marshal(n)
	if err != nil {
		return "0"
	}
	return string(b)
}
