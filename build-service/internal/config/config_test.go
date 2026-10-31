package config

import (
	"os"
	"path/filepath"
	"testing"
)

const validYAML = `
listen: ":8080"
namespace: skquad-builds
kaniko_image: gcr.io/kaniko-project/executor:v1.23.2
registry_secret: agent-builds-registry-creds
principals_file: /etc/build-service/principals.json
cp_policy_url: http://skquad-api.skquad-system.svc.cluster.local
cp_policy_ttl_seconds: 30
destinations_allow:
  - "ghcr.io/rossbrigoli/agent-builds/"
git_hosts_allow:
  - github.com
limits:
  max_concurrent_per_principal: 2
  max_per_hour_per_principal: 20
job:
  active_deadline_seconds: 3600
  ttl_seconds_after_finished: 604800
  cpu_request: "500m"
  memory_request: 2Gi
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	c, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if c.Namespace != "skquad-builds" || c.KanikoServiceAccount != "skquad-kaniko" {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestLoadRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		drop string
	}{
		{"no namespace", "namespace: skquad-builds\n"},
		{"no kaniko image", "kaniko_image: gcr.io/kaniko-project/executor:v1.23.2\n"},
		{"no destinations", "destinations_allow:\n  - \"ghcr.io/rossbrigoli/agent-builds/\"\n"},
		{"no git hosts", "git_hosts_allow:\n  - github.com\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := replaceOnce(t, validYAML, tc.drop, "")
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestDestinationsMustEndWithSlash(t *testing.T) {
	body := replaceOnce(t, validYAML,
		`  - "ghcr.io/rossbrigoli/agent-builds/"`,
		`  - "ghcr.io/rossbrigoli"`)
	if _, err := Load(write(t, body)); err == nil {
		t.Fatal("destination prefix without trailing slash must be rejected")
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	if _, err := Load(write(t, validYAML+"\nsurprise: 1\n")); err == nil {
		t.Fatal("strict parse must reject unknown keys")
	}
}

func replaceOnce(t *testing.T, s, old, repl string) string {
	t.Helper()
	i := indexOf(s, old)
	if i < 0 {
		t.Fatalf("fixture missing %q", old)
	}
	return s[:i] + repl + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
