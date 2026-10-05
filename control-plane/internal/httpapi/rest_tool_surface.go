package httpapi

// TG-4 (S-250): rest_call tool surface for fetch-at-wake discovery.
//
// docs/tool-gateway.md §6.2 defines the agent-facing tool
// `rest_call(resource_id, method, path, headers?, body?)`. rest_call is
// NOT a builtin: the builtin universe (builtin_tools.go /
// GET /agents/me/tools) is unchanged. Instead, the tool appears in the
// typed discovery response (GET /agents/me/resources) as a resource-
// scoped `tools` entry ONLY when the agent holds an active rest grant —
// absent otherwise, which is the fetch-at-wake contract: the runtime
// reads its tool surface fresh at wake from this endpoint.
//
// The effective bounds advertised here mirror the gateway's
// rest.EffectivePolicy folding (tool-gateway/internal/drivers/rest/
// policy.go) exactly: methods intersect across layers (default-deny when
// the ceiling sets none), grant path_allow wins over the ceiling's,
// path_deny unions, numerics take the tightest (min) value. What the
// agent sees is what the gateway enforces.

import (
	"encoding/json"
	"strings"
)

// restCallToolName is the single operation published for rest resources.
const restCallToolName = "rest_call"

// restDefaultMaxRequestBytes / restDefaultMaxResponseBytes mirror the
// gateway driver defaults (tool-gateway rest.DefaultMax*Bytes). Kept as
// local constants so discovery never needs to import the gateway module.
const (
	restDefaultMaxRequestBytes  = 65536  // 64 KiB
	restDefaultMaxResponseBytes = 262144 // 256 KiB
)

// restAllowedAgentHeaders mirrors the gateway's agent-controlled header
// allowlist (drivers/rest allowedAgentHeaders). Published so the runtime
// can pre-validate before a guaranteed header_denied.
var restAllowedAgentHeaders = []string{"accept", "content-type"}

// restCallToolSchema builds the `tools` JSON array surfaced on a rest
// resource entry in agentRuntimeResource. ceilingRaw/constraintsRaw are
// the resource's policy_ceiling and the grant's constraints (both
// validated at write time; parse failures degrade to omitting the tool
// rather than breaking discovery — the gateway still fails closed).
func restCallToolSchema(resourceName, resourceID string, ceilingRaw, constraintsRaw json.RawMessage) json.RawMessage {
	ceiling := parseRestPolicyLayer(ceilingRaw)
	constraints := parseRestPolicyLayer(constraintsRaw)

	methods := upperListPtr(ceiling.methods)
	if constraints.methods != nil {
		methods = intersectStrings(methods, upperListPtr(constraints.methods))
	}

	pathAllow := []string{}
	switch {
	case constraints.pathAllow != nil:
		pathAllow = *constraints.pathAllow
	case ceiling.pathAllow != nil:
		pathAllow = *ceiling.pathAllow
	}

	pathDeny := append(append([]string{}, ceiling.pathDeny...), constraints.pathDeny...)

	maxReq := restDefaultMaxRequestBytes
	for _, v := range []int{ceiling.maxRequestBytes, constraints.maxRequestBytes} {
		if v > 0 && v < maxReq {
			maxReq = v
		}
	}
	maxResp := restDefaultMaxResponseBytes
	for _, v := range []int{ceiling.maxResponseBytes, constraints.maxResponseBytes} {
		if v > 0 && v < maxResp {
			maxResp = v
		}
	}
	rate := 0
	for _, v := range []int{ceiling.ratePerMin, constraints.ratePerMin} {
		if v > 0 && (rate == 0 || v < rate) {
			rate = v
		}
	}

	out, err := json.Marshal([]map[string]any{buildRestCallTool(resourceName, resourceID, methods, pathAllow, pathDeny, maxReq, maxResp, rate)})
	if err != nil {
		return nil
	}
	return out
}

func buildRestCallTool(resourceName, resourceID string, methods, pathAllow, pathDeny []string, maxReq, maxResp, rate int) map[string]any {
	desc := "Call the registered REST resource \"" + resourceName + "\" through the tool gateway. " +
		"Credentials are injected at the gateway (never visible to the agent); every call is policy-checked and audited. " +
		"Gateway endpoint: POST /v1/rest/" + resourceID + " (agent-credential auth)."
	methodEnum := methods
	if len(methodEnum) == 0 {
		methodEnum = []string{}
	}
	return map[string]any{
		"name":        restCallToolName,
		"description": desc,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource_id": map[string]any{
					"type":  "string",
					"const": resourceID,
				},
				"method": map[string]any{
					"type":        "string",
					"enum":        methodEnum,
					"description": "HTTP method; must be within the effective ceiling ∧ grant.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Relative path under the resource base_url (query string allowed). Must match an allowed pattern and no denied pattern; absolute URLs and traversal are rejected.",
				},
				"headers": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Optional. Only these agent-controlled headers are accepted; anything else is denied.",
				},
				"body": map[string]any{
					"type":        "string",
					"description": "Optional request body.",
				},
			},
			"required":             []string{"resource_id", "method", "path"},
			"additionalProperties": false,
		},
		"constraints": map[string]any{
			"methods":            methodEnum,
			"path_allow":         pathAllow,
			"path_deny":          pathDeny,
			"max_request_bytes":  maxReq,
			"max_response_bytes": maxResp,
			"rate_per_min":       rate,
			"allowed_headers":    restAllowedAgentHeaders,
		},
	}
}

// restPolicyLayer is a minimal parse of one policy layer (ceiling or
// grant constraints). Pointer slices distinguish unset from empty,
// mirroring the gateway's restPolicyShape.
type restPolicyLayer struct {
	methods          *[]string
	pathAllow        *[]string
	pathDeny         []string
	maxRequestBytes  int
	maxResponseBytes int
	ratePerMin       int
}

func parseRestPolicyLayer(raw json.RawMessage) restPolicyLayer {
	var layer restPolicyLayer
	if len(raw) == 0 || string(raw) == "null" {
		return layer
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return layer
	}
	layer.methods = stringListPointer(obj, "methods")
	layer.pathAllow = stringListPointer(obj, "path_allow")
	layer.pathDeny = stringList(obj, "path_deny")
	layer.maxRequestBytes = intField(obj, "max_request_bytes")
	layer.maxResponseBytes = intField(obj, "max_response_bytes")
	layer.ratePerMin = intField(obj, "rate_per_min")
	return layer
}

func stringListPointer(obj map[string]json.RawMessage, key string) *[]string {
	raw, ok := obj[key]
	if !ok {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return &list
}

func stringList(obj map[string]json.RawMessage, key string) []string {
	if list := stringListPointer(obj, key); list != nil {
		return *list
	}
	return nil
}

func intField(obj map[string]json.RawMessage, key string) int {
	raw, ok := obj[key]
	if !ok {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return n
}

func upperList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, strings.ToUpper(strings.TrimSpace(v)))
	}
	return out
}

// upperListPtr uppercases a possibly-unset list; nil stays nil so callers
// can distinguish "layer did not set it" from "layer set it empty".
func upperListPtr(in *[]string) []string {
	if in == nil {
		return nil
	}
	return upperList(*in)
}

func intersectStrings(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	out := []string{}
	for _, v := range a {
		if set[v] {
			out = append(out, v)
		}
	}
	return out
}
