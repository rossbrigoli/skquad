package egresspolicy

import (
	"encoding/json"
	"testing"
)

func TestValidateEndpointConfig_SSH(t *testing.T) {
	ok := `{"ssh_user":"ops","port":22,"known_hosts":"host ssh-ed25519 AAAA...","auth_mode":"ca"}`
	if v := ValidateEndpointConfig("ssh", json.RawMessage(ok)); len(v) != 0 {
		t.Fatalf("valid ssh config rejected: %+v", v)
	}
	// minimal: auth_mode + port optional
	if v := ValidateEndpointConfig("ssh", json.RawMessage(`{"ssh_user":"ops","known_hosts":"x"}`)); len(v) != 0 {
		t.Fatalf("minimal ssh config rejected: %+v", v)
	}
	cases := []struct {
		name  string
		in    string
		field string
		code  string
	}{
		{"missing user", `{"known_hosts":"x"}`, "endpoint_config.ssh_user", "required"},
		{"empty user", `{"ssh_user":"  ","known_hosts":"x"}`, "endpoint_config.ssh_user", "required"},
		{"missing known_hosts", `{"ssh_user":"ops"}`, "endpoint_config.known_hosts", "required"},
		{"bad auth_mode", `{"ssh_user":"ops","known_hosts":"x","auth_mode":"magic"}`, "endpoint_config.auth_mode", "invalid_value"},
		{"unknown key", `{"ssh_user":"ops","known_hosts":"x","sudo":true}`, "endpoint_config.sudo", "unknown_key"},
		{"port too big", `{"ssh_user":"ops","known_hosts":"x","port":70000}`, "endpoint_config.port", "invalid_value"},
	}
	for _, tc := range cases {
		v := ValidateEndpointConfig("ssh", json.RawMessage(tc.in))
		if !hasViolation(v, tc.field, tc.code) {
			t.Errorf("%s: want %s/%s, got %+v", tc.name, tc.field, tc.code, v)
		}
	}
}

func TestValidateCeiling_SSH(t *testing.T) {
	ok := `{"hosts_allow":["staging-*","prod-web-*"],"hosts_deny":["prod-db*"],"cert_ttl_minutes":30,"max_concurrent_sessions":4,"exec_timeout_seconds":120,"egress_class":"internal"}`
	if v := ValidateCeiling("ssh", json.RawMessage(ok)); len(v) != 0 {
		t.Fatalf("valid ssh ceiling rejected: %+v", v)
	}
	cases := []struct {
		name  string
		in    string
		field string
		code  string
	}{
		{"missing hosts_allow", `{"cert_ttl_minutes":30}`, "policy_ceiling.hosts_allow", "required"},
		{"empty hosts_allow", `{"hosts_allow":[]}`, "policy_ceiling.hosts_allow", "required"},
		{"ttl too low", `{"hosts_allow":["a"],"cert_ttl_minutes":5}`, "policy_ceiling.cert_ttl_minutes", "invalid_value"},
		{"ttl too high", `{"hosts_allow":["a"],"cert_ttl_minutes":120}`, "policy_ceiling.cert_ttl_minutes", "invalid_value"},
		{"sessions too high", `{"hosts_allow":["a"],"max_concurrent_sessions":99}`, "policy_ceiling.max_concurrent_sessions", "invalid_value"},
		{"timeout too high", `{"hosts_allow":["a"],"exec_timeout_seconds":600}`, "policy_ceiling.exec_timeout_seconds", "invalid_value"},
		{"unknown key", `{"hosts_allow":["a"],"allow_scp":true}`, "policy_ceiling.allow_scp", "unknown_key"},
		{"bad egress class", `{"hosts_allow":["a"],"egress_class":"wild"}`, "policy_ceiling.egress_class", "invalid_value"},
	}
	for _, tc := range cases {
		v := ValidateCeiling("ssh", json.RawMessage(tc.in))
		if !hasViolation(v, tc.field, tc.code) {
			t.Errorf("%s: want %s/%s, got %+v", tc.name, tc.field, tc.code, v)
		}
	}
}

func TestValidateGrant_SSHNoEscalation(t *testing.T) {
	ceiling := json.RawMessage(`{"hosts_allow":["staging-*","prod-web-*"],"command_allow":["ls*","journalctl*"],"cert_ttl_minutes":30,"max_concurrent_sessions":4,"exec_timeout_seconds":120,"egress_class":"internal"}`)

	// Narrower grant passes.
	good := json.RawMessage(`{"hosts_allow":["staging-*"],"command_allow":["ls*"],"cert_ttl_minutes":15,"max_concurrent_sessions":2,"exec_timeout_seconds":60}`)
	if v := ValidateGrant("ssh", good, ceiling); len(v) != 0 {
		t.Fatalf("narrow ssh grant rejected: %+v", v)
	}

	// Widening host escapes the ceiling.
	bad := json.RawMessage(`{"hosts_allow":["prod-db*"]}`)
	if v := ValidateGrant("ssh", bad, ceiling); !hasViolationField(v, "constraints.hosts_allow") {
		t.Fatalf("host escalation accepted: %+v", v)
	}

	// Widening command_allow rejected.
	bad = json.RawMessage(`{"command_allow":["rm *"]}`)
	if v := ValidateGrant("ssh", bad, ceiling); !hasViolationField(v, "constraints.command_allow") {
		t.Fatalf("command escalation accepted: %+v", v)
	}

	// Looser numerics rejected.
	bad = json.RawMessage(`{"cert_ttl_minutes":45,"exec_timeout_seconds":300}`)
	if v := ValidateGrant("ssh", bad, ceiling); !hasViolationField(v, "constraints.cert_ttl_minutes") || !hasViolationField(v, "constraints.exec_timeout_seconds") {
		t.Fatalf("numeric escalation accepted: %+v", v)
	}

	// Adding deny is always allowed (tightening).
	tighter := json.RawMessage(`{"hosts_allow":["staging-*"],"hosts_deny":["staging-canary*"]}`)
	if v := ValidateGrant("ssh", tighter, ceiling); len(v) != 0 {
		t.Fatalf("deny-tightening grant rejected: %+v", v)
	}

	// egress_class mismatch rejected.
	bad = json.RawMessage(`{"egress_class":"public"}`)
	if v := ValidateGrant("ssh", bad, ceiling); !hasViolationField(v, "constraints.egress_class") {
		t.Fatalf("egress_class escalation accepted: %+v", v)
	}

	// Unknown constraint key rejected.
	if v := ValidateGrant("ssh", json.RawMessage(`{"allow_port_forward":true}`), ceiling); !hasViolationField(v, "constraints.allow_port_forward") {
		t.Fatalf("unknown constraint accepted: %+v", v)
	}
}

func hasViolation(v Violations, field, code string) bool {
	for _, x := range v {
		if x.Field == field && x.Code == code {
			return true
		}
	}
	return false
}

func hasViolationField(v Violations, field string) bool {
	for _, x := range v {
		if x.Field == field {
			return true
		}
	}
	return false
}
