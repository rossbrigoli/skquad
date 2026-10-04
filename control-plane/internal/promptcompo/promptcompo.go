// Package promptcompo implements skquad's layered prompt system (ADR-0011).
//
// Four tiers compose in fixed order — platform > organization > squad > agent —
// each rendered as a tagged block. The precedence rule is declared inside the
// platform block: conflicts resolve upward; lower tiers may refine but never
// relax higher tiers.
//
// This package is pure: no I/O, no database, no HTTP. Callers supply prompt
// texts and facts; the composer returns the composed prompt, its sha256,
// per-tier token accounting, and soft-limit warnings.
package promptcompo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

//go:generate echo "platform prompt embedded from platform_prompt.md"

// Tier names in composition order.
type TierName string

const (
	TierPlatform     TierName = "platform"
	TierOrganization TierName = "organization"
	TierSquad        TierName = "squad"
	TierAgent        TierName = "agent"
)

var tierOrder = []TierName{TierPlatform, TierOrganization, TierSquad, TierAgent}

// Tier is one rendered layer of the composed prompt.
type Tier struct {
	Name     TierName
	Content  string // template-substituted, post-sanitization
	Tokens   int
	SoftWarn int
	HardCap  int
}

// Facts are the platform-side values available to template variables.
type Facts struct {
	AgentName   string
	AgentRole   string
	SquadName   string
	SquadRoster []string // agent names + roles in the squad
	Resources   []string // granted resource summary lines
	Workspace   string   // workspace description/URL
	PlatformVer string
	// Tools are pre-rendered enabled-tool inventory lines ("- name — desc").
	// The caller owns description policy; the composer only joins with \n.
	Tools []string
	// Model facts describe the agent's bound LLM. The caller renders every
	// field to a display string ("unknown" when unbound/unresolvable) so
	// missing model data can never fail composition.
	ModelDisplay       string
	ModelName          string
	ModelProvider      string
	ModelContextWindow string
	ModelSupportsTools string
	ModelFallback      string
	// Owner is the display name(s) of the platform admin user(s),
	// comma-joined; "unknown" when unresolvable or none exist.
	Owner string
}

// Composition is the result of composing all tiers.
type Composition struct {
	Prompt    string   // full composed text
	SHA256    string   // hex sha256 of Prompt
	Tiers     []Tier   // rendered tiers (empty tiers omitted) with token counts
	TotalToks int      // sum of tier token counts
	Warnings  []string // soft-limit breaches
}

// CapError reports a hard token-cap violation (tier or composed total).
type CapError struct {
	Scope  string // tier name or "composed"
	Tokens int
	Cap    int
}

func (e *CapError) Error() string {
	return fmt.Sprintf("prompt token cap exceeded: %s has %d tokens, hard cap %d", e.Scope, e.Tokens, e.Cap)
}

// UnknownTemplateVarsError reports template variables not on the allowlist.
type UnknownTemplateVarsError struct {
	Vars []string
}

func (e *UnknownTemplateVarsError) Error() string {
	return "unknown template variable(s): " + strings.Join(e.Vars, ", ")
}

// ReservedTokensError reports reserved delimiter forgery in user-editable text.
type ReservedTokensError struct {
	Tier TierName
}

func (e *ReservedTokensError) Error() string {
	return fmt.Sprintf("prompt_contains_reserved_tokens: %s tier contains reserved <skquad_ delimiter", e.Tier)
}

// templateVars maps the allowed {{...}} variables to Facts accessors.
var templateVars = map[string]func(Facts) string{
	"agent.name":       func(f Facts) string { return f.AgentName },
	"agent.role":       func(f Facts) string { return f.AgentRole },
	"squad.name":       func(f Facts) string { return f.SquadName },
	"squad.roster":     func(f Facts) string { return strings.Join(f.SquadRoster, "; ") },
	"agent.resources":  func(f Facts) string { return strings.Join(f.Resources, "\n") },
	"agent.workspace":  func(f Facts) string { return f.Workspace },
	"platform.version": func(f Facts) string { return f.PlatformVer },
	"tools.enabled":    func(f Facts) string { return strings.Join(f.Tools, "\n") },
	"model.display":    func(f Facts) string { return f.ModelDisplay },
	"model.name":       func(f Facts) string { return f.ModelName },
	"model.provider":   func(f Facts) string { return f.ModelProvider },
	// model.context_window / model.supports_tools / model.fallback are
	// pre-rendered display strings ("128000 tokens", "supported", "none
	// configured") — the composer never interprets them.
	"model.context_window": func(f Facts) string { return f.ModelContextWindow },
	"model.supports_tools": func(f Facts) string { return f.ModelSupportsTools },
	"model.fallback":       func(f Facts) string { return f.ModelFallback },
	"platform.owner":       func(f Facts) string { return f.Owner },
}

// Compose builds the effective prompt from the four tiers.
//
// platformOverride may be empty, in which case the embedded platform prompt is
// used. User-editable tiers (org/squad/agent) are sanitized for reserved
// delimiters, then template variables are substituted, then per-tier and
// composed token caps are enforced. Empty user tiers are omitted from the
// output entirely.
func Compose(platformOverride, orgPrompt, squadPrompt, agentPrompt string, f Facts) (Composition, error) {
	caps := CapsFromEnv()

	platformText := platformOverride
	if strings.TrimSpace(platformText) == "" {
		platformText = EmbeddedPlatformPrompt()
	}

	raw := map[TierName]string{
		TierPlatform:     platformText,
		TierOrganization: orgPrompt,
		TierSquad:        squadPrompt,
		TierAgent:        agentPrompt,
	}

	var (
		composition Composition
		blocks      []string
	)

	for _, name := range tierOrder {
		content := raw[name]
		if strings.TrimSpace(content) == "" {
			continue
		}

		rendered, err := renderTierContent(name, content, f)
		if err != nil {
			return Composition{}, err
		}

		toks := EstimateTokens(rendered)
		cap := caps[name]
		tier := Tier{Name: name, Content: rendered, Tokens: toks, SoftWarn: cap.Soft, HardCap: cap.Hard}
		if toks > cap.Hard {
			return Composition{}, &CapError{Scope: string(name), Tokens: toks, Cap: cap.Hard}
		}
		if toks > cap.Soft {
			composition.Warnings = append(composition.Warnings,
				fmt.Sprintf("%s tier: %d tokens exceeds soft limit %d", name, toks, cap.Soft))
		}

		composition.Tiers = append(composition.Tiers, tier)
		blocks = append(blocks, RenderBlock(string(name), string(name), rendered))
	}

	composition.Prompt = strings.Join(blocks, "\n\n")
	composition.TotalToks = EstimateTokens(composition.Prompt)

	if composed := caps[TierComposed]; composition.TotalToks > composed.Hard {
		return Composition{}, &CapError{Scope: "composed", Tokens: composition.TotalToks, Cap: composed.Hard}
	} else if composition.TotalToks > composed.Soft {
		composition.Warnings = append(composition.Warnings,
			fmt.Sprintf("composed prompt: %d tokens exceeds soft limit %d", composition.TotalToks, composed.Soft))
	}

	sum := sha256.Sum256([]byte(composition.Prompt))
	composition.SHA256 = hex.EncodeToString(sum[:])

	return composition, nil
}

// renderTierContent renders a single tier's template, enforcing that
// user-editable tiers neither contain nor substitute reserved delimiters.
func renderTierContent(name TierName, content string, f Facts) (string, error) {
	// User-editable tiers must not forge reserved delimiters.
	if name != TierPlatform {
		if err := Sanitize(content); err != nil {
			return "", &ReservedTokensError{Tier: name}
		}
	}

	rendered, err := renderTemplates(content, f)
	if err != nil {
		return "", fmt.Errorf("%s tier: %w", name, err)
	}

	// Defensive re-check: substituted fact values must not smuggle
	// reserved delimiters either.
	if name != TierPlatform {
		if err := Sanitize(rendered); err != nil {
			return "", &ReservedTokensError{Tier: name}
		}
	}
	return rendered, nil
}

// RenderBlock wraps content in a trust-labelled XML-style block.
func RenderBlock(tag, trust, content string) string {
	return fmt.Sprintf("<skquad_%s trust=%q>\n%s\n</skquad_%s>", tag, trust, content, tag)
}
