// Package proxy implements the TG-6 browser egress forward-proxy sidecar
// (docs/tg6-browser-protocol.md §5). It is the ONLY network path out of
// the quarantine browser pod: Chromium is launched with
// --proxy-server=http://127.0.0.1:8888 and every connection is checked
// through the shared netguard SSRF floor with dial-time IP pinning.
//
// Trust model: same-pod sidecar. No authentication — NetworkPolicy in
// the skquad-browser namespace ensures nothing else can reach this port.
package proxy

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rossbrigoli/skquad/shared/netguard"
)

// Config wires a Proxy. Guard is required; everything else has
// production-sane defaults.
type Config struct {
	Guard         *netguard.Guard
	Resolver      netguard.Resolver // nil → system resolver (tests inject stubs)
	DialTimeout   time.Duration     // default 10s
	CopyIdle      time.Duration     // default 120s per-direction idle
	AllowedPorts  []int           // default {80, 443}
	Logger        *log.Logger      // nil → log.Default()
}

// Proxy is an http.Handler speaking forward-proxy HTTP: CONNECT tunnels
// plus absolute-URI plain requests, all guard-pinned.
type Proxy struct {
	dialer       netguard.Dialer
	transport    *http.Transport
	dialTimeout  time.Duration
	copyIdle     time.Duration
	allowedPorts map[int]bool
	logger       *log.Logger
}

// New builds a Proxy from config.
func New(cfg Config) *Proxy {
	if cfg.Guard == nil {
		cfg.Guard = &netguard.Guard{} // strict public-only
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.CopyIdle <= 0 {
		cfg.CopyIdle = 120 * time.Second
	}
	ports := map[int]bool{}
	for _, p := range cfg.AllowedPorts {
		ports[p] = true
	}
	if len(ports) == 0 {
		ports = map[int]bool{80: true, 443: true}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	d := netguard.Dialer{Guard: cfg.Guard, Timeout: cfg.DialTimeout, Resolver: cfg.Resolver}
	return &Proxy{
		dialer:       d,
		transport:  &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: cfg.DialTimeout},
		dialTimeout:  cfg.DialTimeout,
		copyIdle:     cfg.CopyIdle,
		allowedPorts: ports,
		logger:       cfg.Logger,
	}
}

// ServeHTTP routes CONNECT vs plain proxy requests.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handlePlain(w, r)
}

// audit writes one structured line per connection decision. Never
// contains payloads, headers, or credentials.
func (p *Proxy) audit(target, ip string, port int, allowed bool, reason string) {
	flag := "f"
	if allowed {
		flag = "t"
	}
	p.logger.Printf("ts=%s target=%s ip=%s port=%d allowed=%s reason=%s",
		time.Now().UTC().Format(time.RFC3339), target, ip, port, flag, reason)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil || host == "" {
		p.audit(r.Host, "", 0, false, "malformed_target")
		http.Error(w, "malformed CONNECT target", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		p.audit(r.Host, "", 0, false, "malformed_port")
		http.Error(w, "malformed port", http.StatusBadRequest)
		return
	}
	if !p.allowedPorts[port] {
		p.audit(host, "", port, false, "port_denied")
		http.Error(w, "port not allowed", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), p.dialTimeout)
	conn, err := p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, portStr))
	if err != nil {
		cancel()
		p.audit(host, "", port, false, "ssrf_blocked_or_unreachable")
		http.Error(w, "destination blocked", http.StatusForbidden)
		return
	}
	defer conn.Close()
	defer cancel()

	hj, ok := w.(http.Hijacker)
	if !ok {
		p.audit(host, conn.RemoteAddr().String(), port, false, "hijack_unsupported")
		http.Error(w, "proxy cannot hijack", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		p.audit(host, conn.RemoteAddr().String(), port, false, "hijack_failed")
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	p.audit(host, conn.RemoteAddr().String(), port, true, "connected")
	p.pipe(client, conn)
}

// handlePlain forwards absolute-URI plain HTTP requests
// (GET http://host/path over the proxy connection).
func (p *Proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme != "http" {
		p.audit(r.URL.Host, "", 0, false, "scheme_denied")
		http.Error(w, "proxy handles absolute-http requests only", http.StatusBadRequest)
		return
	}
	port := 80
	if ps := r.URL.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n <= 0 || n > 65535 {
			p.audit(r.URL.Host, "", 0, false, "malformed_port")
			http.Error(w, "malformed port", http.StatusBadRequest)
			return
		}
		port = n
	}
	if !p.allowedPorts[port] {
		p.audit(r.URL.Hostname(), "", port, false, "port_denied")
		http.Error(w, "port not allowed", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), p.dialTimeout+p.copyIdle)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL.String(), r.Body)
	if err != nil {
		p.audit(r.URL.Host, "", port, false, "request_build_failed")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	copyForwardHeaders(req.Header, r.Header)

	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		p.audit(r.URL.Hostname(), "", port, false, "ssrf_blocked_or_unreachable")
		http.Error(w, "destination blocked", http.StatusForbidden)
		return
	}
	defer resp.Body.Close()

	copyForwardHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	p.audit(r.URL.Hostname(), "", port, true, "forwarded")
}

// hopByHop lists connection-scoped headers that must not be forwarded in
// either direction (RFC 7230 §6.1) plus Proxy-* auth headers.
var hopByHop = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true,
	"Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

func copyForwardHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// pipe copies bytes both directions until either side closes, enforcing
// an idle deadline per read so half-open tunnels cannot leak.
func (p *Proxy) pipe(client net.Conn, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyWithIdle(upstream, client, p.copyIdle)
	}()
	go func() {
		defer wg.Done()
		copyWithIdle(client, upstream, p.copyIdle)
	}()
	wg.Wait()
}

func copyWithIdle(dst io.Writer, src net.Conn, idle time.Duration) {
	buf := make([]byte, 32*1024)
	for {
		if err := src.SetReadDeadline(time.Now().Add(idle)); err != nil {
			return
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			// EOF, reset, or idle deadline: end this direction.
			return
		}
	}
}
