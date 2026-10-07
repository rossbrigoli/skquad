// TG-8 slice C2: typed confirmation-gate accessor over the grant's
// folded config/ceiling JSON (docs/tg8-grant-approvals-spec.md §C).
//
// The gate flag is `require_confirmation` (spec name). The pre-existing
// MCP per-tool shape `requires_confirmation` is reused as an alias —
// including the per-tool map — so existing ceiling shapes flow through
// the new gate unchanged. Evaluation is sticky-true across layers
// (config ∧ ceiling): either layer setting the flag true gates the
// call; nothing can loosen it below the grant floor. Default: false
// (ungated).
//
// Recognized shapes (in each of Config and Ceiling):
//
//	{"require_confirmation": true}
//	{"requires_confirmation": true}
//	{"per_tool": {"<tool>": {"require_confirmation": true}}}
//	{"per_tool": {"<tool>": {"requires_confirmation": true}}}
//
// Malformed layers are ignored (they never widen gating, and never
// un-gate a layer that already said true — same posture as the
// browser-driver selector).
package policy

import "encoding/json"

// confirmationFlagLayer is one JSON layer's confirmation flags. Pointers
// distinguish "absent" from "explicit false" so per-tool false does not
// cancel a top-level true from another layer.
type confirmationFlagLayer struct {
	RequireConfirmation  *bool `json:"require_confirmation"`
	RequiresConfirmation *bool `json:"requires_confirmation"`
	PerTool              map[string]struct {
		RequireConfirmation  *bool `json:"require_confirmation"`
		RequiresConfirmation *bool `json:"requires_confirmation"`
	} `json:"per_tool"`
}

func (l *confirmationFlagLayer) gated(tool string) bool {
	if orTrue(l.RequireConfirmation) || orTrue(l.RequiresConfirmation) {
		return true
	}
	if rule, ok := l.PerTool[tool]; ok {
		return orTrue(rule.RequireConfirmation) || orTrue(rule.RequiresConfirmation)
	}
	return false
}

func orTrue(p *bool) bool { return p != nil && *p }

// ConfirmationRequired reports whether the grant gates the dispatched
// tool/operation behind owner confirmation (TG-8 §C). Checks Config
// then Ceiling, sticky-true; unknown/malformed shapes default false.
func ConfirmationRequired(g *Grant, tool string) bool {
	if g == nil {
		return false
	}
	for _, raw := range []json.RawMessage{g.Config, g.Ceiling} {
		if len(raw) == 0 {
			continue
		}
		var layer confirmationFlagLayer
		if err := json.Unmarshal(raw, &layer); err != nil {
			continue
		}
		if layer.gated(tool) {
			return true
		}
	}
	return false
}
