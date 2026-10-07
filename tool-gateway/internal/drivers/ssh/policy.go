// Package ssh implements the gateway's governed `ssh` driver
// (docs/tool-gateway.md §6.6, TG-10): Terminal-as-a-Service brokering.
//
// The agent never holds SSH credentials and never speaks SSH: every
// command is a JSON call through the gateway, enforced here, executed by
// the in-cluster terminal-service. This file owns the policy fold
// (ceiling ∧ grant constraints); driver.go owns the flow (host check →
// command policy → confirmation gate → audit-before-execute →
// terminal-service call).
package ssh

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Defaults (docs/tool-gateway.md §6.6 + TG-10 brief).
const (
	DefaultCertTTLMinutes    = 15
	MinCertTTLMinutes        = 15
	MaxCertTTLMinutes        = 60
	DefaultMaxConcurrentSess = 2
	DefaultExecTimeoutSec    = 60
	MaxExecTimeoutSec        = 300
)

// defaultCommandDeny is applied even when neither layer sets
// command_deny (TG-10 brief). These patterns route to the
// confirmation gate — they are not hard denials; a deny-pattern hit
// means "an owner must have approved this shape before".
//
// "writes redirect to /etc/*" is realized as the four redirect globs
// below ("> " and ">> " forms, with and without a leading command).
// "sudo *" is a DECISION: the brief's table lists "sudo rm -rf" as a
// case that must not pass silently, and "rm -rf *" does not match it
// (prefix differs). Failing closed, every sudo invocation requires
// confirmation.
var defaultCommandDeny = []string{
	"rm -rf *",
	"dd *of=*",
	"mkfs*",
	"shutdown*",
	"reboot*",
	"systemctl stop *",
	"systemctl disable *",
	"kill -9 1",
	"sudo *",
	"*> /etc/*",
	"> /etc/*",
	"*>> /etc/*",
	">> /etc/*",
}

// sshPolicyShape is the shared JSON shape of the ceiling AND the grant
// constraints (CP registration owns the authoritative copy; this is
// the gateway-side re-validation). Pointer slices distinguish "unset"
// (inherit) from an explicit empty list so default-deny folds
// correctly.
type sshPolicyShape struct {
	HostsAllow            []string `json:"hosts_allow"`
	HostsDeny             []string `json:"hosts_deny"`
	CommandAllow          []string `json:"command_allow"`
	CommandDeny           []string `json:"command_deny"`
	CertTTLMinutes        int      `json:"cert_ttl_minutes"`
	MaxConcurrentSessions int      `json:"max_concurrent_sessions"`
	ExecTimeoutSeconds    int      `json:"exec_timeout_seconds"`
}

// Policy is the effective ssh policy: ceiling ∧ constraints, folded
// tightest-first (mirrors rest/policy.go layering).
type Policy struct {
	HostsAllow            []string // default-deny: empty allow = deny all
	HostsDeny             []string // union; deny wins
	CommandAllow          []string // optional; non-empty ⇒ command must match one
	CommandDeny           []string // union of ceiling ∪ constraints ∪ defaults
	CertTTLMinutes        int      // 15–60, default 15
	MaxConcurrentSessions int      // default 2
	ExecTimeoutSeconds    int      // default 60, hard cap 300
}

// EffectivePolicy folds ceiling ∧ constraints. A malformed layer never
// widens reach: it is an error (caller denies). No-escalation is
// re-validated here (the CP subset check is authoritative; this is a
// second gate): every constraint hosts_allow / command_allow pattern
// must be covered by the ceiling's allow set, and a constraint may not
// add allows where the ceiling allows nothing.
func EffectivePolicy(ceilingRaw, constraintsRaw json.RawMessage) (*Policy, error) {
	var ce, con sshPolicyShape
	if err := parseLayer(ceilingRaw, &ce); err != nil {
		return nil, fmt.Errorf("ceiling: %w", err)
	}
	if err := parseLayer(constraintsRaw, &con); err != nil {
		return nil, fmt.Errorf("constraints: %w", err)
	}

	// No-escalation: constraints ⊆ ceiling on the allow dimensions.
	if err := assertAllowSubset(con.HostsAllow, ce.HostsAllow, "hosts_allow"); err != nil {
		return nil, err
	}
	if err := assertAllowSubset(con.CommandAllow, ce.CommandAllow, "command_allow"); err != nil {
		return nil, err
	}

	p := &Policy{
		// Allow: the grant's list when set (already proven a subset),
		// else the ceiling's. Empty = default-deny.
		HostsAllow:   firstSet(con.HostsAllow, ce.HostsAllow),
		CommandAllow: firstSet(con.CommandAllow, ce.CommandAllow),
		// Deny: UNION across layers plus the platform defaults
		// (additive tightening only; deny always wins).
		HostsDeny:   append(append([]string{}, ce.HostsDeny...), con.HostsDeny...),
		CommandDeny: append(append(append([]string{}, ce.CommandDeny...), con.CommandDeny...), defaultCommandDeny...),
		// Numerics: tightest (min) across layers that set one, then
		// clamped into the platform band.
		CertTTLMinutes:        clampInt(minSet(ce.CertTTLMinutes, con.CertTTLMinutes), DefaultCertTTLMinutes, MinCertTTLMinutes, MaxCertTTLMinutes),
		MaxConcurrentSessions: minPositive(con.MaxConcurrentSessions, ce.MaxConcurrentSessions, DefaultMaxConcurrentSess),
		ExecTimeoutSeconds:    clampInt(minSet(ce.ExecTimeoutSeconds, con.ExecTimeoutSeconds), DefaultExecTimeoutSec, 1, MaxExecTimeoutSec),
	}
	return p, nil
}

func parseLayer(raw json.RawMessage, out *sshPolicyShape) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// firstSet returns a when non-nil, else b. (An explicitly-set empty
// list still selects `a`, preserving default-deny semantics.)
func firstSet(a, b []string) []string {
	if a != nil {
		return a
	}
	return b
}

// minSet returns the minimum of the layers that set a positive value;
// 0 when none set (caller applies the default).
func minSet(vals ...int) int {
	out := 0
	for _, v := range vals {
		if v > 0 && (out == 0 || v < out) {
			out = v
		}
	}
	return out
}

func minPositive(a, b, def int) int {
	if m := minSet(a, b); m > 0 {
		return m
	}
	return def
}

func clampInt(v, def, lo, hi int) int {
	if v == 0 {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// assertAllowSubset rejects a constraint allow-list that is not covered
// by the ceiling allow-list. Coverage is per-pattern: a constraint
// pattern p is covered when some ceiling pattern c matches p's literal
// text (globMatch(c, p)) or equals it. This is a pragmatic second gate
// against obviously escalated grants (disjoint additions, allows where
// the ceiling denies everything); the CP subset validation remains
// authoritative.
//
// DECISION: pattern-vs-pattern coverage is not a sound glob-subset
// proof (that is undecidable-cheap in general); we deliberately keep
// the cheap conservative check and rely on the CP for the real proof.
func assertAllowSubset(conAllow, ceAllow []string, field string) error {
	if len(conAllow) == 0 {
		return nil
	}
	if len(ceAllow) == 0 {
		return fmt.Errorf("%w: constraints set %s but ceiling allows none", ErrCeilingExceeded, field)
	}
	for _, p := range conAllow {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		covered := false
		for _, c := range ceAllow {
			if strings.TrimSpace(c) == p || globMatch(c, p) {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("%w: %s pattern %q not covered by ceiling", ErrCeilingExceeded, field, p)
		}
	}
	return nil
}

// ErrCeilingExceeded marks a no-escalation violation at fold time.
var ErrCeilingExceeded = &ceilingError{}

type ceilingError struct{}

func (e *ceilingError) Error() string { return "ceiling_exceeded" }

// HostAllowed applies default-deny + deny-wins: host must match at
// least one hosts_allow pattern and no hosts_deny pattern.
func (p *Policy) HostAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	for _, d := range p.HostsDeny {
		if globMatch(d, h) {
			return false
		}
	}
	for _, a := range p.HostsAllow {
		if globMatch(a, h) {
			return true
		}
	}
	return false
}

// CommandVerdict is the outcome of command policy evaluation.
type CommandVerdict int

const (
	// CommandPass: allowed outright (matched allow, or allow-list
	// unset and no deny match).
	CommandPass CommandVerdict = iota
	// CommandGate: matched a deny pattern → confirmation gate.
	CommandGate
	// CommandDeny: a non-empty command_allow list exists and the
	// command matched none → hard denial.
	CommandDeny
)

// CheckCommand evaluates the normalized command. When the result is
// CommandGate, DeniedPattern is the first matching deny glob (used in
// the confirmation tool identity).
//
// DECISION (fail-closed): deny globs match ANYWHERE in the command
// (substring semantics), so chained payloads like "ls /; rm -rf /"
// still trip "rm -rf *". Allow globs stay anchored (prefix semantics):
// an allow-list entry only whitelists commands that START with it.
// Over-matching deny costs extra confirmations; under-matching would be
// a bypass — we err toward the former.
func (p *Policy) CheckCommand(normalized string) (verdict CommandVerdict, deniedPattern string) {
	n := NormalizeCommand(normalized)
	for _, d := range p.CommandDeny {
		if globMatchAnywhere(d, n) {
			return CommandGate, strings.TrimSpace(d)
		}
	}
	if len(p.CommandAllow) > 0 {
		for _, a := range p.CommandAllow {
			if globMatch(a, n) {
				return CommandPass, ""
			}
		}
		return CommandDeny, ""
	}
	return CommandPass, ""
}

// NormalizeCommand lowercases and collapses internal whitespace runs
// to single spaces (plus trim) so glob patterns behave predictably on
// shell-ish command strings.
func NormalizeCommand(cmd string) string {
	return strings.Join(strings.Fields(strings.ToLower(cmd)), " ")
}

// globMatch is a case-insensitive glob matcher:
//
//	'*' matches ANY run of characters — including spaces and '/'.
//	'?' matches exactly one character.
//
// DECISION: we do NOT use path.Match: its '*' stops at '/', which
// would silently fail patterns like "rm -rf /var/log" against
// "rm -rf *". Commands are whitespace-normalized first, so '*'
// crossing spaces is the intended semantic for prefix globs like
// "systemctl stop *". Matching is anchored to the whole string.
func globMatch(pattern, s string) bool {
	re, err := compileGlob(strings.ToLower(strings.TrimSpace(pattern)), false)
	if err != nil {
		// A broken pattern matches nothing: fail closed.
		return false
	}
	return re.MatchString(strings.ToLower(s))
}

// globMatchAnywhere matches the pattern against any substring of s
// (deny-side semantics, see CheckCommand DECISION).
func globMatchAnywhere(pattern, s string) bool {
	re, err := compileGlob(strings.ToLower(strings.TrimSpace(pattern)), true)
	if err != nil {
		return false
	}
	return re.MatchString(strings.ToLower(s))
}

var globCache sync.Map // pattern+"|anywhere" -> *regexp.Regexp

func compileGlob(pattern string, anywhere bool) (*regexp.Regexp, error) {
	key := pattern
	if anywhere {
		key += "|anywhere"
	}
	if re, ok := globCache.Load(key); ok {
		return re.(*regexp.Regexp), nil
	}
	var sb strings.Builder
	if anywhere {
		sb.WriteString(".*")
	} else {
		sb.WriteString("^")
	}
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if anywhere {
		sb.WriteString(".*")
	} else {
		sb.WriteString("$")
	}
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil, err
	}
	globCache.Store(key, re)
	return re, nil
}
