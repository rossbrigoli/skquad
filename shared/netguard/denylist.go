// Package netguard — denylist matching for the web driver's content
// policy (docs/tool-gateway.md §6.1: deny_domains). A denied domain
// matches the exact host AND every subdomain of it ("example.com"
// denies "example.com" and "a.example.com", never "notexample.com").
//
// Reminder from the design: the denylist is a content-policy knob, not a
// security boundary. The security floor is the dial-time SSRF guard,
// GET-only, size/time caps and no credential injection.
package netguard

import (
	"net"
	"strings"
)

// NormalizeHost lowercases, trims a trailing dot and strips any port.
func NormalizeHost(host string) string {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "" {
		return h
	}
	if strings.HasPrefix(h, "[") { // IPv6 literal with optional ]port
		if end := strings.Index(h, "]"); end >= 0 {
			return strings.TrimSuffix(h[:end+1], "]") + "]"
		}
		return h
	}
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i+1:], "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

// DomainDenied reports whether host matches any entry in deny. Entries
// are compared case-insensitively; an entry denies the exact host and
// all subdomains. Empty entries are ignored (never deny-all by typo).
func DomainDenied(host string, deny []string) bool {
	h := NormalizeHost(host)
	if h == "" {
		return false
	}
	for _, d := range deny {
		d = NormalizeHost(d)
		if d == "" {
			continue
		}
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// CIDRDenied reports whether a resolved IP falls in any deny CIDR.
func CIDRDenied(ip net.IP, deny []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range deny {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
