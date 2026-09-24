package domain

import (
	"encoding/json"
	"fmt"
)

// ModelPricing holds the four per-1M-token rates of an AI Model
// (ADR-0010 D8). It is parsed from ai_models.pricing and snapshotted
// into metering rows at event time so historical cost never depends on
// live pricing.
type ModelPricing struct {
	InputPer1M       float64 `json:"input_per_1m"`
	CachedInputPer1M float64 `json:"cached_input_per_1m"`
	CacheWritePer1M  float64 `json:"cache_write_per_1m"`
	OutputPer1M      float64 `json:"output_per_1m"`
}

// ParseModelPricing decodes the ai_models.pricing JSON blob. A nil/empty
// blob is not an error: it yields nil (no pricing available).
func ParseModelPricing(raw json.RawMessage) (*ModelPricing, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var p ModelPricing
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("domain: parse model pricing: %w", err)
	}
	if p.InputPer1M < 0 || p.CachedInputPer1M < 0 || p.CacheWritePer1M < 0 || p.OutputPer1M < 0 {
		return nil, fmt.Errorf("domain: model pricing rates must not be negative")
	}
	return &p, nil
}

// CostFor computes the cost of a turn from the snapshot rates.
//
// NOTE (WP5 limitation): the gateway metering payload carries only
// input/output token counts — no cached-input or cache-write token counts —
// so the cached rates are snapshotted for audit but not applied to the
// computed cost. All input tokens are billed at the uncached input rate,
// which is the conservative (never-undercharging) choice. When the gateway
// starts reporting prompt_tokens_details.cached_tokens, this function
// should gain the cached split.
func (p *ModelPricing) CostFor(inputTokens, outputTokens int) float64 {
	if p == nil {
		return 0
	}
	const million = 1_000_000.0
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	return float64(inputTokens)/million*p.InputPer1M +
		float64(outputTokens)/million*p.OutputPer1M
}
