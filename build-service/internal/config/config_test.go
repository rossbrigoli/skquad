package config

import (
	"os"
	"path/filepath"
	"testing"
)

const goodYAML = `
listen: ":8090"
namespace: skquad-builds
kaniko_image: gcr.io/kaniko-project/executor:v1.24.0
kaniko_service_account: skquad-kaniko
registry_secret: skquad-build-registry
principals_file: /etc/skquad-build/principals.json
cp_policy_url: http://skquad-api.skquad-system.svc.cluster.local
cp_policy_ttl_seconds: 60
cp_policy_timeout_seconds: 5
destinations_allow:
  - docker.io/brigss007/agent-builds/
git_hosts_allow:
  - github.com
limits:
  max_concurrent_per_principal: 1
  max_per_hour_per_principal: 4
job:
  active_deadline_seconds: 1800
  ttl_seconds_after_finished: 3600
  cpu_request: 250m
  memory_request: 512Mi
`

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadGood(t *testing.T) {
	c, err := Load(writeTmp(t, goodYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Namespace != "skquad-builds" || c.KanikoServiceAccount != "skquad-kaniko" {
		t.Fatalf("bad parse: %+v", c)
	}
	if c.CPPolicyTimeoutSeconds != 5 {
		t.Fatal("timeout not parsed")
	}
}

func TestLoadFailClosed(t *testing.T) {
	bad := []struct {
		name  string
		body  string
		match string
	}{
		{"no listen", `namespace: n
kaniko_image: i
principals_file: p
destinations_allow: ["a/"]
git_hosts_allow: ["github.com"]
limits: {max_concurrent_per_principal: 1, max_per_hour_per_principal: 1}
job: {active_deadline_seconds: 1, ttl_seconds_after_finished: 1}`, "listen"},
		{"empty destinations", `listen: ":1"
namespace: n
kaniko_image: i
principals_file: p
destinations_allow: []
git_hosts_allow: ["github.com"]
limits: {max_concurrent_per_principal: 1, max_per_hour_per_principal: 1}
job: {active_deadline_seconds: 1, ttl_seconds_after_finished: 1}`, "destinations_allow"},
		{"dest without slash", `listen: ":1"
namespace: n
kaniko_image: i
principals_file: p
destinations_allow: ["docker.io/x"]
git_hosts_allow: ["github.com"]
limits: {max_concurrent_per_principal: 1, max_per_hour_per_principal: 1}
job: {active_deadline_seconds: 1, ttl_seconds_after_finished: 1}`, "ending in '/'"},
		{"no git hosts", `listen: ":1"
namespace: n
kaniko_image: i
principals_file: p
destinations_allow: ["a/"]
git_hosts_allow: []
limits: {max_concurrent_per_principal: 1, max_per_hour_per_principal: 1}
job: {active_deadline_seconds: 1, ttl_seconds_after_finished: 1}`, "git_hosts_allow"},
		{"zero concurrency", `listen: ":1"
namespace: n
kaniko_image: i
principals_file: p
destinations_allow: ["a/"]
git_hosts_allow: ["github.com"]
limits: {max_concurrent_per_principal: 0, max_per_hour_per_principal: 1}
job: {active_deadline_seconds: 1, ttl_seconds_after_finished: 1}`, "max_concurrent"},
		{"unknown key strict", goodYAML + "\nunknown_field: 1\n", "parse"},
		{"bad cp url", `listen: ":1"
namespace: n
kaniko_image: i
principals_file: p
cp_policy_url: "ftp://x"
cp_policy_ttl_seconds: 5
destinations_allow: ["a/"]
git_hosts_allow: ["github.com"]
limits: {max_concurrent_per_principal: 1, max_per_hour_per_principal: 1}
job: {active_deadline_seconds: 1, ttl_seconds_after_finished: 1}`, "http(s)"},
		{"cp url no ttl", `listen: ":1"
namespace: n
kaniko_image: i
principals_file: p
cp_policy_url: "http://x"
destinations_allow: ["a/"]
git_hosts_allow: ["github.com"]
limits: {max_concurrent_per_principal: 1, max_per_hour_per_principal: 1}
job: {active_deadline_seconds: 1, ttl_seconds_after_finished: 1}`, "cp_policy_ttl_seconds"},
	}
	for _, c := range bad {
		_, err := Load(writeTmp(t, c.body))
		if err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
		if !contains(err.Error(), c.match) {
			t.Fatalf("%s: error %q does not mention %q", c.name, err.Error(), c.match)
		}
	}
}

func TestDefaults(t *testing.T) {
	c := &Config{
		Listen: ":1", Namespace: "n", KanikoImage: "i", PrincipalsFile: "p",
		DestinationsAllow: []string{"a/"}, GitHostsAllow: []string{"github.com"},
		Limits: Limits{MaxConcurrentPerPrincipal: 1, MaxPerHourPerPrincipal: 1},
		Job:    JobConfig{ActiveDeadlineSeconds: 1, TTLSecondsAfterFinished: 1},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.KanikoServiceAccount != "skquad-kaniko" {
		t.Fatal("default kaniko SA not applied")
	}
	if c.CPPolicyTimeoutSeconds != 5 {
		t.Fatal("default cp timeout not applied")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
