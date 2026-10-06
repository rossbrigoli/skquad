// TG-6 slice D: unit tests for the browser-driver ceiling shape —
// validation, default materialization, and the no-escalation grant
// fold. Field names/bounds mirror
// tool-gateway/internal/drivers/browser/policy.go (authoritative).
package egresspolicy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsBrowserConfig(t *testing.T) {
	require.True(t, IsBrowserConfig(json.RawMessage(`{"url":"http://x/mcp","driver":"browser"}`)))
	require.True(t, IsBrowserConfig(json.RawMessage(`{"driver":"Browser"}`)), "selection is case-insensitive (gateway EqualFold)")
	require.True(t, IsBrowserConfig(json.RawMessage(`{"driver":" browser "}`)))
	require.False(t, IsBrowserConfig(json.RawMessage(`{"url":"http://x/mcp"}`)))
	require.False(t, IsBrowserConfig(json.RawMessage(`{"driver":"mcp"}`)))
	require.False(t, IsBrowserConfig(nil))
	require.False(t, IsBrowserConfig(json.RawMessage(`"not-an-object"`)))
}

func TestValidateBrowserCeilingHappy(t *testing.T) {
	v := ValidateBrowserCeiling(json.RawMessage(`{
		"deny_hosts": ["*.internal", "metadata.google.internal"],
		"max_pages": 25,
		"max_screenshot_bytes": 1048576,
		"idle_timeout_s": 300,
		"max_session_minutes": 15,
		"max_sessions_per_agent": 2
	}`))
	require.Empty(t, v)

	// Empty object is legal (defaults fold at the gateway anyway).
	require.Empty(t, ValidateBrowserCeiling(json.RawMessage(`{}`)))
	require.Empty(t, ValidateBrowserCeiling(nil))
}

func TestValidateBrowserCeilingRejects(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		field string
		code  string
	}{
		{"unknown_key", `{"max_loops": 3}`, "policy_ceiling.max_loops", "unknown_key"},
		{"mcp_ceiling_key_rejected", `{"tools_allow": ["browser.navigate"]}`, "policy_ceiling.tools_allow", "unknown_key"},
		{"max_pages_too_high", `{"max_pages": 501}`, "policy_ceiling.max_pages", "invalid_value"},
		{"max_pages_zero", `{"max_pages": 0}`, "policy_ceiling.max_pages", "invalid_value"},
		{"session_minutes_too_high", `{"max_session_minutes": 241}`, "policy_ceiling.max_session_minutes", "invalid_value"},
		{"sessions_zero", `{"max_sessions_per_agent": 0}`, "policy_ceiling.max_sessions_per_agent", "invalid_value"},
		{"screenshot_bytes_string", `{"max_screenshot_bytes": "2MB"}`, "policy_ceiling.max_screenshot_bytes", "invalid_type"},
		{"deny_hosts_not_array", `{"deny_hosts": "*.internal"}`, "policy_ceiling.deny_hosts", "invalid_type"},
		{"deny_hosts_empty_entry", `{"deny_hosts": ["ok.example", "  "]}`, "policy_ceiling.deny_hosts[1]", "invalid_value"},
		{"idle_timeout_negative", `{"idle_timeout_s": -1}`, "policy_ceiling.idle_timeout_s", "invalid_value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateBrowserCeiling(json.RawMessage(tc.raw))
			require.NotEmpty(t, v)
			var found bool
			for _, x := range v {
				if x.Field == tc.field && x.Code == tc.code {
					found = true
				}
			}
			require.True(t, found, "expected violation %s/%s, got %v", tc.field, tc.code, v)
		})
	}
}

func TestNormalizeBrowserCeiling(t *testing.T) {
	out, v := NormalizeBrowserCeiling(json.RawMessage(`{"max_pages": 10}`))
	require.Empty(t, v)
	var ce map[string]any
	require.NoError(t, json.Unmarshal(out, &ce))
	require.Equal(t, float64(10), ce["max_pages"], "provided value preserved")
	require.Equal(t, float64(BrowserDefaultMaxScreenshotBytes), ce["max_screenshot_bytes"])
	require.Equal(t, float64(BrowserDefaultIdleTimeoutS), ce["idle_timeout_s"])
	require.Equal(t, float64(BrowserDefaultMaxSessionMinutes), ce["max_session_minutes"])
	require.Equal(t, float64(BrowserDefaultMaxSessionsPerAgent), ce["max_sessions_per_agent"])
	require.Equal(t, []any{}, ce["deny_hosts"], "deny_hosts defaults to []")
	require.Len(t, ce, len(BrowserCeilingKeys), "normalization emits exactly the browser shape")

	// Full ceiling passes through unchanged in values.
	full := `{"deny_hosts":["a.example"],"max_pages":7,"max_screenshot_bytes":4096,"idle_timeout_s":60,"max_session_minutes":5,"max_sessions_per_agent":3}`
	out2, v2 := NormalizeBrowserCeiling(json.RawMessage(full))
	require.Empty(t, v2)
	var ce2 map[string]any
	require.NoError(t, json.Unmarshal(out2, &ce2))
	require.Equal(t, []any{"a.example"}, ce2["deny_hosts"])
	require.Equal(t, float64(7), ce2["max_pages"])
	require.Equal(t, float64(4096), ce2["max_screenshot_bytes"])
	require.Equal(t, float64(60), ce2["idle_timeout_s"])
	require.Equal(t, float64(5), ce2["max_session_minutes"])
	require.Equal(t, float64(3), ce2["max_sessions_per_agent"])

	// Invalid input surfaces violations, never a silent default.
	_, v3 := NormalizeBrowserCeiling(json.RawMessage(`{"bogus": 1}`))
	require.NotEmpty(t, v3)
}

func TestValidateBrowserGrantNarrowsOK(t *testing.T) {
	ceiling := json.RawMessage(`{"deny_hosts":["*.internal"],"max_pages":100,"max_screenshot_bytes":2097152,"idle_timeout_s":600,"max_session_minutes":120,"max_sessions_per_agent":4}`)
	good := json.RawMessage(`{"max_pages":10,"max_session_minutes":30,"max_sessions_per_agent":1,"deny_hosts":["*.internal","bad.example"]}`)
	require.Empty(t, ValidateBrowserGrant(good, ceiling))

	// Absent fields inherit — always safe.
	require.Empty(t, ValidateBrowserGrant(json.RawMessage(`{}`), ceiling))
}

func TestValidateBrowserGrantWideningRejected(t *testing.T) {
	ceiling := json.RawMessage(`{"deny_hosts":["*.internal"],"max_pages":100,"max_screenshot_bytes":2097152,"idle_timeout_s":600,"max_session_minutes":120,"max_sessions_per_agent":4}`)

	bad := ValidateBrowserGrant(json.RawMessage(`{"max_pages":200}`), ceiling)
	require.Len(t, bad, 1)
	require.Equal(t, "constraints.max_pages", bad[0].Field)
	require.Equal(t, "exceeds_ceiling", bad[0].Code)

	bad = ValidateBrowserGrant(json.RawMessage(`{"max_sessions_per_agent":5}`), ceiling)
	require.Len(t, bad, 1)
	require.Equal(t, "constraints.max_sessions_per_agent", bad[0].Field)

	// Dropping a ceiling denial is escalation (denylists only grow).
	bad = ValidateBrowserGrant(json.RawMessage(`{"deny_hosts":["extra.example"]}`), ceiling)
	require.Len(t, bad, 1)
	require.Equal(t, "constraints.deny_hosts", bad[0].Field)
	require.Equal(t, "denylist_shrunk", bad[0].Code)

	// Unknown keys rejected on the constraint side too.
	bad = ValidateBrowserGrant(json.RawMessage(`{"max_tabs":9}`), ceiling)
	require.NotEmpty(t, bad)
	require.Equal(t, "unknown_key", bad[0].Code)

	// Broken ceiling reported with the ceiling. prefix (mirrors ValidateGrant).
	bad = ValidateBrowserGrant(json.RawMessage(`{"max_pages":5}`), json.RawMessage(`{"max_pages":-3}`))
	require.NotEmpty(t, bad)
	require.Contains(t, bad[0].Field, "ceiling.")
}

func TestMCPConfigDriverValidation(t *testing.T) {
	// browser + mcp driver values accepted; anything else rejected.
	require.Empty(t, ValidateEndpointConfig("mcp", json.RawMessage(`{"url":"http://skquad-browser-service.skquad-browser.svc.cluster.local:8090/mcp","driver":"browser"}`)))
	require.Empty(t, ValidateEndpointConfig("mcp", json.RawMessage(`{"url":"https://x.example/mcp","auth_kind":"bearer","driver":"mcp"}`)))

	v := ValidateEndpointConfig("mcp", json.RawMessage(`{"url":"https://x.example/mcp","driver":"firefox"}`))
	require.Len(t, v, 1)
	require.Equal(t, "endpoint_config.driver", v[0].Field)
	require.Equal(t, "invalid_value", v[0].Code)

	// Browser config still enforces the url rule.
	v = ValidateEndpointConfig("mcp", json.RawMessage(`{"url":"ftp://x/mcp","driver":"browser"}`))
	require.NotEmpty(t, v)

	// Unknown config keys rejected.
	v = ValidateEndpointConfig("mcp", json.RawMessage(`{"url":"https://x/mcp","driver":"browser","engine":"chromium"}`))
	require.NotEmpty(t, v)
	require.Equal(t, "unknown_key", v[0].Code)
}
