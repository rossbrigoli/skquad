package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/shared/netguard"
)

// stubResolver maps hostnames to fixed IPs (DNS-rebinding simulation).
type stubResolver struct {
	mu     sync.Mutex
	answers map[string][]string
	calls   map[string]int
}

func (s *stubResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[host]++
	ips, ok := s.answers[host]
	if !ok {
		return nil, fmt.Errorf("stub: no such host %s", host)
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}

func (s *stubResolver) count(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[host]
}

// startEcho starts a local TCP echo server, returns "port" string.
func startEcho(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func newProxyServer(t *testing.T, guard *netguard.Guard, res *stubResolver, ports []int, buf *bytes.Buffer) *httptest.Server {
	t.Helper()
	logger := log.New(buf, "", 0)
	p := New(Config{Guard: guard, Resolver: res, AllowedPorts: ports, Logger: logger, CopyIdle: 2 * time.Second})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv
}

func readStatusLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	return strings.TrimSpace(line)
}

// drainHeaders consumes response headers up to the blank line.
func drainHeaders(t *testing.T, r *bufio.Reader) {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			return
		}
	}
}

func TestConnectLoopbackDenied(t *testing.T) {
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{"evil.test": {"127.0.0.1"}}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{}, res, nil, &buf)

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT evil.test:443 HTTP/1.1\r\nHost: evil.test:443\r\n\r\n")
	status := readStatusLine(t, bufio.NewReader(c))
	if !strings.Contains(status, "403") {
		t.Fatalf("want 403, got %q", status)
	}
	if !strings.Contains(buf.String(), "allowed=f reason=ssrf_blocked_or_unreachable") {
		t.Fatalf("missing audit denial: %s", buf.String())
	}
}

func TestConnectRFC1918Denied(t *testing.T) {
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{"internal.test": {"10.0.0.5"}}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{}, res, nil, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "CONNECT internal.test:80 HTTP/1.1\r\nHost: internal.test:80\r\n\r\n")
	status := readStatusLine(t, bufio.NewReader(c))
	if !strings.Contains(status, "403") {
		t.Fatalf("want 403, got %q", status)
	}
}

func TestConnectRebindingDenied(t *testing.T) {
	// DNS answer set mixing a public and a private address (rebinding):
	// strict guard denies the WHOLE name, never dials.
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{"mix.test": {"93.184.216.34", "127.0.0.1"}}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{}, res, nil, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "CONNECT mix.test:443 HTTP/1.1\r\nHost: mix.test:443\r\n\r\n")
	status := readStatusLine(t, bufio.NewReader(c))
	if !strings.Contains(status, "403") {
		t.Fatalf("want 403 for poisoned DNS set, got %q", status)
	}
}

func TestConnectPortDenied(t *testing.T) {
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{"ssh.test": {"93.184.216.34"}}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{}, res, nil, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "CONNECT ssh.test:22 HTTP/1.1\r\nHost: ssh.test:22\r\n\r\n")
	status := readStatusLine(t, bufio.NewReader(c))
	if !strings.Contains(status, "403") {
		t.Fatalf("want 403 for port 22, got %q", status)
	}
	if res.count("ssh.test") != 0 {
		t.Fatal("resolver must not be consulted for denied ports")
	}
	if !strings.Contains(buf.String(), "reason=port_denied") {
		t.Fatalf("missing port_denied audit: %s", buf.String())
	}
}

func TestConnectMalformed(t *testing.T) {
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{}, res, nil, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "CONNECT noport HTTP/1.1\r\nHost: noport\r\n\r\n")
	status := readStatusLine(t, bufio.NewReader(c))
	if !strings.Contains(status, "400") {
		t.Fatalf("want 400 for missing port, got %q", status)
	}
}

func TestConnectAllowedTunnelsBytes(t *testing.T) {
	// Mechanics test: AllowPrivate so the local echo target is dialable;
	// the strict floor itself is covered by the deny tests above.
	echoPort := startEcho(t)
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{"ok.test": {"127.0.0.1"}}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{AllowPrivate: true}, res, []int{echoPort}, &buf)

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT ok.test:%d HTTP/1.1\r\nHost: ok.test:%d\r\n\r\n", echoPort, echoPort)
	r := bufio.NewReader(c)
	status := readStatusLine(t, r)
	if !strings.Contains(status, "200 Connection Established") {
		t.Fatalf("want established, got %q", status)
	}
	drainHeaders(t, r)
	if _, err := c.Write([]byte("hello-tunnel")); err != nil {
		t.Fatal(err)
	}
	buf2 := make([]byte, 12)
	if _, err := io.ReadFull(r, buf2); err != nil {
		t.Fatal(err)
	}
	if string(buf2) != "hello-tunnel" {
		t.Fatalf("tunnel corrupted: %q", string(buf2))
	}
	if !strings.Contains(buf.String(), "allowed=t reason=connected") {
		t.Fatalf("missing allow audit: %s", buf.String())
	}
}

func TestPlainGetForwarded(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("pong:" + r.URL.Path))
	}))
	defer target.Close()
	port := target.Listener.Addr().(*net.TCPAddr).Port
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{"ok.test": {"127.0.0.1"}}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{AllowPrivate: true}, res, []int{port}, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "GET http://ok.test:%d/ping HTTP/1.1\r\nHost: ok.test:%d\r\n\r\n", port, port)
	r := bufio.NewReader(c)
	status := readStatusLine(t, r)
	if !strings.Contains(status, "200") {
		t.Fatalf("want 200 relayed, got %q", status)
	}
	drainHeaders(t, r)
	body := make([]byte, len("pong:/ping"))
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "pong:/ping" {
		t.Fatalf("body mismatch: %q", string(body))
	}
	if !strings.Contains(buf.String(), "allowed=t reason=forwarded") {
		t.Fatalf("missing forwarded audit: %s", buf.String())
	}
}

func TestPlainGetStrictGuardDenied(t *testing.T) {
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{"internal.test": {"192.168.1.1"}}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{}, res, nil, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "GET http://internal.test:80/x HTTP/1.1\r\nHost: internal.test\r\n\r\n")
	status := readStatusLine(t, bufio.NewReader(c))
	if !strings.Contains(status, "403") {
		t.Fatalf("want 403, got %q", status)
	}
}

func TestPlainSchemeDenied(t *testing.T) {
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{}, res, nil, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	// Absolute https URI through a plain-HTTP proxy connection is invalid.
	fmt.Fprintf(c, "GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	status := readStatusLine(t, bufio.NewReader(c))
	if !strings.Contains(status, "400") {
		t.Fatalf("want 400 for absolute-https over cleartext proxy, got %q", status)
	}
}

func TestPlainRelativeRequestDenied(t *testing.T) {
	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{}, res, nil, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "GET /relative HTTP/1.1\r\nHost: proxy\r\n\r\n")
	status := readStatusLine(t, bufio.NewReader(c))
	if !strings.Contains(status, "400") {
		t.Fatalf("want 400 for relative (non-proxy) request, got %q", status)
	}
}

func TestHopByHopHeadersStripped(t *testing.T) {
	var fwd http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fwd = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer target.Close()
	port := target.Listener.Addr().(*net.TCPAddr).Port

	var buf bytes.Buffer
	res := &stubResolver{answers: map[string][]string{"ok.test": {"127.0.0.1"}}, calls: map[string]int{}}
	srv := newProxyServer(t, &netguard.Guard{AllowPrivate: true}, res, []int{port}, &buf)

	c, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "GET http://ok.test:%d/x HTTP/1.1\r\nHost: ok.test:%d\r\nProxy-Authorization: ***\r\nProxy-Connection: keep-alive\r\nX-Kept: yes\r\n\r\n", port, port)
	readStatusLine(t, bufio.NewReader(c))

	if fwd.Get("Proxy-Authorization") != "" {
		t.Fatal("Proxy-Authorization must not be forwarded")
	}
	if fwd.Get("Proxy-Connection") != "" {
		t.Fatal("Proxy-Connection must not be forwarded")
	}
	if fwd.Get("X-Kept") != "yes" {
		t.Fatal("non-hop-by-hop header must be forwarded")
	}
}
