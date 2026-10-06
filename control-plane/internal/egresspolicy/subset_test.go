package egresspolicy

import (
	"encoding/json"
	"math/rand"
	"testing"
)

func TestValidateGrant_Rest(t *testing.T) {
	ceiling := raw(`{"methods":["GET","POST"],"path_allow":["/issues/**","/search"],"path_deny":["/admin/**"],"max_request_bytes":65536,"max_response_bytes":262144,"rate_per_min":60,"egress_class":"public"}`)
	cases := []struct {
		name        string
		constraints string
		wantOK      bool
		wantCode    string
		wantField   string
	}{
		{"empty inherits", `{}`, true, "", ""},
		{"equal", `{"methods":["GET","POST"],"path_allow":["/issues/**","/search"],"path_deny":["/admin/**"],"max_request_bytes":65536,"max_response_bytes":262144,"rate_per_min":60,"egress_class":"public"}`, true, "", ""},
		{"subset methods+paths", `{"methods":["GET"],"path_allow":["/issues/123"],"rate_per_min":10}`, true, "", ""},
		{"superset method", `{"methods":["GET","POST","DELETE"]}`, false, "not_subset", "constraints.methods"},
		{"superset path", `{"path_allow":["/issues/**","/search","/other"]}`, false, "not_subset", "constraints.path_allow"},
		{"wildcard over explicit", `{"path_allow":["/admin/x"]}`, false, "not_subset", "constraints.path_allow"},
		{"explicit under wildcard ok", `{"path_allow":["/issues/5/comments"]}`, true, "", ""},
		{"rate above ceiling", `{"rate_per_min":61}`, false, "exceeds_ceiling", "constraints.rate_per_min"},
		{"rate equal ok", `{"rate_per_min":60}`, true, "", ""},
		{"size above", `{"max_response_bytes":262145}`, false, "exceeds_ceiling", "constraints.max_response_bytes"},
		{"size below ok", `{"max_request_bytes":1024}`, true, "", ""},
		{"egress class mismatch", `{"egress_class":"internal"}`, false, "mismatch", "constraints.egress_class"},
		{"denylist shrink", `{"path_deny":[]}`, false, "denylist_shrunk", "constraints.path_deny"},
		{"denylist grow ok", `{"path_deny":["/admin/**","/secret/**"]}`, true, "", ""},
		{"unknown key", `{"max_rps":5}`, false, "unknown_key", "constraints.max_rps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateGrant("rest", raw(tc.constraints), ceiling)
			if tc.wantOK {
				if len(v) != 0 {
					t.Fatalf("expected pass, got %v", v)
				}
				return
			}
			if len(v) == 0 {
				t.Fatalf("expected violation %s/%s, got none", tc.wantField, tc.wantCode)
			}
			found := false
			for _, x := range v {
				if x.Field == tc.wantField && x.Code == tc.wantCode {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %s/%s in %v", tc.wantField, tc.wantCode, v)
			}
		})
	}
}

func TestValidateGrant_MCP(t *testing.T) {
	ceiling := raw(`{"tools_allow":["list_pulls","get_issue","create_issue"],"tools_deny":["merge_pull","*_delete"],"per_tool":{"create_issue":{"requires_confirmation":true}},"rate_per_min":30,"max_args_bytes":32768}`)
	cases := []struct {
		name        string
		constraints string
		wantOK      bool
		wantCode    string
		wantField   string
	}{
		{"empty", `{}`, true, "", ""},
		{"subset tools", `{"tools_allow":["list_pulls"]}`, true, "", ""},
		{"new tool", `{"tools_allow":["drop_table"]}`, false, "not_subset", "constraints.tools_allow"},
		{"deny shrink", `{"tools_deny":["merge_pull"]}`, false, "denylist_shrunk", "constraints.tools_deny"},
		{"deny grow ok", `{"tools_deny":["merge_pull","*_delete","get_issue"]}`, true, "", ""},
		{"confirm tighten ok", `{"per_tool":{"list_pulls":{"requires_confirmation":true}}}`, true, "", ""},
		{"confirm loosen denied", `{"per_tool":{"create_issue":{"requires_confirmation":false}}}`, false, "exceeds_ceiling", "constraints.per_tool.create_issue.requires_confirmation"},
		{"per_tool unknown tool", `{"per_tool":{"mystery":{"requires_confirmation":true}}}`, false, "not_subset", "constraints.per_tool.mystery"},
		{"args too big", `{"max_args_bytes":32769}`, false, "exceeds_ceiling", "constraints.max_args_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateGrant("mcp", raw(tc.constraints), ceiling)
			if tc.wantOK && len(v) != 0 {
				t.Fatalf("expected pass, got %v", v)
			}
			if !tc.wantOK {
				found := false
				for _, x := range v {
					if x.Field == tc.wantField && x.Code == tc.wantCode {
						found = true
					}
				}
				if !found {
					t.Fatalf("expected %s/%s in %v", tc.wantField, tc.wantCode, v)
				}
			}
		})
	}
}

func TestValidateGrant_WebAndGit(t *testing.T) {
	webCeiling := raw(`{"deny_domains":["bad.com"],"deny_cidrs":["10.0.0.0/8"],"rate_per_min":60,"max_bytes":1048576,"allow_private_network":false}`)
	if v := ValidateGrant("web", raw(`{"deny_domains":["bad.com","worse.com"],"rate_per_min":30}`), webCeiling); len(v) != 0 {
		t.Fatalf("web tighten should pass: %v", v)
	}
	if v := ValidateGrant("web", raw(`{"deny_domains":[]}`), webCeiling); len(v) == 0 || v[0].Code != "denylist_shrunk" {
		t.Fatalf("web deny shrink must fail: %v", v)
	}
	if v := ValidateGrant("web", raw(`{"allow_private_network":true}`), webCeiling); len(v) == 0 || v[0].Code != "exceeds_ceiling" {
		t.Fatalf("private network beyond ceiling must fail: %v", v)
	}
	// timeout_seconds: grant may tighten but not exceed the ceiling.
	webCeilingTO := raw(`{"timeout_seconds":60}`)
	if v := ValidateGrant("web", raw(`{"timeout_seconds":30}`), webCeilingTO); len(v) != 0 {
		t.Fatalf("timeout tighten should pass: %v", v)
	}
	if v := ValidateGrant("web", raw(`{"timeout_seconds":90}`), webCeilingTO); len(v) == 0 || v[0].Code != "exceeds_ceiling" {
		t.Fatalf("timeout beyond ceiling must fail: %v", v)
	}
	if v := ValidateGrant("web", raw(`{"timeout_seconds":30}`), webCeiling); len(v) == 0 || v[0].Code != "exceeds_ceiling" {
		t.Fatalf("timeout set without ceiling must fail: %v", v)
	}
	gitCeiling := raw(`{"repos_allow":["org/repo1","org/*"],"allow_push":false}`)
	if v := ValidateGrant("git", raw(`{"repos_allow":["org/repo1"]}`), gitCeiling); len(v) != 0 {
		t.Fatalf("git subset should pass: %v", v)
	}
	if v := ValidateGrant("git", raw(`{"repos_allow":["evil/repo"]}`), gitCeiling); len(v) == 0 || v[0].Code != "not_subset" {
		t.Fatalf("git outside ceiling must fail: %v", v)
	}
	if v := ValidateGrant("git", raw(`{"allow_push":true}`), gitCeiling); len(v) == 0 || v[0].Code != "exceeds_ceiling" {
		t.Fatalf("push beyond ceiling must fail: %v", v)
	}
	if v := ValidateGrant("git", raw(`{"repos_allow":["org/*"],"allow_push":true}`), raw(`{"repos_allow":["org/*"],"allow_push":true}`)); len(v) != 0 {
		t.Fatalf("equal push ceiling should pass: %v", v)
	}
}

func TestValidateGrant_InvalidCeilingIsViolation(t *testing.T) {
	v := ValidateGrant("rest", raw(`{}`), raw(`{"methods":["BREW"]}`))
	if len(v) == 0 {
		t.Fatal("invalid ceiling must produce violations")
	}
	// ceiling violations are prefixed so callers can tell which side broke
	if v[0].Field != "ceiling.policy_ceiling.methods[0]" {
		t.Fatalf("expected ceiling-prefixed field, got %v", v)
	}
}

func TestGlobCovers(t *testing.T) {
	cases := []struct {
		pattern, candidate string
		want               bool
	}{
		{"a", "a", true},
		{"a/**", "a/b", true},
		{"a/**", "a/b/c", true},
		{"a/**", "a", false},
		{"a/*", "a/b", true},
		{"a/*", "a/b/c", false},
		{"a*", "ab", true},
		{"get_*", "get_issue", true},
		{"get_*", "set_issue", false},
		{"issues/**", "issues/1", true},
		{"issues/1", "issues/**", false}, // wildcard candidate not covered by explicit
		{"org/*", "org/repo", true},
		{"org/**", "org/team/repo", true},
	}
	for _, tc := range cases {
		if got := globCovers(tc.pattern, tc.candidate); got != tc.want {
			t.Errorf("globCovers(%q,%q)=%v want %v", tc.pattern, tc.candidate, got, tc.want)
		}
	}
}

// TestValidateGrant_PropertyRandom is a property-style fuzz (deterministic
// seed): random ceilings are generated, then random constraints built in
// three modes — subset (must pass), equal (must pass), and superset-with-
// extra (must fail). A violation-free superset or a rejected subset means
// the lattice logic is broken.
func TestValidateGrant_PropertyRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(20261006))
	methodPool := []string{"GET", "POST", "PUT", "PATCH", "HEAD"}
	pool := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	pickFrom := func(src []string, n int) []string {
		perm := rng.Perm(len(src))
		out := make([]string, 0, n)
		for _, i := range perm[:n] {
			out = append(out, src[i])
		}
		return out
	}
	pick := func(n int) []string { return pickFrom(pool, n) }

	for iter := 0; iter < 300; iter++ {
		ceMethods := pickFrom(methodPool, 1+rng.Intn(len(methodPool)))
		ceAllow := pick(1 + rng.Intn(4))
		ceDeny := pick(rng.Intn(2))
		ceRate := 10 + rng.Intn(90)
		ceSize := 1024 * (1 + rng.Intn(64))
		ceiling := map[string]any{
			"methods":            ceMethods,
			"path_allow":         ceAllow,
			"path_deny":          ceDeny,
			"max_request_bytes":  ceSize,
			"max_response_bytes": ceSize,
			"rate_per_min":       ceRate,
			"egress_class":       "public",
		}
		ceRaw, _ := json.Marshal(ceiling)

		// Mode 1: exact copy → must pass.
		if v := ValidateGrant("rest", ceRaw, ceRaw); len(v) != 0 {
			t.Fatalf("iter %d: equal constraints rejected: %v", iter, v)
		}

		// Mode 2: strict subset (fewer allows, tighter numbers, grown deny) → must pass.
		sub := map[string]any{
			"methods":           ceMethods[:1],
			"path_allow":        ceAllow[:1],
			"path_deny":         append(append([]string{}, ceDeny...), "extra-deny"),
			"rate_per_min":      ceRate - 1,
			"max_request_bytes": ceSize - 1,
			"egress_class":      "public",
		}
		subRaw, _ := json.Marshal(sub)
		if v := ValidateGrant("rest", subRaw, ceRaw); len(v) != 0 {
			t.Fatalf("iter %d: subset rejected: %v", iter, v)
		}

		// Mode 3: superset (extra allow entry) → must fail with not_subset.
		super := map[string]any{
			"methods":    append(append([]string{}, ceMethods...), "OPTIONS"),
			"path_allow": append(append([]string{}, ceAllow...), "omega"),
		}
		superRaw, _ := json.Marshal(super)
		v := ValidateGrant("rest", superRaw, ceRaw)
		found := false
		for _, x := range v {
			if x.Code == "not_subset" {
				found = true
			}
		}
		if !found {
			t.Fatalf("iter %d: superset not caught: %v", iter, v)
		}

		// Mode 4: rate one over ceiling → must fail exceeds_ceiling.
		over := map[string]any{"rate_per_min": ceRate + 1}
		overRaw, _ := json.Marshal(over)
		v = ValidateGrant("rest", overRaw, ceRaw)
		found = false
		for _, x := range v {
			if x.Code == "exceeds_ceiling" && x.Field == "constraints.rate_per_min" {
				found = true
			}
		}
		if !found {
			t.Fatalf("iter %d: rate overflow not caught: %v", iter, v)
		}
	}
}
