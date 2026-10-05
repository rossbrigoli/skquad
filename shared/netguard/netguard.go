// Package netguard is the single source of truth for skquad's dial-time
// SSRF guard (TG-3 extraction of the BT-6 guard from
// control-plane/internal/httpapi/webfetch_proxy.go). Both the control
// plane and the tool gateway consume it — no forked copies.
//
// Defense model (docs/tool-gateway.md §6.1):
//
//   - Checks run at DIAL time on the RESOLVED IP, immediately before
//     connect (net.Dialer.Control semantics), closing the DNS-rebinding
//     TOCTOU a resolve-then-connect check has.
//   - Blocked by default: loopback, RFC1918 private, CGNAT 100.64.0.0/10,
//     link-local unicast/multicast (incl. cloud metadata 169.254.169.254),
//     IPv6 ULA fc00::/7, multicast, unspecified, and anything that is not
//     global unicast. Malformed input fails CLOSED.
//   - IP PINNING: Guard.DialContext resolves the hostname once, validates
//     EVERY returned address, and dials the pinned IP. If any resolved
//     address is blocked the whole dial is denied (strict mode — a name
//     that alternates public/private answers, i.e. DNS rebinding, never
//     reaches the private answer).
//   - AllowPrivate relaxes the floor for internal-class registrations
//     (mirrors the old allowPrivateNetwork switch) but DenyCIDRs still
//     apply on top.
//   - Redirects must be re-checked per hop: use RedirectCheck together
//     with a Guard-backed transport (the Control/dial hook fires again
//     for every new connection, so IP checks are automatic; the
//     RedirectCheck adds the hop cap, scheme check and domain denylist).
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

var (
	cgnatRange = mustCIDR("100.64.0.0/10") // CGNAT / shared address space
	ulaRange   = mustCIDR("fc00::/7")      // IPv6 unique local addresses
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("netguard: bad built-in CIDR " + s)
	}
	return n
}

// BlockedIP reports whether a resolved IP must be rejected by the SSRF
// floor. nil fails closed. This is the pure address-table half of the
// BT-6 guard; keep semantics byte-identical to the original
// blockedDestAddr checks.
func BlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified():
		return true
	}
	if !ip.IsGlobalUnicast() {
		return true
	}
	if ip.To4() != nil {
		if cgnatRange.Contains(ip) {
			return true
		}
	} else {
		if ulaRange.Contains(ip) {
			return true
		}
	}
	return false
}

// BlockedHostPort reports whether a dial destination "host:port" string
// must be rejected. The host must already be a resolved IP literal
// (as guaranteed by net.Dialer.Control); anything malformed fails closed.
// This is the Control-hook-shaped entry point kept for callers of the old
// httpapi.blockedDestAddr.
func BlockedHostPort(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return true // malformed address: fail closed
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // Control should see resolved IPs; fail closed
	}
	return BlockedIP(ip)
}

// Guard enforces the SSRF floor plus optional extra deny CIDRs.
// The zero value is the strict public-only guard.
type Guard struct {
	// AllowPrivate relaxes the floor (internal-class egress). Mirrors
	// the BT-6 allowPrivateNetwork semantics: when true, private/
	// loopback/link-local destinations are permitted — DenyCIDRs still
	// block on top.
	AllowPrivate bool
	// DenyCIDRs are additional always-blocked ranges (from the web
	// resource's deny_cidrs floor/constraints), applied regardless of
	// AllowPrivate.
	DenyCIDRs []*net.IPNet
}

// Blocked reports whether ip is denied by this guard.
func (g *Guard) Blocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	for _, deny := range g.DenyCIDRs {
		if deny.Contains(ip) {
			return true
		}
	}
	if g.AllowPrivate {
		return false
	}
	return BlockedIP(ip)
}

// CheckIP returns a typed error when ip is denied, nil otherwise.
func (g *Guard) CheckIP(ip net.IP) error {
	if g.Blocked(ip) {
		return fmt.Errorf("ssrf_guard: blocked dial to %s", ip)
	}
	return nil
}

// Resolver allows tests to inject a stub resolver (DNS-rebinding
// simulation without touching the system resolver).
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Dialer is the Guard's dial configuration.
type Dialer struct {
	// Guard is required.
	Guard *Guard
	// Timeout bounds each connect.
	Timeout time.Duration
	// Resolver overrides DNS lookups (tests inject stubs). nil uses
	// net.DefaultResolver.
	Resolver Resolver
}

// DialContext resolves host:port, validates every answer against the
// guard, and dials the pinned IP (the first validated answer). Any
// blocked answer denies the whole dial — this is what defeats DNS
// rebinding: the checked decision and the connected socket use the same
// pinned IP, and a rotating answer set containing a blocked IP never
// gets connected at all.
func (d Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	guard := d.Guard
	if guard == nil {
		guard = &Guard{}
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("ssrf_guard: malformed dial address: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil {
		// IP literal: check and dial directly.
		if err := guard.CheckIP(ip); err != nil {
			return nil, err
		}
		return d.dial(ctx, network, address)
	}
	res := d.Resolver
	if res == nil {
		res = net.DefaultResolver
	}
	addrs, err := res.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("ssrf_guard: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("ssrf_guard: no addresses for %s", host)
	}
	var pinned *net.IPAddr
	for i := range addrs {
		if guard.Blocked(addrs[i].IP) {
			// Strict: one poisoned answer denies the whole name.
			return nil, fmt.Errorf("ssrf_guard: blocked dial to %s (resolved %s)", host, addrs[i].IP)
		}
		if pinned == nil {
			pinned = &addrs[i]
		}
	}
	pinnedAddr := net.JoinHostPort(pinned.IP.String(), port)
	return d.dial(ctx, network, pinnedAddr)
}

func (d Dialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	nd := &net.Dialer{Timeout: d.Timeout}
	return nd.DialContext(ctx, network, address)
}

// ControlHook returns a net.Dialer.Control function that re-validates
// the (already resolved) dial address against this guard. Provided for
// callers that build their own net.Dialer but want the shared floor;
// equivalent to the original BT-6 Control closure. Malformed addresses
// and non-IP hosts fail closed.
func (g *Guard) ControlHook() func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("ssrf_guard: malformed dial address %q", address)
		}
		if err := g.CheckIP(net.ParseIP(host)); err != nil {
			return err
		}
		return nil
	}
}

// NewTransport builds an http.Transport whose every outbound connection
// is dialed through the guard with IP pinning (SNI/Host still carry the
// original hostname because http.Client derives TLS ServerName from the
// request URL, not the dialed address).
func (g *Guard) NewTransport(timeout time.Duration) *http.Transport {
	d := Dialer{Guard: g, Timeout: timeout}
	return &http.Transport{
		DialContext:       d.DialContext,
		TLSHandshakeTimeout: timeout,
	}
}

// RedirectCheck builds an http.Client CheckRedirect policy: caps hops,
// rejects non-http(s) schemes, and applies an optional host denylist
// per hop. The dial-time IP checks re-fire for every hop via the
// Guard-backed transport, so per-hop SSRF coverage is automatic.
func RedirectCheck(maxHops int, denyHost func(host string) bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > maxHops {
			return http.ErrUseLastResponse
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
		}
		if denyHost != nil && denyHost(req.URL.Hostname()) {
			return fmt.Errorf("denied redirect target host %s", req.URL.Hostname())
		}
		return nil
	}
}
