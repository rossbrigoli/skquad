// Package grantlint implements the TG-8 grant-change linter: a deterministic,
// pure (no I/O) policy-diff checker per docs/tool-gateway.md §8 invariant 4
// and docs/tg8-grant-approvals-spec.md §A. Every grant/ceiling change is
// machine-checked BEFORE it takes effect and before any auto-approval.
// Findings with Severity "block" disable auto-approval and force the change
// through the owner Inbox with the diff + findings shown.
package grantlint

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// Finding codes (stable wire values — Inbox UI and tests key on these).
const (
	// #nosec G101 -- finding-code wire constant, not a credential.
	CodeNewCredentialedReach = "new_credentialed_reach"
	CodeNewHTTPMethod        = "new_http_method"
	CodeNewMCPTool           = "new_mcp_tool"
	CodeMetadataPath         = "metadata_path"
	CodeClusterInternalPath  = "cluster_internal_path"
	CodeCeilingWidened       = "ceiling_widened"
)

// Severity levels. Only "block" disables auto-approval.
const (
	SeverityBlock = "block"
	SeverityWarn  = "warn"
)

// Finding is one lint result for a change.
type Finding struct {
	Code     string
	Severity string
	Detail   string
}

// ChangeSnapshot is one side of a grant/ceiling change. before==nil means a
// brand-new grant (empty baseline).
type ChangeSnapshot struct {
	ResourceID    string
	RiskTier      string // low|medium|high
	Hosts         []string
	HTTPMethods   []string
	MCPTools      []string
	HasCredential bool
	NumericCaps   map[string]int
	EgressClass   string // public|internal
}

var (
	metadataCIDRs = []*net.IPNet{
		mustCIDR("169.254.0.0/16"), // link-local incl. cloud metadata IPs
	}
	metadataExactIPs = []string{"100.100.100.200"} // Alibaba Cloud metadata
	metadataSuffixes = []string{"metadata.google.internal", ".metadata.goog"}

	privateCIDRs = []*net.IPNet{
		mustCIDR("10.0.0.0/8"),
		mustCIDR("172.16.0.0/12"),
		mustCIDR("192.168.0.0/16"),
		mustCIDR("100.64.0.0/10"), // CGNAT
		mustCIDR("fc00::/7"),      // IPv6 ULA
	}
	clusterSuffixes = []string{".svc", ".svc.cluster.local", ".cluster.local", ".internal"}
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("grantlint: bad built-in CIDR " + s)
	}
	return n
}

// normalizeHost reduces a host entry to a bare lowercase hostname or IP string.
// Accepts bare hosts, host:port, and URL forms; returns "" if nothing remains.
func normalizeHost(raw string) string {
	h := strings.TrimSpace(strings.ToLower(raw))
	if h == "" {
		return ""
	}
	// URL forms: scheme://host[:port]/path
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	// Strip path/fragment/query.
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	// Already a bare IP (v4 or v6) — never port-strip it.
	if net.ParseIP(h) != nil {
		return h
	}
	// Strip port (bracketed IPv6 or plain host:port).
	if strings.HasPrefix(h, "[") {
		if j := strings.Index(h, "]"); j > 0 {
			h = h[1:j]
		}
	} else if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}
	return h
}

func isMetadataHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		for _, c := range metadataCIDRs {
			if c.Contains(ip) {
				return true
			}
		}
		for _, e := range metadataExactIPs {
			if h == e {
				return true
			}
		}
		return false
	}
	for _, s := range metadataSuffixes {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

func isClusterInternalHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		for _, c := range privateCIDRs {
			if c.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, s := range clusterSuffixes {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// gained returns members of after not present in before, in after order.
func gained(before, after []string) []string {
	b := toSet(before)
	var out []string
	for _, a := range after {
		if !b[a] {
			out = append(out, a)
		}
	}
	return out
}

// LintChange compares before/after grant+ceiling snapshots and returns all
// findings, deterministically sorted by Code then Detail. before==nil is a
// brand-new grant (empty baseline).
func LintChange(before, after *ChangeSnapshot) []Finding {
	var f []Finding
	if after == nil {
		return f
	}
	var bHosts, bMethods, bTools []string
	bEgress := ""
	var bCaps map[string]int
	if before != nil {
		bHosts, bMethods, bTools = before.Hosts, before.HTTPMethods, before.MCPTools
		bEgress = before.EgressClass
		bCaps = before.NumericCaps
	}

	// --- absolute rule: metadata paths are findings regardless of before ---
	f = append(f, metadataHostFindings(after)...)

	gainedHosts := gained(bHosts, after.Hosts)

	// --- widening rules: only meaningful against a non-nil baseline ---
	// (A brand-new grant has nothing to widen from; its absolute dangers are
	// still caught by the rules above, and its approval path is tier routing.)
	if before != nil {
		f = append(f, wideningFindings(bMethods, bTools, after, gainedHosts)...)
	}

	f = append(f, internalReachFindings(gainedHosts, after.EgressClass, bEgress)...)
	f = append(f, capWideningFindings(before, bCaps, after)...)

	sort.Slice(f, func(i, j int) bool {
		if f[i].Code != f[j].Code {
			return f[i].Code < f[j].Code
		}
		return f[i].Detail < f[j].Detail
	})
	return f
}

// metadataHostFindings flags cloud metadata endpoints regardless of baseline.
func metadataHostFindings(after *ChangeSnapshot) []Finding {
	var f []Finding
	for _, raw := range after.Hosts {
		h := normalizeHost(raw)
		if h == "" {
			continue
		}
		if isMetadataHost(h) {
			f = append(f, Finding{
				Code:     CodeMetadataPath,
				Severity: SeverityBlock,
				Detail:   fmt.Sprintf("host %s is a cloud metadata endpoint", h),
			})
		}
	}
	return f
}

// wideningFindings reports newly-usable credential reach, new HTTP methods
// and new MCP tools against a non-nil baseline.
func wideningFindings(bMethods, bTools []string, after *ChangeSnapshot, gainedHosts []string) []Finding {
	var f []Finding
	if after.HasCredential && len(gainedHosts) > 0 {
		f = append(f, Finding{
			Code:     CodeNewCredentialedReach,
			Severity: SeverityBlock,
			Detail:   fmt.Sprintf("credential newly usable against %d host(s): %s", len(gainedHosts), strings.Join(gainedHosts, ", ")),
		})
	}
	for _, m := range gained(bMethods, after.HTTPMethods) {
		f = append(f, Finding{
			Code:     CodeNewHTTPMethod,
			Severity: SeverityBlock,
			Detail:   fmt.Sprintf("HTTP method %s newly allowed", m),
		})
	}
	for _, t := range gained(bTools, after.MCPTools) {
		f = append(f, Finding{
			Code:     CodeNewMCPTool,
			Severity: SeverityBlock,
			Detail:   fmt.Sprintf("MCP tool %s newly allowed", t),
		})
	}
	return f
}

// internalReachFindings covers cluster-internal reach: gained hosts (for
// before==nil, gained = all after hosts, so brand-new grants into private
// space are caught) plus newly-internal egress class. Metadata hosts are
// excluded here to avoid double-reporting the same host under both codes.
func internalReachFindings(gainedHosts []string, afterEgress, beforeEgress string) []Finding {
	var f []Finding
	for _, raw := range gainedHosts {
		h := normalizeHost(raw)
		if h != "" && isClusterInternalHost(h) && !isMetadataHost(h) {
			f = append(f, Finding{
				Code:     CodeClusterInternalPath,
				Severity: SeverityBlock,
				Detail:   fmt.Sprintf("host %s is cluster-internal/private space", h),
			})
		}
	}
	if afterEgress == "internal" && beforeEgress != "internal" {
		f = append(f, Finding{
			Code:     CodeClusterInternalPath,
			Severity: SeverityBlock,
			Detail:   "egress class newly set to internal",
		})
	}
	return f
}

// capWideningFindings reports any numeric cap increased. (Set widening is
// already surfaced by the gained-* codes above.)
func capWideningFindings(before *ChangeSnapshot, bCaps map[string]int, after *ChangeSnapshot) []Finding {
	var f []Finding
	for cap, av := range after.NumericCaps {
		if bv, ok := bCaps[cap]; ok && av > bv {
			f = append(f, Finding{
				Code:     CodeCeilingWidened,
				Severity: SeverityBlock,
				Detail:   fmt.Sprintf("cap %s widened %d -> %d", cap, bv, av),
			})
		} else if !ok && before != nil {
			// Brand-new cap on an existing grant: only a finding if it is
			// effectively no limit vs a prior implicit one — conservative:
			// treat added caps as widening only when value > 0 and the key
			// was absent before on a NON-nil baseline.
			f = append(f, Finding{
				Code:     CodeCeilingWidened,
				Severity: SeverityWarn,
				Detail:   fmt.Sprintf("cap %s added (%d) where baseline had none", cap, av),
			})
		}
	}
	return f
}

// HasBlock reports whether any finding disables auto-approval.
func HasBlock(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityBlock {
			return true
		}
	}
	return false
}
