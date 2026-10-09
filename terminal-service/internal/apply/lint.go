package apply

import (
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// LintPlaybook enforces the §6.7 pre-run gate on a playbook's YAML.
// The content is agent-authored and only "approved" by human merge — the
// lint is the second net for review fatigue. Rules:
//
//  1. unpinned_package — package modules (apt/yum/dnf/zypper/package)
//     must pin explicit versions. A merged PR must describe the exact
//     package state that gets applied; "install latest" is a time bomb
//     that decouples the applied state from what was reviewed.
//  2. mirror_not_allowed — a package task that overrides its repo source
//     (deb/sources/baseurl/...) may only point at mirrors_allow. With no
//     allowlist configured, explicit overrides are refused outright.
//  3. denied_command — command/shell/raw tasks are matched against the
//     ceiling's TG-10 deny globs (case-insensitive, match-anywhere).
//     A playbook needing a denied command belongs in the emergency lane.
//
// Returns all findings; any Fatal finding means the apply must be refused.
func LintPlaybook(playbookYAML []byte, mirrorsAllow, denyPatterns []string) ([]LintFinding, error) {
	var top []any
	if err := yaml.Unmarshal(playbookYAML, &top); err != nil {
		return []LintFinding{{
			Rule:    "unparseable_playbook",
			Message: "playbook is not valid YAML: " + err.Error(),
			Fatal:   true,
		}}, nil
	}
	l := &linter{mirrors: normalizeHosts(mirrorsAllow), denies: denyPatterns}
	for _, play := range top {
		l.walkPlay(play)
	}
	return l.findings, nil
}

type linter struct {
	mirrors map[string]bool
	denies  []string
	findings []LintFinding
}

func (l *linter) add(rule, task, msg string, fatal bool) {
	l.findings = append(l.findings, LintFinding{Rule: rule, Task: task, Message: msg, Fatal: fatal})
}

func (l *linter) walkPlay(play any) {
	playMap, ok := play.(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"pre_tasks", "tasks", "post_tasks", "handlers"} {
		if tasks, ok := playMap[key].([]any); ok {
			for _, t := range tasks {
				l.walkTask(t)
			}
		}
	}
}

func (l *linter) walkTask(task any) {
	tm, ok := task.(map[string]any)
	if !ok {
		return
	}
	name, _ := tm["name"].(string)
	// Block structure: recurse into block/rescue/always.
	for _, key := range []string{"block", "rescue", "always"} {
		if sub, ok := tm[key].([]any); ok {
			for _, t := range sub {
				l.walkTask(t)
			}
		}
	}
	for rawModule, args := range tm {
		module := normalizeModule(rawModule)
		switch module {
		case "command", "shell", "raw":
			l.checkCommand(name, module, args)
		case "apt", "yum", "dnf", "zypper", "package":
			l.checkPackage(name, module, args)
		}
	}
}

func (l *linter) checkCommand(task, module string, args any) {
	var cmd string
	switch a := args.(type) {
	case string:
		cmd = a
	case map[string]any:
		if c, ok := a["cmd"].(string); ok {
			cmd = c
		} else if argv, ok := a["argv"].([]any); ok {
			parts := make([]string, 0, len(argv))
			for _, p := range argv {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}
			cmd = strings.Join(parts, " ")
		}
	}
	if cmd == "" {
		return
	}
	lower := strings.ToLower(cmd)
	for _, pat := range l.denies {
		if denyGlobMatch(pat, lower) {
			l.add("denied_command", taskName(task, module),
				fmt.Sprintf("%s task matches denied pattern %q — use the emergency interactive lane instead", module, pat), true)
			return
		}
	}
}

func (l *linter) checkPackage(task, module string, args any) {
	am, ok := args.(map[string]any)
	if !ok {
		// `- apt: nginx` (string form) — single unpinned package.
		if s, isStr := args.(string); isStr && s != "" {
			if !hasVersionSpec(s) {
				l.add("unpinned_package", taskName(task, module),
					fmt.Sprintf("package %q has no explicit version pin", s), true)
			}
		}
		return
	}
	// Task-level version parameter (zypper/package).
	hasTaskVersion := false
	if v, ok := am["version"]; ok {
		if vs, isStr := v.(string); isStr && strings.TrimSpace(vs) != "" {
			hasTaskVersion = true
		} else if vl, isList := v.([]any); isList && len(vl) > 0 {
			hasTaskVersion = true
		}
	}
	names := []string{}
	switch n := am["name"].(type) {
	case string:
		names = append(names, n)
	case []any:
		for _, x := range n {
			if s, ok := x.(string); ok {
				names = append(names, s)
			}
		}
	}
	if !hasTaskVersion {
		for _, n := range names {
			if !hasVersionSpec(n) {
				l.add("unpinned_package", taskName(task, module),
					fmt.Sprintf("package %q has no explicit version pin (=X.Y.Z, :X.Y.Z, or version:)", n), true)
			}
		}
	}
	// Mirror overrides.
	for _, key := range []string{"deb", "sources", "baseurl", "baseurls", "rpms", "reponame"} {
		vals := []string{}
		switch v := am[key].(type) {
		case string:
			vals = append(vals, v)
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					vals = append(vals, s)
				}
			}
		}
		for _, v := range vals {
			host := mirrorHost(v)
			if host == "" {
				l.add("mirror_not_allowed", taskName(task, module),
					fmt.Sprintf("unparseable repo override %q in %s", v, key), true)
				continue
			}
			if !l.mirrors[host] {
				l.add("mirror_not_allowed", taskName(task, module),
					fmt.Sprintf("repo override %q (host %s) is not in mirrors_allow", v, host), true)
			}
		}
	}
}

// normalizeModule strips collection prefixes: ansible.builtin.apt → apt.
func normalizeModule(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	for _, p := range []string{"ansible.builtin.", "ansible.legacy."} {
		m = strings.TrimPrefix(m, p)
	}
	return m
}

// hasVersionSpec accepts "foo=1.2" (apt/zypper), "foo:1.2" (yum/dnf).
func hasVersionSpec(s string) bool {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "=:"); i > 0 && i < len(s)-1 {
		ver := strings.TrimSpace(s[i+1:])
		return ver != "" && ver[0] != '*' && ver[0] != '?'
	}
	return false
}

func normalizeHosts(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, h := range in {
		m[strings.ToLower(strings.TrimSpace(h))] = true
	}
	return m
}

func mirrorHost(v string) string {
	v = strings.TrimSpace(v)
	if u, err := url.Parse(v); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	// bare hostname (zypper reponame style)
	if !strings.ContainsAny(v, "/ ") && v != "" {
		return strings.ToLower(v)
	}
	return ""
}

func taskName(task, module string) string {
	if strings.TrimSpace(task) != "" {
		return task
	}
	return module
}

// denyGlobMatch mirrors TG-10 deny semantics (tool-gateway ssh policy):
// case-insensitive, pattern matches ANYWHERE in the command. '*' = any run,
// '?' = one char.
func denyGlobMatch(pattern, lowerCmd string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return false
	}
	// Try every start position (match-anywhere) with the anchored matcher.
	for start := 0; start <= len(lowerCmd); start++ {
		if anchoredGlobHere(p, lowerCmd[start:]) {
			return true
		}
	}
	return false
}

func anchoredGlobHere(pattern, s string) bool {
	pi, si := 0, 0
	star, starSi := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			starSi = si
			pi++
		case star >= 0:
			pi = star + 1
			starSi++
			si = starSi
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
