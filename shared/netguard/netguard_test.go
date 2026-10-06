package netguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The address table must stay byte-identical in behavior to the original
// BT-6 blockedDestAddr table (control-plane webfetch_proxy_test.go).
func TestBlockedIPTable(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.0.0.53",
		"10.0.0.5", "172.16.9.9", "192.168.68.131",
		"169.254.169.254", // cloud metadata
		"100.64.0.1", "100.127.255.255", // CGNAT edges
		"fc00::1", "fd12:3456::7:80", // IPv6 ULA
		"fe80::1", // link-local
		"0.0.0.0", "::",
		"224.0.0.1", "239.1.1.1", // multicast
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		require.NotNil(t, ip, s)
		require.True(t, BlockedIP(ip), "%s must be blocked", s)
	}
	allowed := []string{
		"93.184.216.34", "8.8.8.8",
		"2606:2800:220:1:248:1893:25c8:1946",
		"2001:4860:4860::8888",
		"100.63.255.255", // just below CGNAT
		"100.128.0.0",    // just above CGNAT
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		require.NotNil(t, ip, s)
		require.False(t, BlockedIP(ip), "%s must be allowed", s)
	}
	require.True(t, BlockedIP(nil), "nil fails closed")
}

func TestBlockedHostPort(t *testing.T) {
	require.True(t, BlockedHostPort("169.254.169.254:80"))
	require.True(t, BlockedHostPort("not-an-ip:80"))
	require.True(t, BlockedHostPort("no-port"))
	require.False(t, BlockedHostPort("8.8.8.8:443"))
	require.False(t, BlockedHostPort("[2001:4860:4860::8888]:53"))
}

func TestGuardDenyCIDRsApplyEvenWithAllowPrivate(t *testing.T) {
	_, deny, err := net.ParseCIDR("192.0.2.0/24")
	require.NoError(t, err)
	g := &Guard{AllowPrivate: true, DenyCIDRs: []*net.IPNet{deny}}
	require.False(t, g.Blocked(net.ParseIP("10.1.2.3"))) // private allowed
	require.True(t, g.Blocked(net.ParseIP("192.0.2.7")))  // explicit deny wins
	require.True(t, g.Blocked(nil))
}

// stubResolver simulates DNS answers, including rebinding rotation.
type stubResolver struct {
	answers [][]net.IPAddr
	calls   int
}

func (s *stubResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	a := s.answers[s.calls%len(s.answers)]
	s.calls++
	return a, nil
}

func ipAddrs(ss ...string) []net.IPAddr {
	out := make([]net.IPAddr, 0, len(ss))
	for _, s := range ss {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out
}

func TestDialContextStrictDeniesRebindingRotation(t *testing.T) {
	// A name whose answer set contains a poisoned (private) record: the
	// strict guard denies the whole name, never dialing any answer.
	res := &stubResolver{answers: [][]net.IPAddr{
		ipAddrs("93.184.216.34", "127.0.0.1"),
	}}
	d := Dialer{Guard: &Guard{}, Resolver: res, Timeout: time.Second}
	for i := 0; i < 4; i++ {
		_, err := d.DialContext(context.Background(), "tcp", "rebind.example:80")
		require.Error(t, err, "dial %d must be denied", i)
		require.Contains(t, err.Error(), "ssrf_guard")
	}
}

func TestDialContextDeniesMetadata(t *testing.T) {
	res := &stubResolver{answers: [][]net.IPAddr{ipAddrs("169.254.169.254")}}
	d := Dialer{Guard: &Guard{}, Resolver: res}
	_, err := d.DialContext(context.Background(), "tcp", "metadata.example:80")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ssrf_guard")
}

func TestDialContextIPLiteralBlocked(t *testing.T) {
	d := Dialer{Guard: &Guard{}}
	_, err := d.DialContext(context.Background(), "tcp", "10.0.0.1:22")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ssrf_guard")
}

func TestDialContextPinsAndConnects(t *testing.T) {
	// AllowPrivate so we can pin 127.0.0.1 and actually reach the
	// httptest server through the guard — proves the pinned dial path.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		fmt.Fprint(w, "pinned-ok")
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)

	res := &stubResolver{answers: [][]net.IPAddr{ipAddrs("127.0.0.1")}}
	d := Dialer{Guard: &Guard{AllowPrivate: true}, Resolver: res, Timeout: 2 * time.Second}
	conn, err := d.DialContext(context.Background(), "tcp", "pinned.example:"+port)
	require.NoError(t, err)
	defer conn.Close()
	// Connected to the test server: issue a minimal request.
	_, _ = conn.Write([]byte("GET /probe HTTP/1.1\r\nHost: pinned.example\r\nConnection: close\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	all, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Contains(t, string(all), "pinned-ok")
	require.Equal(t, "/probe", got)
}

func TestGuardNewTransportEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()
	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)

	// Strict guard: dialing the (private) httptest host must fail.
	strict := &Guard{}
	client := &http.Client{Transport: strict.NewTransport(2 * time.Second)}
	_, err = client.Get(srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ssrf_guard")

	// AllowPrivate: works.
	open := &Guard{AllowPrivate: true}
	client = &http.Client{Transport: open.NewTransport(2 * time.Second)}
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}

func TestControlHook(t *testing.T) {
	g := &Guard{}
	hook := g.ControlHook()
	require.Error(t, hook("tcp", "169.254.169.254:80", nil))
	require.Error(t, hook("tcp", "garbage", nil))
	require.NoError(t, hook("tcp", "8.8.8.8:443", nil))

	priv := &Guard{AllowPrivate: true}
	require.NoError(t, priv.ControlHook()("tcp", "10.1.1.1:80", nil))
}

func TestRedirectCheck(t *testing.T) {
	deny := func(host string) bool { return host == "evil.example" }
	check := RedirectCheck(3, deny)
	req := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		require.NoError(t, err)
		return r
	}
	via := []*http.Request{req("http://a.example"), req("http://b.example"), req("http://c.example")}
	// Cap: with maxHops=3, a 4th redirect (len(via)==4) must stop.
	require.Equal(t, http.ErrUseLastResponse, check(req("http://d.example"), append(via, req("http://x.example"))))
	require.Error(t, check(req("http://evil.example"), via[:1]))
	require.NoError(t, check(req("https://ok.example"), via[:1]))
	require.Error(t, check(req("ftp://ok.example"), via[:1]))
}

// errors.Is plumbing sanity: guard errors are plain errors carrying the
// ssrf_guard marker the drivers classify on.
func TestGuardErrorShape(t *testing.T) {
	err := (&Guard{}).CheckIP(net.ParseIP("127.0.0.1"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "ssrf_guard")
	require.False(t, errors.Is(err, http.ErrUseLastResponse))
}
