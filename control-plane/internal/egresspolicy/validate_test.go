package egresspolicy

import (
	"encoding/json"
	"testing"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func codes(vs Violations) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Field+"/"+v.Code)
	}
	return out
}

func TestValidateEndpointConfig_Web(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr []string
	}{
		{"empty", `{}`, nil},
		{"full valid", `{"deny_domains":["evil.com"],"deny_cidrs":["169.254.169.254/32"],"rate_per_min":30,"max_bytes":1048576,"allow_private_network":false}`, nil},
		{"unknown key", `{"deny_domains":[],"bogus":1}`, []string{"endpoint_config.bogus/unknown_key"}},
		{"bad cidr", `{"deny_cidrs":["not-a-cidr"]}`, []string{"endpoint_config.deny_cidrs[0]/invalid_value"}},
		{"zero rate", `{"rate_per_min":0}`, []string{"endpoint_config.rate_per_min/invalid_value"}},
		{"negative max", `{"max_bytes":-5}`, []string{"endpoint_config.max_bytes/invalid_value"}},
		{"timeout valid", `{"timeout_seconds":45}`, nil},
		{"timeout zero", `{"timeout_seconds":0}`, []string{"endpoint_config.timeout_seconds/invalid_value"}},
		{"timeout over cap", `{"timeout_seconds":301}`, []string{"endpoint_config.timeout_seconds/invalid_value"}},
		{"wrong type", `{"deny_domains":"evil.com"}`, []string{"endpoint_config.deny_domains/invalid_type"}},
		{"not object", `[1,2]`, []string{"endpoint_config/invalid_json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateEndpointConfig("web", raw(tc.raw))
			if got := codes(v); len(got) != len(tc.wantErr) {
				t.Fatalf("got %v want %v", got, tc.wantErr)
			} else if len(got) > 0 && got[0] != tc.wantErr[0] {
				t.Fatalf("got %v want %v", got, tc.wantErr)
			}
		})
	}
}

func TestValidateEndpointConfig_Rest(t *testing.T) {
	if v := ValidateEndpointConfig("rest", raw(`{"base_url":"https://api.example.com/v2","auth_kind":"bearer"}`)); len(v) != 0 {
		t.Fatalf("valid rest config rejected: %v", v)
	}
	if v := ValidateEndpointConfig("rest", raw(`{}`)); len(v) == 0 || v[0].Code != "required" {
		t.Fatalf("missing base_url must be required, got %v", v)
	}
	if v := ValidateEndpointConfig("rest", raw(`{"base_url":"ftp://x"}`)); len(v) == 0 || v[0].Code != "invalid_value" {
		t.Fatalf("bad scheme rejected wrong: %v", v)
	}
	if v := ValidateEndpointConfig("rest", raw(`{"base_url":"https://x","auth_kind":"ntlm"}`)); len(v) == 0 || v[0].Code != "invalid_value" {
		t.Fatalf("bad auth_kind: %v", v)
	}
	if v := ValidateEndpointConfig("rest", raw(`{"base_url":"https://x","header_name":"X-Api-Key"}`)); len(v) == 0 || v[0].Code != "invalid_value" {
		t.Fatalf("header_name without api_key_header must fail: %v", v)
	}
	if v := ValidateEndpointConfig("rest", raw(`{"base_url":"https://x","auth_kind":"api_key_header","header_name":"X-Api-Key"}`)); len(v) != 0 {
		t.Fatalf("api_key_header with header_name must pass: %v", v)
	}
	if v := ValidateEndpointConfig("rest", raw(`{"base_url":"https://x","secret_ref":"k8s://s"}`)); len(v) == 0 || v[0].Code != "unknown_key" {
		t.Fatalf("secret_ref must be unknown in endpoint_config (secrets live in auth_ref): %v", v)
	}
}

func TestValidateCeiling_RestMCPGit(t *testing.T) {
	if v := ValidateCeiling("rest", raw(`{"methods":["GET","POST"],"path_allow":["/issues/**"],"path_deny":["/admin/**"],"max_request_bytes":65536,"max_response_bytes":262144,"rate_per_min":60,"egress_class":"public"}`)); len(v) != 0 {
		t.Fatalf("valid rest ceiling rejected: %v", v)
	}
	if v := ValidateCeiling("rest", raw(`{"methods":["BREW"]}`)); len(v) == 0 || v[0].Code != "invalid_value" {
		t.Fatalf("bad method: %v", v)
	}
	if v := ValidateCeiling("rest", raw(`{"egress_class":"private"}`)); len(v) == 0 || v[0].Code != "invalid_value" {
		t.Fatalf("bad egress_class: %v", v)
	}
	if v := ValidateCeiling("mcp", raw(`{"tools_allow":["list_pulls"],"tools_deny":["*_delete"],"per_tool":{"create_issue":{"requires_confirmation":true}},"rate_per_min":30,"max_args_bytes":32768}`)); len(v) != 0 {
		t.Fatalf("valid mcp ceiling rejected: %v", v)
	}
	if v := ValidateCeiling("mcp", raw(`{"per_tool":{"x":{"confirm":true}}}`)); len(v) == 0 || v[0].Code != "unknown_key" {
		t.Fatalf("bad per_tool key: %v", v)
	}
	if v := ValidateCeiling("git", raw(`{"repos_allow":["org/*"],"allow_push":false,"rate_per_min":10}`)); len(v) != 0 {
		t.Fatalf("valid git ceiling rejected: %v", v)
	}
	if v := ValidateCeiling("git", raw(`{"repos_allow":["org/*"],"force_push":true}`)); len(v) == 0 || v[0].Code != "unknown_key" {
		t.Fatalf("unknown git ceiling key: %v", v)
	}
	// Legacy types accept anything (no typed semantics).
	if v := ValidateCeiling("skill", raw(`{"whatever":true}`)); len(v) != 0 {
		t.Fatalf("legacy type must not be shape-checked: %v", v)
	}
}
