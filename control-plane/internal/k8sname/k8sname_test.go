package k8sname

import (
	"strings"
	"testing"
)

func TestSanitizePart(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"bob":                     "bob",
		"Bob":                     "bob",
		"BOB":                     "bob",
		"bob smith":               "bob-smith",
		"  Ross  Brigoli  ":       "ross-brigoli",
		"my_agent_name":           "my-agent-name",
		"a__b":                    "a-b",
		"a--b":                    "a-b",
		"a---b":                   "a-b",
		"-lead":                   "lead",
		"trail-":                  "trail",
		"---":                     "",
		"":                        "",
		"!!!":                     "",
		"héllo":                   "h-llo",       // non-ASCII letters are not [a-z]
		"Ünïcødé Wörld":           "n-c-d-w-rld", // each invalid rune becomes one dash, collapsed
		"agent.name":              "agent-name",
		"agent+plus":              "agent-plus",
		"agent/0":                 "agent-0",
		"Agent 42 — prime":        "agent-42-prime", // em-dash maps to a separator
		"tab\there":               "tab-here",
		"keep.dots.as.separators": "keep-dots-as-separators",
		"trailing!!!":             "trailing",
		"!!!leading":              "leading",
		"MiXeD CaSe 123":          "mixed-case-123",
		"日本語":                     "", // nothing survives
		"x日本語y":                   "x-y",
	}
	for in, want := range tests {
		if got := SanitizePart(in); got != want {
			t.Errorf("SanitizePart(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOwnerSlug(t *testing.T) {
	t.Parallel()

	if got := OwnerSlug("Ross Brigoli", "ross@example.com", "u-1"); got != "ross-brigoli" {
		t.Fatalf("display name: got %q", got)
	}
	if got := OwnerSlug("日本語", "ross@example.com", "u-1"); got != "ross" {
		t.Fatalf("email fallback: got %q", got)
	}
	if got := OwnerSlug("", "@no-local.com", "u-1"); got != "u-1" {
		t.Fatalf("user id fallback: got %q", got)
	}
	if got := OwnerSlug("", "", ""); got != "user" {
		t.Fatalf("empty everything: got %q", got)
	}
	if got := OwnerSlug("日本語", "", ""); got != "user" {
		t.Fatalf("unsanitizable everything: got %q", got)
	}
}

func TestWorkspacePVCNameBasic(t *testing.T) {
	t.Parallel()

	guid := "d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c"
	got := WorkspacePVCName("ross brigoli", "minions", "bob", guid)
	want := "ross-brigoli-minions-bob-workspace-" + guid
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWorkspacePVCNameSkipsEmptyComponents(t *testing.T) {
	t.Parallel()

	guid := "d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c"
	if got := WorkspacePVCName("", "minions", "bob", guid); got != "minions-bob-workspace-"+guid {
		t.Fatalf("empty owner: got %q", got)
	}
	if got := WorkspacePVCName("ross", "", "bob", guid); got != "ross-bob-workspace-"+guid {
		t.Fatalf("empty squad: got %q", got)
	}
	if got := WorkspacePVCName("ross", "minions", "日本語", guid); got != "ross-minions-workspace-"+guid {
		t.Fatalf("unsanitizable agent: got %q", got)
	}
	if got := WorkspacePVCName("", "", "", guid); got != "workspace-"+guid {
		t.Fatalf("all components empty: got %q", got)
	}
	// Never produce a doubled dash from adjacent empties.
	for _, got := range []string{
		WorkspacePVCName("", "", "bob", guid),
		WorkspacePVCName("!!!", "minions", "bob", guid),
	} {
		if strings.Contains(got, "--") {
			t.Fatalf("double dash in %q", got)
		}
	}
}

func TestWorkspacePVCNameRequiresGUID(t *testing.T) {
	t.Parallel()

	if got := WorkspacePVCName("ross", "minions", "bob", ""); got != "" {
		t.Fatalf("empty GUID must yield empty name, got %q", got)
	}
	if got := WorkspacePVCName("ross", "minions", "bob", "!!!"); got != "" {
		t.Fatalf("unsanitizable GUID must yield empty name, got %q", got)
	}
}

func TestWorkspacePVCNameTruncationPreservesGUIDTail(t *testing.T) {
	t.Parallel()

	guid := "d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c"
	long := strings.Repeat("x", 200)
	got := WorkspacePVCName(long, long, long, guid)
	if len(got) > MaxNameLen {
		t.Fatalf("name exceeds %d: %d", MaxNameLen, len(got))
	}
	tail := "-workspace-" + guid
	if !strings.HasSuffix(got, tail) {
		t.Fatalf("GUID tail not preserved: %q", got)
	}
	if strings.HasSuffix(got[:len(got)-len(tail)], "-") {
		t.Fatalf("truncated prefix left a trailing dash: %q", got)
	}
	// Prefix must have actually been cut (3*200 + tail > 253).
	if len(got) != MaxNameLen {
		t.Fatalf("expected exactly %d chars, got %d", MaxNameLen, len(got))
	}
	// Exactly-at-limit composition is untouched.
	pad := strings.Repeat("y", MaxNameLen-len("-a-workspace-"+guid))
	exact := WorkspacePVCName(pad, "a", "", guid)
	if len(exact) != MaxNameLen || !strings.HasSuffix(exact, tail) {
		t.Fatalf("exact-length name mangled: len=%d %q", len(exact), exact)
	}
}

func TestWorkspacePVCNameIsRFC1123(t *testing.T) {
	t.Parallel()

	guid := "d88f5db9-ea9e-41a6-95fe-90fd51ed1b6c"
	name := WorkspacePVCName("Ross's Squad!!", "Team 42", "Bob_The_Builder", guid)
	if len(name) == 0 || len(name) > MaxNameLen {
		t.Fatalf("bad length: %d", len(name))
	}
	if name[0] == '-' || name[0] == '.' || name[len(name)-1] == '-' || name[len(name)-1] == '.' {
		t.Fatalf("must start/end alphanumeric: %q", name)
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.') {
			t.Fatalf("invalid rune %q in %q", r, name)
		}
	}
}
