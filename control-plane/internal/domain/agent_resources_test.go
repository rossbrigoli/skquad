package domain

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// S-287: agent pod resource quantity parsing, validation, resolution.

func TestParseCPUQuantityValid(t *testing.T) {
	for raw, want := range map[string]int64{
		"100m":  100,
		"500m":  500,
		"1":     1000,
		"2":     2000,
		"0.5":   500,
		"1.25":  1250,
		"0.1":   100,
		"1000m": 1000,
	} {
		got, err := ParseCPUQuantity(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
}

func TestParseCPUQuantityInvalid(t *testing.T) {
	for _, raw := range []string{
		"", " ", "abc", "100x", "-100m", "0", "0m", "0.0", "1Gi", "m", "100 mm",
		"1e3", ".5", "1.2.3", " 100m", "100m ", "+100m",
	} {
		_, err := ParseCPUQuantity(raw)
		require.Error(t, err, raw)
	}
}

func TestValidateAgentResourcesOK(t *testing.T) {
	res, err := ValidateAgentResources("100m", "500m", "128Mi", "512Mi")
	require.NoError(t, err)
	require.Equal(t, AgentResourceSettings{
		CPURequest: "100m", CPULimit: "500m",
		MemoryRequest: "128Mi", MemoryLimit: "512Mi",
	}, res)

	// Equal request/limit is valid; mixed formats too.
	_, err = ValidateAgentResources("1", "1000m", "1Gi", "1024Mi")
	require.NoError(t, err)
}

func TestValidateAgentResourcesErrors(t *testing.T) {
	cases := []struct {
		name                           string
		cpuReq, cpuLim, memReq, memLim string
		contains                       string
	}{
		{"cpu request unparsable", "banana", "500m", "128Mi", "512Mi", "cpu_request"},
		{"cpu limit unparsable", "100m", "500x", "128Mi", "512Mi", "cpu_limit"},
		{"cpu request over limit", "600m", "500m", "128Mi", "512Mi", "exceeds cpu_limit"},
		{"mem request unparsable", "100m", "500m", "128Q", "512Mi", "memory_request"},
		{"mem limit unparsable", "100m", "500m", "128Mi", "512?", "memory_limit"},
		{"mem request over limit", "100m", "500m", "2Gi", "512Mi", "exceeds memory_limit"},
		{"empty cpu request", "", "500m", "128Mi", "512Mi", "cpu_request"},
	}
	for _, tc := range cases {
		_, err := ValidateAgentResources(tc.cpuReq, tc.cpuLim, tc.memReq, tc.memLim)
		require.Error(t, err, tc.name)
		require.Contains(t, err.Error(), tc.contains, tc.name)
	}
}

func TestResolveAgentResourcesDefaults(t *testing.T) {
	get := func(_ context.Context, _ string) (string, bool, error) { return "", false, nil }
	require.Equal(t, DefaultAgentResources(), ResolveAgentResources(context.Background(), get))
}

func TestResolveAgentResourcesStoredWins(t *testing.T) {
	values := map[string]string{
		PlatformSettingAgentCPURequest:    "250m",
		PlatformSettingAgentCPULimit:      "750m",
		PlatformSettingAgentMemoryRequest: "256Mi",
		PlatformSettingAgentMemoryLimit:   "1Gi",
	}
	get := func(_ context.Context, key string) (string, bool, error) {
		v, ok := values[key]
		return v, ok, nil
	}
	res := ResolveAgentResources(context.Background(), get)
	require.Equal(t, AgentResourceSettings{
		CPURequest: "250m", CPULimit: "750m",
		MemoryRequest: "256Mi", MemoryLimit: "1Gi",
	}, res)
}

func TestResolveAgentResourcesBlankFallsBack(t *testing.T) {
	get := func(_ context.Context, key string) (string, bool, error) {
		if key == PlatformSettingAgentCPULimit {
			return "   ", true, nil
		}
		return "", false, nil
	}
	res := ResolveAgentResources(context.Background(), get)
	require.Equal(t, DefaultAgentCPULimit, res.CPULimit)
	require.Equal(t, DefaultAgentCPURequest, res.CPURequest)
}
