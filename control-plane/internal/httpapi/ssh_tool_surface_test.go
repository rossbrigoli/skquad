package httpapi

import (
	"encoding/json"
	"testing"
)

func TestSSHToolSchema_Shape(t *testing.T) {
	ceiling := json.RawMessage(`{"hosts_allow":["staging-*","prod-web-*"],"hosts_deny":["prod-db*"],"command_allow":["ls*","journalctl*"],"cert_ttl_minutes":30,"exec_timeout_seconds":120,"max_concurrent_sessions":4}`)
	constraints := json.RawMessage(`{"hosts_allow":["staging-*"],"exec_timeout_seconds":60}`)

	raw := sshToolSchema("Lab Hosts", "res-ssh-1", ceiling, constraints, nil)
	if raw == nil {
		t.Fatal("nil schema")
	}
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	if len(tools) != 5 {
		t.Fatalf("want 5 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl["name"].(string)] = true
	}
	for _, want := range []string{"ssh_exec", "ssh_session_open", "ssh_session_send", "ssh_session_events", "ssh_session_close"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}

	exec := tools[0]
	params := exec["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	rid := props["resource_id"].(map[string]any)
	if rid["const"] != "res-ssh-1" {
		t.Errorf("resource_id not const-bound: %v", rid)
	}
	con := exec["constraints"].(map[string]any)
	// hosts_allow intersection: only staging-* survives both layers.
	hosts := con["hosts_allow"].([]any)
	if len(hosts) != 1 || hosts[0] != "staging-*" {
		t.Errorf("hosts fold wrong: %v", hosts)
	}
	// deny unions from ceiling.
	deny := con["hosts_deny"].([]any)
	if len(deny) != 1 || deny[0] != "prod-db*" {
		t.Errorf("hosts_deny fold wrong: %v", deny)
	}
	// exec timeout tightest = 60 (grant) vs 120 (ceiling).
	if con["exec_timeout_seconds"].(float64) != 60 {
		t.Errorf("exec timeout fold wrong: %v", con["exec_timeout_seconds"])
	}
	// cert TTL = ceiling 30 vs default 15 → tightest 15.
	if con["cert_ttl_minutes"].(float64) != 15 {
		t.Errorf("cert ttl fold wrong: %v", con["cert_ttl_minutes"])
	}
	// command_allow from ceiling survives.
	ca := con["command_allow"].([]any)
	if len(ca) != 2 {
		t.Errorf("command_allow wrong: %v", ca)
	}
}

func TestSSHToolSchema_DegradesOnGarbage(t *testing.T) {
	// Garbage layers must not panic; unset lists degrade to empty, defaults apply.
	raw := sshToolSchema("X", "r2", json.RawMessage(`"not-an-object"`), json.RawMessage(`{bad`), json.RawMessage(`{"artifact": 7}`))
	if raw == nil {
		t.Fatal("nil schema on garbage input")
	}
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	con := tools[0]["constraints"].(map[string]any)
	if con["cert_ttl_minutes"].(float64) != 15 || con["exec_timeout_seconds"].(float64) != 60 {
		t.Errorf("defaults not applied: %v", con)
	}
}

// ---- TG-11 slice C: ssh_apply discovery gating ----

const tg11Ceiling = `{"hosts_allow":["web1.staging.lab","web1.lab","web2.lab"],
 "host_groups":{"staging":{"hosts":["web1.staging.lab"],"tier":"medium"},"prod":{"hosts":["web1.lab","web2.lab"],"tier":"high"}},
 "mirrors_allow":["pypi.internal.lab"],"require_tip":{"high":true}}`

const tg11Artifact = `{"artifact":{"git_url":"https://github.com/rossbrigoli/infra-playbooks.git","default_branch":"main","playbooks_path":"playbooks/"}}`

func toolNames(raw json.RawMessage) map[string]bool {
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		panic(err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl["name"].(string)] = true
	}
	return names
}

func TestSSHApplySurface_PresentWithArtifactAndCapability(t *testing.T) {
	constraints := json.RawMessage(`{"apply_enabled":true,"host_groups_allow":["staging"],"playbooks_allow":["web/*"],"check_only":true}`)
	raw := sshToolSchema("Lab", "res-ssh-9", json.RawMessage(tg11Ceiling), constraints, json.RawMessage(tg11Artifact))
	names := toolNames(raw)
	if !names["ssh_apply"] || !names["ssh_apply_status"] {
		t.Fatalf("apply tools missing: %v", names)
	}
	var tools []map[string]any
	json.Unmarshal(raw, &tools)
	var apply map[string]any
	for _, tl := range tools {
		if tl["name"] == "ssh_apply" {
			apply = tl
		}
	}
	con := apply["constraints"].(map[string]any)
	groups := con["host_groups"].(map[string]any)
	if _, ok := groups["staging"]; !ok {
		t.Errorf("staging group missing from surfaced host_groups: %v", groups)
	}
	if _, ok := groups["prod"]; ok {
		t.Errorf("prod must be narrowed out by host_groups_allow: %v", groups)
	}
	if con["check_only_only"].(bool) != true {
		t.Errorf("check_only_only wrong: %v", con["check_only_only"])
	}
	pa := con["playbooks_allow"].([]any)
	if len(pa) != 1 || pa[0] != "web/*" {
		t.Errorf("playbooks_allow wrong: %v", pa)
	}
	// git_rev pattern pinned to full SHA.
	params := apply["parameters"].(map[string]any)
	rev := params["properties"].(map[string]any)["git_rev"].(map[string]any)
	if rev["pattern"] != "^[0-9a-f]{40}$" {
		t.Errorf("git_rev pattern wrong: %v", rev)
	}
}

func TestSSHApplySurface_AbsentWithoutCapability(t *testing.T) {
	// Artifact configured but grant lacks apply capability → ABSENT (bypass rule).
	raw := sshToolSchema("Lab", "res-ssh-9", json.RawMessage(tg11Ceiling), json.RawMessage(`{}`), json.RawMessage(tg11Artifact))
	names := toolNames(raw)
	if names["ssh_apply"] || names["ssh_apply_status"] {
		t.Fatalf("apply tools must be absent without capability: %v", names)
	}
	// Explicit false also absent.
	raw = sshToolSchema("Lab", "res-ssh-9", json.RawMessage(tg11Ceiling), json.RawMessage(`{"apply_enabled":false}`), json.RawMessage(tg11Artifact))
	names = toolNames(raw)
	if names["ssh_apply"] {
		t.Fatal("apply_enabled:false must not surface tools")
	}
}

func TestSSHApplySurface_AbsentWithoutArtifact(t *testing.T) {
	// Capability but no artifact config → ABSENT.
	raw := sshToolSchema("Lab", "res-ssh-9", json.RawMessage(tg11Ceiling), json.RawMessage(`{"apply_enabled":true}`), json.RawMessage(`{}`))
	names := toolNames(raw)
	if names["ssh_apply"] || names["ssh_apply_status"] {
		t.Fatalf("apply tools must be absent without artifact config: %v", names)
	}
	// Artifact present but git_url empty → not usable → absent.
	raw = sshToolSchema("Lab", "res-ssh-9", json.RawMessage(tg11Ceiling), json.RawMessage(`{"apply_enabled":true}`),
		json.RawMessage(`{"artifact":{"default_branch":"main"}}`))
	names = toolNames(raw)
	if names["ssh_apply"] {
		t.Fatal("artifact without git_url must not surface tools")
	}
}
