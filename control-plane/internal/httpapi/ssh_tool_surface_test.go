package httpapi

import (
	"encoding/json"
	"testing"
)

func TestSSHToolSchema_Shape(t *testing.T) {
	ceiling := json.RawMessage(`{"hosts_allow":["staging-*","prod-web-*"],"hosts_deny":["prod-db*"],"command_allow":["ls*","journalctl*"],"cert_ttl_minutes":30,"exec_timeout_seconds":120,"max_concurrent_sessions":4}`)
	constraints := json.RawMessage(`{"hosts_allow":["staging-*"],"exec_timeout_seconds":60}`)

	raw := sshToolSchema("Lab Hosts", "res-ssh-1", ceiling, constraints)
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
	raw := sshToolSchema("X", "r2", json.RawMessage(`"not-an-object"`), json.RawMessage(`{bad`))
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
