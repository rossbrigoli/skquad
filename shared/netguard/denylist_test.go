package netguard

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDomainDenied(t *testing.T) {
	deny := []string{"Example.COM.", "ads.tracker.io"}
	require.True(t, DomainDenied("example.com", deny))
	require.True(t, DomainDenied("EXAMPLE.com:443", deny))
	require.True(t, DomainDenied("a.example.com", deny))
	require.True(t, DomainDenied("deep.sub.example.com", deny))
	require.True(t, DomainDenied("ads.tracker.io", deny))
	require.False(t, DomainDenied("notexample.com", deny))
	require.False(t, DomainDenied("example.org", deny))
	require.False(t, DomainDenied("tracker.io", deny))
	require.False(t, DomainDenied("example.com.evil.net", deny))
	// Empty entries never deny-all.
	require.False(t, DomainDenied("anything.example", []string{"", "  "}))
	require.False(t, DomainDenied("", deny))
}

func TestNormalizeHost(t *testing.T) {
	require.Equal(t, "example.com", NormalizeHost("Example.COM."))
	require.Equal(t, "example.com", NormalizeHost(" example.com:8080 "))
	require.Equal(t, "[::1]", NormalizeHost("[::1]:8080"))
	require.Equal(t, "[::1]", NormalizeHost("[::1]"))
}

func TestCIDRDenied(t *testing.T) {
	_, cgnat, err := net.ParseCIDR("100.64.0.0/10")
	require.NoError(t, err)
	require.True(t, CIDRDenied(net.ParseIP("100.64.5.5"), []*net.IPNet{cgnat}))
	require.False(t, CIDRDenied(net.ParseIP("8.8.8.8"), []*net.IPNet{cgnat}))
	require.False(t, CIDRDenied(nil, []*net.IPNet{cgnat}))
	require.False(t, CIDRDenied(net.ParseIP("8.8.8.8"), nil))
}
