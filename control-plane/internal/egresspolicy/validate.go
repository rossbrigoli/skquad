package egresspolicy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// object decodes raw JSON into a key map. Empty/absent raw is treated as an
// empty object. Non-object JSON is a structured violation.
func object(field string, raw json.RawMessage) (map[string]json.RawMessage, Violations) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, Violations{{Field: field, Code: "invalid_json", Message: field + " must be a JSON object"}}
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// checkUnknownKeys reports every key not present in allowed.
func checkUnknownKeys(field string, obj map[string]json.RawMessage, allowed ...string) Violations {
	allowedSet := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allowedSet[k] = true
	}
	var v Violations
	for k := range obj {
		if !allowedSet[k] {
			v = append(v, Violation{
				Field:   field + "." + k,
				Code:    "unknown_key",
				Message: fmt.Sprintf("unknown key %q for %s (allowed: %s)", k, field, strings.Join(allowed, ", ")),
			})
		}
	}
	return sortByField(v)
}

// stringField looks up `key` in obj; `path` is the display path used in
// violations.
func stringField(obj map[string]json.RawMessage, key, path string, out *string, required bool, allowed map[string]bool) Violations {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		if required {
			return Violations{{Field: path, Code: "required", Message: path + " is required"}}
		}
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return Violations{{Field: path, Code: "invalid_type", Message: path + " must be a string"}}
	}
	if allowed != nil && !allowed[*out] {
		return Violations{{Field: path, Code: "invalid_value", Message: fmt.Sprintf("%s must be one of: %s", path, sortedKeys(allowed))}}
	}
	return nil
}

func stringListField(obj map[string]json.RawMessage, key, path string, out *[]string) Violations {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return Violations{{Field: path, Code: "invalid_type", Message: path + " must be an array of strings"}}
	}
	for i, s := range *out {
		if strings.TrimSpace(s) == "" {
			return Violations{{Field: fmt.Sprintf("%s[%d]", path, i), Code: "invalid_value", Message: path + " entries must be non-empty"}}
		}
	}
	return nil
}

func positiveIntField(obj map[string]json.RawMessage, key, path string, out *int) Violations {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return Violations{{Field: path, Code: "invalid_type", Message: path + " must be an integer"}}
	}
	if *out <= 0 {
		return Violations{{Field: path, Code: "invalid_value", Message: path + " must be > 0"}}
	}
	return nil
}

func boolField(obj map[string]json.RawMessage, key, path string, out *bool) Violations {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return Violations{{Field: path, Code: "invalid_type", Message: path + " must be a boolean"}}
	}
	return nil
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return strings.Join(keys, ", ")
}

func sortByField(v Violations) Violations {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j].Field < v[j-1].Field; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	return v
}

var (
	httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}
	authKinds   = map[string]bool{"none": true, "bearer": true, "api_key_header": true, "basic": true, "oauth2_client_credentials": true}
	mcpAuth     = map[string]bool{"none": true, "bearer": true}
	egressClass = map[string]bool{"public": true, "internal": true}
)

func validHTTPURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

func validCIDR(value string) bool {
	_, _, err := net.ParseCIDR(value)
	return err == nil
}

// Shape key lists (exported for docs/tests).
var (
	WebShapeKeys    = []string{"deny_domains", "deny_cidrs", "rate_per_min", "max_bytes", "timeout_seconds", "allow_private_network"}
	RestConfigKeys  = []string{"base_url", "auth_kind", "header_name"}
	RestCeilingKeys = []string{"methods", "path_allow", "path_deny", "max_request_bytes", "max_response_bytes", "rate_per_min", "egress_class"}
	MCPConfigKeys   = []string{"url", "auth_kind"}
	MCPCeilingKeys  = []string{"tools_allow", "tools_deny", "per_tool", "rate_per_min", "max_args_bytes"}
	GitConfigKeys   = []string{"base_url"}
	GitCeilingKeys  = []string{"repos_allow", "allow_push", "rate_per_min"}
)

// ValidateEndpointConfig validates a resource's endpoint_config for its
// type. Legacy (untyped) resources accept any object.
func ValidateEndpointConfig(resourceType string, raw json.RawMessage) Violations {
	obj, v := object("endpoint_config", raw)
	if len(v) > 0 {
		return v
	}
	switch resourceType {
	case "web":
		return validateWebShape("endpoint_config", obj)
	case "rest":
		return validateRestConfig(obj)
	case "mcp":
		return validateMCPConfig(obj)
	case "git":
		return checkUnknownKeys("endpoint_config", obj, GitConfigKeys...)
	default:
		return nil
	}
}

// ValidateCeiling validates a resource's policy_ceiling for its type.
func ValidateCeiling(resourceType string, raw json.RawMessage) Violations {
	obj, v := object("policy_ceiling", raw)
	if len(v) > 0 {
		return v
	}
	switch resourceType {
	case "web":
		return validateWebShape("policy_ceiling", obj)
	case "rest":
		return validateRestCeiling(obj)
	case "mcp":
		return validateMCPCeiling(obj)
	case "git":
		return validateGitCeiling(obj)
	default:
		return nil
	}
}

func validateWebShape(field string, obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys(field, obj, WebShapeKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var denyDomains, denyCIDRs []string
	v = append(v, stringListField(obj, "deny_domains", field+".deny_domains", &denyDomains)...)
	v = append(v, stringListField(obj, "deny_cidrs", field+".deny_cidrs", &denyCIDRs)...)
	for i, c := range denyCIDRs {
		if !validCIDR(c) {
			v = append(v, Violation{Field: fmt.Sprintf("%s.deny_cidrs[%d]", field, i), Code: "invalid_value", Message: "not a valid CIDR"})
		}
	}
	var rate, maxBytes int
	v = append(v, positiveIntField(obj, "rate_per_min", field+".rate_per_min", &rate)...)
	v = append(v, positiveIntField(obj, "max_bytes", field+".max_bytes", &maxBytes)...)
	var timeoutSec int
	v = append(v, positiveIntField(obj, "timeout_seconds", field+".timeout_seconds", &timeoutSec)...)
	if timeoutSec > 300 {
		v = append(v, Violation{Field: field + ".timeout_seconds", Code: "invalid_value", Message: "timeout_seconds must be <= 300"})
	}
	var allowPrivate bool
	v = append(v, boolField(obj, "allow_private_network", field+".allow_private_network", &allowPrivate)...)
	return sortByField(v)
}

func validateRestConfig(obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("endpoint_config", obj, RestConfigKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var baseURL string
	v = append(v, stringField(obj, "base_url", "endpoint_config.base_url", &baseURL, true, nil)...)
	if baseURL != "" && !validHTTPURL(baseURL) {
		v = append(v, Violation{Field: "endpoint_config.base_url", Code: "invalid_value", Message: "base_url must be an absolute http(s) URL"})
	}
	var authKind string
	v = append(v, stringField(obj, "auth_kind", "endpoint_config.auth_kind", &authKind, false, authKinds)...)
	_, hasHeader := obj["header_name"]
	if hasHeader && authKind != "api_key_header" {
		v = append(v, Violation{Field: "endpoint_config.header_name", Code: "invalid_value", Message: "header_name is only valid with auth_kind=api_key_header"})
	}
	return sortByField(v)
}

func validateRestCeiling(obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("policy_ceiling", obj, RestCeilingKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var methods []string
	v = append(v, stringListField(obj, "methods", "policy_ceiling.methods", &methods)...)
	for i, m := range methods {
		if !httpMethods[strings.ToUpper(m)] {
			v = append(v, Violation{Field: fmt.Sprintf("policy_ceiling.methods[%d]", i), Code: "invalid_value", Message: "unsupported HTTP method"})
		}
	}
	var pathAllow, pathDeny []string
	v = append(v, stringListField(obj, "path_allow", "policy_ceiling.path_allow", &pathAllow)...)
	v = append(v, stringListField(obj, "path_deny", "policy_ceiling.path_deny", &pathDeny)...)
	var reqBytes, respBytes, rate int
	v = append(v, positiveIntField(obj, "max_request_bytes", "policy_ceiling.max_request_bytes", &reqBytes)...)
	v = append(v, positiveIntField(obj, "max_response_bytes", "policy_ceiling.max_response_bytes", &respBytes)...)
	v = append(v, positiveIntField(obj, "rate_per_min", "policy_ceiling.rate_per_min", &rate)...)
	var ec string
	v = append(v, stringField(obj, "egress_class", "policy_ceiling.egress_class", &ec, false, egressClass)...)
	return sortByField(v)
}

func validateMCPConfig(obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("endpoint_config", obj, MCPConfigKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var u string
	v = append(v, stringField(obj, "url", "endpoint_config.url", &u, true, nil)...)
	if u != "" && !validHTTPURL(u) {
		v = append(v, Violation{Field: "endpoint_config.url", Code: "invalid_value", Message: "url must be an absolute http(s) URL"})
	}
	var authKind string
	v = append(v, stringField(obj, "auth_kind", "endpoint_config.auth_kind", &authKind, false, mcpAuth)...)
	return sortByField(v)
}

func validateMCPCeiling(obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("policy_ceiling", obj, MCPCeilingKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var allow, deny []string
	v = append(v, stringListField(obj, "tools_allow", "policy_ceiling.tools_allow", &allow)...)
	v = append(v, stringListField(obj, "tools_deny", "policy_ceiling.tools_deny", &deny)...)
	var rate, maxArgs int
	v = append(v, positiveIntField(obj, "rate_per_min", "policy_ceiling.rate_per_min", &rate)...)
	v = append(v, positiveIntField(obj, "max_args_bytes", "policy_ceiling.max_args_bytes", &maxArgs)...)
	if raw, ok := obj["per_tool"]; ok {
		var perTool map[string]json.RawMessage
		if err := json.Unmarshal(raw, &perTool); err != nil {
			v = append(v, Violation{Field: "policy_ceiling.per_tool", Code: "invalid_type", Message: "per_tool must be an object keyed by tool name"})
		} else {
			for name, traw := range perTool {
				var tObj map[string]json.RawMessage
				if err := json.Unmarshal(traw, &tObj); err != nil {
					v = append(v, Violation{Field: "policy_ceiling.per_tool." + name, Code: "invalid_type", Message: "per_tool entry must be an object"})
					continue
				}
				v = append(v, checkUnknownKeys("policy_ceiling.per_tool."+name, tObj, "requires_confirmation")...)
				var rc bool
				v = append(v, boolField(tObj, "requires_confirmation", "policy_ceiling.per_tool."+name+".requires_confirmation", &rc)...)
			}
		}
	}
	return sortByField(v)
}

func validateGitCeiling(obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("policy_ceiling", obj, GitCeilingKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var repos []string
	v = append(v, stringListField(obj, "repos_allow", "policy_ceiling.repos_allow", &repos)...)
	// TG-4b: repos_allow is REQUIRED and non-empty on a git ceiling.
	// Default-deny is only meaningful when the admin explicitly
	// enumerated the reachable repos; an unset list would leave the
	// resource reachable-but-unusable and invites "it validated, so
	// it must be allowed" confusion at grant time.
	if len(repos) == 0 {
		v = append(v, Violation{Field: "policy_ceiling.repos_allow", Code: "required", Message: "repos_allow is required and must contain at least one repo pattern (default-deny semantics)"})
	}
	var allowPush bool
	v = append(v, boolField(obj, "allow_push", "policy_ceiling.allow_push", &allowPush)...)
	var rate int
	v = append(v, positiveIntField(obj, "rate_per_min", "policy_ceiling.rate_per_min", &rate)...)
	return sortByField(v)
}
