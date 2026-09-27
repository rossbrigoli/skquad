package httpapi

// S-PROMPT WP3 — operator platform-prompt override (ADR-0011 §2.4, D6).
//
// The platform tier is deploy-time only: the embedded prompt ships in the
// binary; an operator may replace it with a Helm-rendered file via
// SKQUAD_PLATFORM_PROMPT_FILE. The file is read ONCE at startup and
// size-capped against the platform tier cap (promptcompo CapsFromEnv).
// A configured-but-unreadable or over-cap file fails the process loudly —
// a silently-ignored platform override would quietly downgrade the highest
// trust layer to the embedded default, which is exactly the failure mode
// ADR-0011 D4 forbids.
//
// WP2's composePromptForAgent passes the loaded override as the composer's
// platformOverride argument (empty string => embedded default):
//
//	return promptcompo.Compose(s.platformPrompt, orgPrompt, squad.Prompt, agent.SystemPrompt, facts)

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/promptcompo"
)

// loadPlatformPromptOverride reads the operator platform-prompt override
// from SKQUAD_PLATFORM_PROMPT_FILE. Returns "" when the env var is unset
// (embedded default applies). Errors (never partially-loads) when the file
// is unreadable or empty, or when its estimated token count exceeds the
// platform tier hard cap.
func loadPlatformPromptOverride() (string, error) {
	path := strings.TrimSpace(os.Getenv("SKQUAD_PLATFORM_PROMPT_FILE"))
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("SKQUAD_PLATFORM_PROMPT_FILE=%s must be an absolute path", path)
	}
	// #nosec G703 -- path is the operator-controlled SKQUAD_PLATFORM_PROMPT_FILE
	// env var (Helm-rendered at deploy time), required absolute above; it is
	// never derived from user/request input.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("SKQUAD_PLATFORM_PROMPT_FILE=%s is unreadable: %w", path, err)
	}
	text := string(data)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("SKQUAD_PLATFORM_PROMPT_FILE=%s is empty", path)
	}
	caps := promptcompo.CapsFromEnv()[promptcompo.TierPlatform]
	if tokens := promptcompo.EstimateTokens(text); tokens > caps.Hard {
		return "", fmt.Errorf(
			"SKQUAD_PLATFORM_PROMPT_FILE=%s has %d tokens, platform hard cap is %d",
			path, tokens, caps.Hard,
		)
	}
	return text, nil
}
