// S-283 follow-on: validate the seeded platform-browser registration
// (migration 0060_browser_activation.sql) against the CP's own
// registration validators, using the REAL artifact read from disk —
// so a migration edit that breaks the browser-driver contract fails
// here, not at a live registration.
package egresspolicy

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func readBrowserSeedSQL(t *testing.T) string {
	t.Helper()
	const path = "../storage/migrations/0060_browser_activation.sql"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	return string(raw)
}

func extractJSONLine(t *testing.T, sql, prefix string) json.RawMessage {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*('` + prefix + `.*?'),$`)
	m := re.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("no %s line found in 0060 migration", prefix)
	}
	unquoted := strings.TrimSuffix(strings.TrimPrefix(m[1], "'"), "'")
	if !json.Valid([]byte(unquoted)) {
		t.Fatalf("%s line is not valid JSON: %.120s", prefix, unquoted)
	}
	return json.RawMessage(unquoted)
}

func TestBrowserSeedPassesRegistrationValidators(t *testing.T) {
	t.Parallel()
	sql := readBrowserSeedSQL(t)

	cfg := extractJSONLine(t, sql, `{"url"`)
	ceiling := extractJSONLine(t, sql, `{"deny_hosts"`)

	if !IsBrowserConfig(cfg) {
		t.Fatal("seeded endpoint_config does not select the browser driver")
	}
	if v := ValidateEndpointConfig("mcp", cfg); len(v) > 0 {
		t.Fatalf("seeded endpoint_config rejected: %v", v)
	}
	if v := ValidateBrowserCeiling(ceiling); len(v) > 0 {
		t.Fatalf("seeded policy_ceiling rejected: %v", v)
	}
	// Empty grant constraints must satisfy the no-escalation rule.
	if v := ValidateBrowserGrant(json.RawMessage(`{}`), ceiling); len(v) > 0 {
		t.Fatalf("empty constraints violate seeded ceiling: %v", v)
	}
}
