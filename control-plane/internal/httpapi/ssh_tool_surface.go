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
	"regexp"
	"sort"
	"strings"
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

// minPositive returns the smallest positive value in vals, or def when
// none of them is positive (tightest-wins policy layering).
func minPositive(def int, vals ...int) int {
	out := def
	for _, v := range vals {
		if v > 0 && v < out {
			out = v
		}
	}
	return out
}

// minPositiveUnbounded returns the smallest positive value in vals, or
// 0 when none — 0 means "no rate limit" in the policy layers.
func minPositiveUnbounded(vals ...int) int {
	out := 0
	for _, v := range vals {
		if v > 0 && (out == 0 || v < out) {
			out = v
		}
	}
	return out
}

// sshToolSchema builds the `tools` JSON array surfaced on an ssh resource
// entry in agentRuntimeResource. Parse failures degrade to omitting the
// tools rather than breaking discovery — the gateway still fails closed.
func sshToolSchema(resourceName, resourceID string, ceilingRaw, constraintsRaw, endpointConfigRaw json.RawMessage) json.RawMessage {
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

	certTTL := minPositive(sshDefaultCertTTLMinutes, ceiling.certTTLMinutes, constraints.certTTLMinutes)
	execTimeout := minPositive(sshDefaultExecTimeoutSecs, ceiling.execTimeoutSeconds, constraints.execTimeoutSeconds)
	maxSessions := minPositive(sshDefaultMaxSessions, ceiling.maxConcurrentSessions, constraints.maxConcurrentSessions)

	gatewayNote := "Calls execute in the quarantined terminal-service; SSH credentials (CA-signed, TTL " + itoa(certTTL) + "m) never enter your sandbox. " +
		"Every command is audited BEFORE execution. Commands matching denied patterns pause for owner confirmation (Inbox). " +
		"No scp, no port-forwarding, no raw tunnels."

	out := []map[string]any{
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

	// TG-11 §6.7: ssh_apply / ssh_apply_status surface ONLY when the
	// resource carries artifact config AND the grant carries the apply
	// capability (constraints.apply_enabled=true). Under-matching is a
	// bypass: artifact present but capability absent ⇒ tools ABSENT,
	// not present-and-refused.
	if hasArtifactConfig(endpointConfigRaw) && grantHasApplyCapability(constraintsRaw) {
		out = append(out, sshApplyToolSchemas(resourceID, ceiling, constraints, hostsAllow)...)
	}

	tools := out
	out2, err := json.Marshal(tools)
	if err != nil {
		return nil
	}
	return out2
}

// hasArtifactConfig reports whether endpoint_config carries a usable
// artifact section (object with non-empty git_url).
func hasArtifactConfig(endpointConfigRaw json.RawMessage) bool {
	if len(endpointConfigRaw) == 0 || string(endpointConfigRaw) == "null" {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(endpointConfigRaw, &obj); err != nil {
		return false
	}
	raw, ok := obj["artifact"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var art struct {
		GitURL string `json:"git_url"`
	}
	if err := json.Unmarshal(raw, &art); err != nil {
		return false
	}
	return strings.TrimSpace(art.GitURL) != ""
}

// grantHasApplyCapability reads constraints.apply_enabled (bool).
// Anything other than literal true = no capability.
func grantHasApplyCapability(constraintsRaw json.RawMessage) bool {
	if len(constraintsRaw) == 0 || string(constraintsRaw) == "null" {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(constraintsRaw, &obj); err != nil {
		return false
	}
	raw, ok := obj["apply_enabled"]
	if !ok {
		return false
	}
	var en bool
	if err := json.Unmarshal(raw, &en); err != nil {
		return false
	}
	return en
}

// effectiveSSHHostGroups filters the ceiling's admin-managed host_groups
// by the grant's host_groups_allow, yielding the surfaced name → {hosts,tier} map.
func effectiveSSHHostGroups(ceiling, constraints sshPolicyLayer) map[string]any {
	groups := map[string]any{}
	if len(ceiling.hostGroups) == 0 {
		return groups
	}
	var gh map[string]json.RawMessage
	if json.Unmarshal(ceiling.hostGroups, &gh) != nil {
		return groups
	}
	for name, raw := range gh {
		if constraints.hostGroupsAllow != nil && !globMatchAnyString(*constraints.hostGroupsAllow, name) {
			continue
		}
		var g struct {
			Hosts []string `json:"hosts"`
			Tier  string   `json:"tier"`
		}
		if json.Unmarshal(raw, &g) == nil {
			groups[name] = map[string]any{"hosts": g.Hosts, "tier": g.Tier}
		}
	}
	return groups
}

// sshApplyToolSchemas builds the ssh_apply + ssh_apply_status tool
// entries with the effective (ceiling ∧ grant) apply constraints so
// agents see exactly what the gateway will enforce.
func sshApplyToolSchemas(resourceID string, ceiling, constraints sshPolicyLayer, hostsAllow []string) []map[string]any {
	// host_groups: from the ceiling; narrowed by constraints.host_groups_allow when set.
	groups := effectiveSSHHostGroups(ceiling, constraints)

	checkOnlyOnly := false
	if constraints.checkOnly != nil {
		checkOnlyOnly = *constraints.checkOnly
	}

	applyNote := "Apply a MERGED, fully-pinned playbook revision (full 40-char git SHA) through the governed artifact lane. " +
		"The repo comes only from the resource's registered artifact config — the call cannot redirect it. " +
		"Every apply requires confirmation (identity: host_group + playbook; high tier needs owner + admin co-sign) and is fully recorded. " +
		"Mutation happens via approved change artifacts, never keystrokes."

	apply := map[string]any{
		"name":        "ssh_apply",
		"description": applyNote,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource_id": map[string]any{"type": "string", "const": resourceID},
				"playbook": map[string]any{
					"type":        "string",
					"description": "Playbook path within the registered playbooks_path (relative, traversal-free).",
				},
				"git_rev": map[string]any{
					"type":        "string",
					"pattern":     "^[0-9a-f]{40}$",
					"description": "Full 40-char lowercase git SHA of the MERGED revision. Branch names and short SHAs are rejected.",
				},
				"host_group": map[string]any{
					"type":        "string",
					"description": "Target host group from the resource ceiling's admin-managed inventory.",
					"enum":        sortedGroupKeys(groups),
				},
				"check_only": map[string]any{
					"type":        "boolean",
					"description": "Dry-run (ansible --check). Dry-run-only grants refuse check_only=false.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Gateway wait window before handoff to ssh_apply_status (default 120, max 900).",
				},
			},
			"required":             []string{"resource_id", "playbook", "git_rev", "host_group"},
			"additionalProperties": false,
		},
		"constraints": map[string]any{
			"host_groups":       groups,
			"host_groups_allow": effectiveStringList(derefStrings(constraints.hostGroupsAllow)),
			"playbooks_allow":   effectiveStringList(constraints.playbooksAllow),
			"hosts_allow":       hostsAllow,
			"check_only_only":   checkOnlyOnly,
			"max_wait_seconds":  900,
		},
	}

	status := map[string]any{
		"name":        "ssh_apply_status",
		"description": "Poll a started apply for its result: {apply_id, status, exit_code?, per_host?, refusal_reason?, recording_id?}.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource_id": map[string]any{"type": "string", "const": resourceID},
				"apply_id":    map[string]any{"type": "string", "description": "Apply id returned by ssh_apply."},
			},
			"required":             []string{"resource_id", "apply_id"},
			"additionalProperties": false,
		},
	}
	return []map[string]any{apply, status}
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
	// TG-11 apply-relevant fields (ceiling: host_groups, require_tip;
	// constraints: apply_enabled, playbooks_allow, host_groups_allow, check_only).
	hostGroups      json.RawMessage
	requireTip      json.RawMessage
	playbooksAllow  []string
	hostGroupsAllow *[]string
	checkOnly       *bool
	applyEnabled    bool
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
	if v, ok := obj["host_groups"]; ok {
		layer.hostGroups = v
	}
	if v, ok := obj["require_tip"]; ok {
		layer.requireTip = v
	}
	if v := stringListPointer(obj, "playbooks_allow"); v != nil {
		layer.playbooksAllow = *v
	}
	layer.hostGroupsAllow = stringListPointer(obj, "host_groups_allow")
	if raw, ok := obj["check_only"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			layer.checkOnly = &b
		}
	}
	if raw, ok := obj["apply_enabled"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			layer.applyEnabled = b
		}
	}
	return layer
}

// effectiveStringList renders a possibly-nil list as [] for JSON.
func effectiveStringList(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// globMatchAnyString mirrors the gateway's anchored case-insensitive glob
// semantics (drivers/ssh/policy.go) for discovery-side narrowing.
func globMatchAnyString(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatchString(strings.TrimSpace(p), strings.ToLower(s)) {
			return true
		}
	}
	return false
}

func globMatchString(pattern, s string) bool {
	var sb strings.Builder
	sb.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func sortedGroupKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// derefStrings unwraps a *[]string to []string (nil-safe).
func derefStrings(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
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
