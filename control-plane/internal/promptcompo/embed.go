package promptcompo

import _ "embed"

//go:embed platform_prompt.md
var embeddedPlatformPrompt string

// EmbeddedPlatformPrompt returns the platform prompt compiled into the
// control-plane binary. Operators override the platform layer with a
// Helm-rendered file (SKQUAD_PLATFORM_PROMPT_FILE) read by the caller —
// never by lower tiers.
func EmbeddedPlatformPrompt() string {
	return embeddedPlatformPrompt
}
