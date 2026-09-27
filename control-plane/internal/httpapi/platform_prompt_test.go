package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/promptcompo"
)

func TestLoadPlatformPromptOverrideUnset(t *testing.T) {
	t.Setenv("SKQUAD_PLATFORM_PROMPT_FILE", "")
	text, err := loadPlatformPromptOverride()
	if err != nil {
		t.Fatalf("unset override must not error: %v", err)
	}
	if text != "" {
		t.Fatalf("unset override must return empty string, got %q", text)
	}
}

func TestLoadPlatformPromptOverrideReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "platform.md")
	if err := os.WriteFile(path, []byte("Operator platform prompt.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKQUAD_PLATFORM_PROMPT_FILE", path)
	text, err := loadPlatformPromptOverride()
	if err != nil {
		t.Fatalf("readable override must load: %v", err)
	}
	if text != "Operator platform prompt.\n" {
		t.Fatalf("unexpected content %q", text)
	}
}

func TestLoadPlatformPromptOverrideUnreadableFails(t *testing.T) {
	t.Setenv("SKQUAD_PLATFORM_PROMPT_FILE", filepath.Join(t.TempDir(), "missing.md"))
	if _, err := loadPlatformPromptOverride(); err == nil {
		t.Fatal("unreadable override must fail (fail-fast, no silent embedded fallback)")
	} else if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("error must name the failure mode, got %v", err)
	}
}

func TestLoadPlatformPromptOverrideEmptyFileFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKQUAD_PLATFORM_PROMPT_FILE", path)
	if _, err := loadPlatformPromptOverride(); err == nil {
		t.Fatal("empty override must fail rather than silently blank the platform tier")
	}
}

func TestLoadPlatformPromptOverrideCapEnforced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.md")
	caps := promptcompo.CapsFromEnv()[promptcompo.TierPlatform]
	// One byte per token estimate => cap.Hard*4+1 bytes exceeds the hard cap.
	oversize := strings.Repeat("a", caps.Hard*4+1)
	if promptcompo.EstimateTokens(oversize) <= caps.Hard {
		t.Skip("estimator no longer byte/4; adjust fixture")
	}
	if err := os.WriteFile(path, []byte(oversize), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKQUAD_PLATFORM_PROMPT_FILE", path)
	if _, err := loadPlatformPromptOverride(); err == nil {
		t.Fatal("over-cap override must be rejected at startup")
	} else if !strings.Contains(err.Error(), "hard cap") {
		t.Fatalf("error must name the cap, got %v", err)
	}

	// A within-cap file loads cleanly.
	good := strings.Repeat("b", caps.Hard*4-1)
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlatformPromptOverride(); err != nil {
		t.Fatalf("within-cap override must load: %v", err)
	}
}
