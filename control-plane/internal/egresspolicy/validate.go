package egresspolicy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
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
	mcpDrivers  = map[string]bool{"browser": true, "mcp": true}
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
	MCPConfigKeys   = []string{"url", "auth_kind", "driver"}
	// TG-5 slice B2a: egress_class joins the MCP ceiling shape — the
	// gateway's mcp policy folds it into AllowPrivate (internal-class
	// upstreams need allow_private at enumerate/call time), matching the
	// §6.3 ceiling example.
	MCPCeilingKeys = []string{"tools_allow", "tools_deny", "per_tool", "rate_per_min", "max_args_bytes", "egress_class"}
	GitConfigKeys  = []string{"base_url"}
	GitCeilingKeys = []string{"repos_allow", "allow_push", "rate_per_min"}
	// TG-10 ssh (Terminal-as-a-Service, §6.6): endpoint_config carries
	// the non-secret connection shape; the private key lives in credential
	// custody (kind "ssh_key"), never in endpoint_config.
	SSHConfigKeys  = []string{"ssh_user", "port", "known_hosts", "auth_mode", "artifact"}
	SSHCeilingKeys = []string{"hosts_allow", "hosts_deny", "command_allow", "command_deny", "cert_ttl_minutes", "max_concurrent_sessions", "exec_timeout_seconds", "egress_class", "host_groups", "mirrors_allow", "require_tip"}
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
	case "ssh":
		return validateSSHConfig(obj)
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
	case "ssh":
		return validateSSHCeiling(obj)
	default:
		return nil
	}
}

// validateSSHConfig validates the ssh endpoint_config shape:
//
//	ssh_user    required, non-empty — the OS user the terminal-service connects as
//	port        optional, 1..65535 (default 22 at the service)
//	known_hosts required — host-key material; unverified host keys are never accepted
//	auth_mode   optional, "ca" (default, ephemeral CA certs) | "static_key" (BYO key custody)
func validateSSHConfig(obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("endpoint_config", obj, SSHConfigKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var user string
	v = append(v, stringField(obj, "ssh_user", "endpoint_config.ssh_user", &user, true, nil)...)
	if strings.TrimSpace(user) == "" {
		v = append(v, Violation{Field: "endpoint_config.ssh_user", Code: "required", Message: "ssh_user is required and must be non-empty"})
	}
	var port int
	v = append(v, positiveIntField(obj, "port", "endpoint_config.port", &port)...)
	if port > 65535 {
		v = append(v, Violation{Field: "endpoint_config.port", Code: "invalid_value", Message: "port must be <= 65535"})
	}
	var kh string
	v = append(v, stringField(obj, "known_hosts", "endpoint_config.known_hosts", &kh, true, nil)...)
	if strings.TrimSpace(kh) == "" {
		v = append(v, Violation{Field: "endpoint_config.known_hosts", Code: "required", Message: "known_hosts is required (host key verification is mandatory)"})
	}
	var authMode string
	v = append(v, stringField(obj, "auth_mode", "endpoint_config.auth_mode", &authMode, false, map[string]bool{"ca": true, "static_key": true})...)
	// TG-11 §6.7: optional artifact section turns this ssh resource into an
	// artifact-executor target (approved-playbook apply).
	if raw, ok := obj["artifact"]; ok {
		v = append(v, validateArtifactConfig(raw)...)
	}
	return sortByField(v)
}

// validateArtifactConfig validates the TG-11 §6.7 artifact section of an
// ssh endpoint_config:
//
//	git_url         required — the ONLY repo the executor may clone (tool
//	                calls cannot point it elsewhere)
//	default_branch  optional (default "main") — merge-approval anchor
//	playbooks_path  optional (default "playbooks/") — subtree the agent
//	                may reference; must be relative and traversal-free
func validateArtifactConfig(raw json.RawMessage) Violations {
	obj, v := object("endpoint_config.artifact", raw)
	if len(v) > 0 {
		return v
	}
	if v := checkUnknownKeys("endpoint_config.artifact", obj, "git_url", "default_branch", "playbooks_path", "drift_playbook"); len(v) > 0 {
		return v
	}
	var gitURL string
	v = append(v, stringField(obj, "git_url", "endpoint_config.artifact.git_url", &gitURL, true, nil)...)
	if strings.TrimSpace(gitURL) == "" || !validHTTPURL(gitURL) {
		v = append(v, Violation{Field: "endpoint_config.artifact.git_url", Code: "invalid_value", Message: "git_url is required and must be a valid http(s) URL"})
	}
	var branch string
	v = append(v, stringField(obj, "default_branch", "endpoint_config.artifact.default_branch", &branch, false, nil)...)
	if _, ok := obj["default_branch"]; ok && strings.TrimSpace(branch) == "" {
		v = append(v, Violation{Field: "endpoint_config.artifact.default_branch", Code: "invalid_value", Message: "default_branch must be non-empty when present"})
	}
	var path string
	v = append(v, stringField(obj, "playbooks_path", "endpoint_config.artifact.playbooks_path", &path, false, nil)...)
	if _, ok := obj["playbooks_path"]; ok {
		if strings.TrimSpace(path) == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			v = append(v, Violation{Field: "endpoint_config.artifact.playbooks_path", Code: "invalid_value", Message: "playbooks_path must be a relative, traversal-free path"})
		}
	}
	// TG-11 slice D: optional drift_playbook — the entry playbook the
	// periodic drift-check runs for this resource (falls back to the
	// runner's SKQUAD_DRIFT_PLAYBOOK default when unset). Same relative,
	// traversal-free rule as playbooks_path.
	var driftPB string
	v = append(v, stringField(obj, "drift_playbook", "endpoint_config.artifact.drift_playbook", &driftPB, false, nil)...)
	if _, ok := obj["drift_playbook"]; ok {
		if strings.TrimSpace(driftPB) == "" || strings.HasPrefix(driftPB, "/") || strings.Contains(driftPB, "..") {
			v = append(v, Violation{Field: "endpoint_config.artifact.drift_playbook", Code: "invalid_value", Message: "drift_playbook must be a relative, traversal-free path"})
		}
	}
	return sortByField(v)
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
	// TG-6 slice D: driver selects the gateway driver for this mcp
	// registration. "browser" routes to the TG-6 browser driver; "mcp"
	// is the explicit default. Anything else is a typo, not a driver.
	var driver string
	v = append(v, stringField(obj, "driver", "endpoint_config.driver", &driver, false, mcpDrivers)...)
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
	var ec string
	v = append(v, stringField(obj, "egress_class", "policy_ceiling.egress_class", &ec, false, egressClass)...)
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

// validateSSHCeiling validates the ssh policy_ceiling shape (§6.6):
//
//	hosts_allow  REQUIRED non-empty — default-deny host globs
//	hosts_deny   optional globs, union, wins over allow
//	command_allow optional globs; when set, a command must match one
//	command_deny optional globs; deny wins over allow
//	cert_ttl_minutes        15..60 (short-lived CA certs; §6.6)
//	max_concurrent_sessions > 0, <= 16
//	exec_timeout_seconds    > 0, <= 300
//	egress_class one of the standard classes
func validateSSHCeiling(obj map[string]json.RawMessage) Violations {
	if v := checkUnknownKeys("policy_ceiling", obj, SSHCeilingKeys...); len(v) > 0 {
		return v
	}
	var v Violations
	var hostsAllow, hostsDeny, cmdAllow, cmdDeny []string
	v = append(v, stringListField(obj, "hosts_allow", "policy_ceiling.hosts_allow", &hostsAllow)...)
	if len(hostsAllow) == 0 {
		v = append(v, Violation{Field: "policy_ceiling.hosts_allow", Code: "required", Message: "hosts_allow is required and must contain at least one host pattern (default-deny semantics)"})
	}
	v = append(v, stringListField(obj, "hosts_deny", "policy_ceiling.hosts_deny", &hostsDeny)...)
	v = append(v, stringListField(obj, "command_allow", "policy_ceiling.command_allow", &cmdAllow)...)
	v = append(v, stringListField(obj, "command_deny", "policy_ceiling.command_deny", &cmdDeny)...)
	var ttl int
	v = append(v, positiveIntField(obj, "cert_ttl_minutes", "policy_ceiling.cert_ttl_minutes", &ttl)...)
	if ttl > 0 && (ttl < 15 || ttl > 60) {
		v = append(v, Violation{Field: "policy_ceiling.cert_ttl_minutes", Code: "invalid_value", Message: "cert_ttl_minutes must be between 15 and 60"})
	}
	var maxSess int
	v = append(v, positiveIntField(obj, "max_concurrent_sessions", "policy_ceiling.max_concurrent_sessions", &maxSess)...)
	if maxSess > 16 {
		v = append(v, Violation{Field: "policy_ceiling.max_concurrent_sessions", Code: "invalid_value", Message: "max_concurrent_sessions must be <= 16"})
	}
	var timeout int
	v = append(v, positiveIntField(obj, "exec_timeout_seconds", "policy_ceiling.exec_timeout_seconds", &timeout)...)
	if timeout > 300 {
		v = append(v, Violation{Field: "policy_ceiling.exec_timeout_seconds", Code: "invalid_value", Message: "exec_timeout_seconds must be <= 300"})
	}
	var ec string
	v = append(v, stringField(obj, "egress_class", "policy_ceiling.egress_class", &ec, false, egressClass)...)
	// TG-11 §6.7: host_groups — admin-managed, versioned inventory. Every
	// host in a group MUST be covered by hosts_allow: groups narrow the
	// approved surface, they can never widen it past the ceiling.
	if raw, ok := obj["host_groups"]; ok {
		v = append(v, validateHostGroups(raw, hostsAllow)...)
	}
	var mirrors []string
	v = append(v, stringListField(obj, "mirrors_allow", "policy_ceiling.mirrors_allow", &mirrors)...)
	// require_tip: tier → bool (e.g. {"high": true}). High-tier applies
	// must use the current default-branch tip, not a merged-but-superseded
	// revision. Unknown tiers or non-bool values are refused.
	if raw, ok := obj["require_tip"]; ok {
		tipObj, tv := object("policy_ceiling.require_tip", raw)
		if len(tv) > 0 {
			return sortByField(append(v, tv...))
		}
		for k := range tipObj {
			if k != "low" && k != "medium" && k != "high" {
				v = append(v, Violation{Field: "policy_ceiling.require_tip." + k, Code: "invalid_value", Message: "require_tip keys must be low|medium|high"})
			}
			var b bool
			v = append(v, boolField(tipObj, k, "policy_ceiling.require_tip."+k, &b)...)
		}
	}
	return sortByField(v)
}

// validateHostGroups validates policy_ceiling.host_groups against the
// ceiling's hosts_allow patterns. Semantics mirror the gateway's ssh
// globMatch (tool-gateway/internal/drivers/ssh/policy.go): '*' matches
// any run of characters, '?' exactly one; whole-string anchored.
func validateHostGroups(raw json.RawMessage, hostsAllow []string) Violations {
	groups, v := object("policy_ceiling.host_groups", raw)
	if len(v) > 0 {
		return v
	}
	if len(groups) == 0 {
		return Violations{{Field: "policy_ceiling.host_groups", Code: "required", Message: "host_groups must contain at least one group when present"}}
	}
	for name, graw := range groups {
		if name == "" || !hostGroupNameRe.MatchString(name) {
			v = append(v, Violation{Field: "policy_ceiling.host_groups." + name, Code: "invalid_value", Message: "group name must match [a-z0-9_-]+"})
		}
		gobj, gv := object("policy_ceiling.host_groups."+name, graw)
		if len(gv) > 0 {
			v = append(v, gv...)
			continue
		}
		if uv := checkUnknownKeys("policy_ceiling.host_groups."+name, gobj, "hosts", "tier"); len(uv) > 0 {
			v = append(v, uv...)
			continue
		}
		var hosts []string
		v = append(v, stringListField(gobj, "hosts", "policy_ceiling.host_groups."+name+".hosts", &hosts)...)
		if len(hosts) == 0 {
			v = append(v, Violation{Field: "policy_ceiling.host_groups." + name + ".hosts", Code: "required", Message: "group must contain at least one host"})
		}
		var tier string
		if tv := stringField(gobj, "tier", "policy_ceiling.host_groups."+name+".tier", &tier, true, map[string]bool{"low": true, "medium": true, "high": true}); len(tv) > 0 {
			v = append(v, tv...)
		}
		for _, h := range hosts {
			covered := false
			for _, p := range hostsAllow {
				if sshHostGlobMatch(p, h) {
					covered = true
					break
				}
			}
			if !covered {
				v = append(v, Violation{Field: "policy_ceiling.host_groups." + name + ".hosts", Code: "outside_hosts_allow", Message: "host " + h + " is not covered by any hosts_allow pattern — groups cannot widen the ceiling"})
			}
		}
	}
	return v
}

var hostGroupNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// sshHostGlobMatch mirrors the gateway's anchored glob semantics for
// hosts_allow patterns (see tool-gateway/internal/drivers/ssh/policy.go
// globMatch): '*' = any run, '?' = exactly one, whole-string anchored,
// case-insensitive. Duplicated (not imported) so the control plane never
// depends on the gateway module.
func sshHostGlobMatch(pattern, host string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	h := strings.ToLower(strings.TrimSpace(host))
	return anchoredGlob(p, h)
}

func anchoredGlob(pattern, s string) bool {
	// Iterative wildcard match (no regex compile per call).
	pi, si := 0, 0
	star, starSi := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			starSi = si
			pi++
		case star >= 0:
			pi = star + 1
			starSi++
			si = starSi
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
