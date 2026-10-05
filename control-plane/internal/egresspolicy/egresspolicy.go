// Package egresspolicy implements the governed egress plane's typed
// resource validation and the no-escalation grant validator
// (docs/tool-gateway.md §6–§8, TG-2).
//
// Two responsibilities:
//
//  1. Shape validation — endpoint_config and policy_ceiling are JSONB whose
//     accepted keys depend on the resource type (web/rest/mcp/git). Unknown
//     keys are rejected with structured violations so a typo can never
//     silently widen a policy.
//
//  2. The no-escalation invariant — a grant's constraints must be a
//     subset of the resource's policy_ceiling: allowlists only shrink,
//     denylists only grow, numeric bounds only tighten, egress_class must
//     match, and private-network/push flags can never be turned on beyond
//     the ceiling. Violations are returned as structured data (never just
//     a string) so API callers can render them.
//
// Wire shapes (snake_case, matching the design doc YAML):
//
//	web   config:  {deny_domains:[str], deny_cidrs:[str], rate_per_min:int,
//	               max_bytes:int, timeout_seconds:int(1..300),
//	               allow_private_network:bool}
//	web   ceiling: same shape as config
//	rest  config:  {base_url:str(required), auth_kind:none|bearer|api_key_header|
//	               basic|oauth2_client_credentials, header_name:str(api_key_header only)}
//	rest  ceiling: {methods:[GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS],
//	               path_allow:[glob], path_deny:[glob], max_request_bytes:int,
//	               max_response_bytes:int, rate_per_min:int, egress_class:public|internal}
//	mcp   config:  {url:str(required), auth_kind:none|bearer}
//	mcp   ceiling: {tools_allow:[glob], tools_deny:[glob],
//	               per_tool:{name:{requires_confirmation:bool}},
//	               rate_per_min:int, max_args_bytes:int}
//	git   config:  {base_url:str}
//	git   ceiling: {repos_allow:[glob], allow_push:bool, rate_per_min:int}
//
// endpoint_config NEVER carries secret material: credentials live in the
// resource's auth_ref (K8s Secret) and are resolved only at call time by
// the gateway. The policy endpoint strips defensively anyway.
package egresspolicy

// Violation is one structured validation failure.
type Violation struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Violations is a non-empty list of structured failures.
type Violations []Violation

func (v Violations) Error() string {
	if len(v) == 0 {
		return "no violations"
	}
	return v[0].Field + ": " + v[0].Message
}

// IsTyped reports whether a resource type carries egress endpoint_config /
// policy_ceiling semantics (web/rest/mcp/git). Legacy types (skill, tool,
// knowledge_base, project_workspace, ai_provider) do not.
func IsTyped(resourceType string) bool {
	switch resourceType {
	case "web", "rest", "mcp", "git":
		return true
	default:
		return false
	}
}
