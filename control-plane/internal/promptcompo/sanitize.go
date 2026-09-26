package promptcompo

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// reservedPattern matches any forged block delimiter: "<skquad_" or
// "</skquad_" (case-insensitive). Checking the prefix form covers both open
// and close tags, including split-token tricks like "<skquad_" + "platform>".
var reservedPattern = regexp.MustCompile(`(?i)</?skquad_`)

// templatePattern matches {{ variable }} references (optional inner spaces).
var templatePattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.]+)\s*\}\}`)

// Sanitize validates user-editable prompt text.
//
// Checks run on the NFKC-normalized form so homoglyph/compatibility-character
// tricks (e.g. fullwidth "＜ｓｋｑｕａｄ＿") fold to the same ASCII shape
// before the reserved-delimiter scan. Returns ReservedTokensError-style
// rejection via bool; callers wrap with tier context.
func Sanitize(userText string) error {
	normalized := norm.NFKC.String(userText)
	if reservedPattern.MatchString(normalized) {
		return &ReservedTokensError{Tier: TierName("user")}
	}
	// Reject stray "{{" that isn't a well-formed allowlisted variable only
	// at save time via ValidateTemplateVars; here we only guard delimiters.
	return nil
}

// ValidateTemplateVars returns the unknown {{...}} variables in text
// (empty slice = all known).
func ValidateTemplateVars(text string) []string {
	var unknown []string
	seen := map[string]bool{}
	for _, m := range templatePattern.FindAllStringSubmatch(text, -1) {
		v := m[1]
		if _, ok := templateVars[v]; !ok && !seen[v] {
			unknown = append(unknown, v)
			seen[v] = true
		}
	}
	return unknown
}

// renderTemplates substitutes allowlisted variables. Unknown variables are a
// hard error (fail closed — a typo must not silently drop context).
func renderTemplates(text string, f Facts) (string, error) {
	if unknown := ValidateTemplateVars(text); len(unknown) > 0 {
		return "", &UnknownTemplateVarsError{Vars: unknown}
	}
	return templatePattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := templatePattern.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		if fn, ok := templateVars[sub[1]]; ok {
			return fn(f)
		}
		return m
	}), nil
}

// ContainsOnlyPrintable is a soft helper for UI-side validation: reports
// whether text contains control characters other than \n and \t.
func ContainsOnlyPrintable(text string) bool {
	for _, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// BlockTagFor returns the block tag for a tier (e.g. "skquad_platform").
func BlockTagFor(name TierName) string {
	return "skquad_" + strings.ToLower(string(name))
}
