package promptcompo

import (
	"os"
	"strconv"
	"strings"
)

// TierComposed is the pseudo-tier for the total composed prompt budget.
const TierComposed TierName = "composed"

// Caps are soft-warn / hard-cap token budgets for one scope.
type Caps struct {
	Soft int
	Hard int
}

// DefaultCaps returns the ADR-0011 D6 budgets (decided by Ross 2026-09-27:
// agent tier 8k hard; composed total raised to 16k to accommodate.
// S-148, 2026-09-28: organization tier raised to 4k hard, soft scaled to
// 3k to keep the ~75% soft/hard ratio used by the other tiers).
func DefaultCaps() map[TierName]Caps {
	return map[TierName]Caps{
		TierPlatform:     {Soft: 3000, Hard: 4000},
		TierOrganization: {Soft: 3000, Hard: 4000},
		TierSquad:        {Soft: 1500, Hard: 2000},
		TierAgent:        {Soft: 6000, Hard: 8000},
		TierComposed:     {Soft: 12000, Hard: 16000},
	}
}

// CapsFromEnv returns default caps with environment overrides applied.
//
// Global:   SKQUAD_PROMPT_SOFT_TOKENS, SKQUAD_PROMPT_MAX_TOKENS
// Per-tier: SKQUAD_PROMPT_SOFT_TOKENS_<PLATFORM|ORGANIZATION|SQUAD|AGENT|COMPOSED>
//
// Per-tier overrides win over globals. Invalid values are ignored (defaults
// retained) — configuration errors must not silently raise security budgets.
func CapsFromEnv() map[TierName]Caps {
	caps := DefaultCaps()
	for _, name := range append(append([]TierName{}, tierOrder...), TierComposed) {
		upper := strings.ToUpper(string(name))
		applyTierOverride(caps, name, "SKQUAD_PROMPT_MAX_TOKENS_"+upper, func(c *Caps, v int) { c.Hard = v })
		applyTierOverride(caps, name, "SKQUAD_PROMPT_SOFT_TOKENS_"+upper, func(c *Caps, v int) { c.Soft = v })
	}
	applyGlobalOverride(caps, "SKQUAD_PROMPT_MAX_TOKENS", func(c *Caps, v int) { c.Hard = v })
	applyGlobalOverride(caps, "SKQUAD_PROMPT_SOFT_TOKENS", func(c *Caps, v int) { c.Soft = v })
	// Enforce soft <= hard after overrides.
	for name, c := range caps {
		if c.Soft > c.Hard {
			c.Soft = c.Hard
			caps[name] = c
		}
	}
	return caps
}

// applyTierOverride sets one cap field for a single tier from an env key.
func applyTierOverride(caps map[TierName]Caps, name TierName, key string, set func(*Caps, int)) {
	if v, ok := envInt(key); ok {
		c := caps[name]
		set(&c, v)
		caps[name] = c
	}
}

// applyGlobalOverride sets one cap field for all non-composed tiers from an env key.
func applyGlobalOverride(caps map[TierName]Caps, key string, set func(*Caps, int)) {
	if v, ok := envInt(key); ok {
		for name := range caps {
			if name == TierComposed {
				continue
			}
			c := caps[name]
			set(&c, v)
			caps[name] = c
		}
	}
}

func envInt(key string) (int, bool) {
	raw := os.Getenv(key)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// EstimateTokens is a deterministic, dependency-free token heuristic.
//
// ceil(utf8Bytes/4) for ASCII-dominant text; ceil(utf8Bytes/3.5) when the
// non-ASCII rune ratio exceeds 20% (multi-byte characters tokenize less
// efficiently). Documented in ADR-0011 D6; within ±25% of tiktoken cl100k
// for typical English prose (see TestEstimateTokensAgainstCl100kBaseline).
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	bytes := len([]byte(s))
	runes := []rune(s)
	nonASCII := 0
	for _, r := range runes {
		if r > 127 {
			nonASCII++
		}
	}
	if len(runes) > 0 && float64(nonASCII)/float64(len(runes)) > 0.20 {
		return ceilDiv(bytes, 2, 7) // bytes / 3.5 == bytes * 2 / 7
	}
	return ceilDiv(bytes, 1, 4)
}

// ceilDiv computes ceil(a * num / den) without floating point.
func ceilDiv(a, num, den int) int {
	return (a*num + den - 1) / den
}
