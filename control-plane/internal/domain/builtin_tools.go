package domain

// Built-in platform tools (BT-2, ADR-0012). The tool names are the
// complete universe enforced by the builtin_tools_config CHECK constraint;
// policy validation below mirrors the schemas pinned in ADR-0012 §3.
// send_message was added for S-164 (agent-to-agent messaging within the
// squad); migration 0023 widens the constraint and seeds it ENABLED —
// unlike the three original builtins, intra-squad messaging is the core
// squad primitive and is governed control-plane-side (same-squad always
// allowed, cross-squad needs access grants).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
)

const (
	BuiltinToolExec         = "exec"
	BuiltinToolWebFetch     = "web_fetch"
	BuiltinToolWebSearch    = "web_search"
	BuiltinToolSendMessage  = "send_message"
	BuiltinToolSendInbox    = "send_inbox"
	BuiltinToolNotifyOwner  = "notify_owner"
	BuiltinToolMemorySearch = "memory_search"
)

// BuiltinToolNames lists the built-in tools in canonical order.
var BuiltinToolNames = []string{BuiltinToolExec, BuiltinToolWebFetch, BuiltinToolWebSearch, BuiltinToolSendMessage, BuiltinToolSendInbox, BuiltinToolNotifyOwner, BuiltinToolMemorySearch}

// BuiltinToolDefaultEnabled reports whether a built-in ships enabled when
// seeded. The original three are security-sensitive and ship disabled
// (ADR-0012 §1). send_message ships enabled: it is the S-164 squad
// primitive, its blast radius is bounded by squad isolation plus the
// correlation-chain budget, and disabling it by default would make the
// card's feature invisible until an admin noticed the toggle.
// notify_owner ships enabled for the same reason: its blast radius is
// one capped-length action_required row in the agent's own squad owner's
// inbox — it triggers no auto-action and reaches nobody else. send_inbox
// ships enabled to match its S-193 seed (migration 0029): same posture,
// one capped agent_message row in the agent's own squad owner's inbox.
// memory_search ships enabled to match its S-212 seed (migration 0035):
// recall is read-only, hard-scoped to the calling agent's own
// agent_memory rows, excludes rejected memories, and additionally
// no-ops server-side unless SKQUAD_MEMORY_EMBEDDINGS_ENABLED=true.
func BuiltinToolDefaultEnabled(name string) bool {
	return name == BuiltinToolSendMessage || name == BuiltinToolSendInbox || name == BuiltinToolNotifyOwner || name == BuiltinToolMemorySearch
}

// SearchProviderNames lists the accepted web_search policy providers.
// duckduckgo is keyless and the default; brave/perplexity need a
// control-plane-side API key.
var SearchProviderNames = []string{"duckduckgo", "brave", "perplexity"}

// BuiltinToolConfig is one row of builtin_tools_config: the platform
// admin's enablement + validated policy JSON for a built-in tool.
type BuiltinToolConfig struct {
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Policy    json.RawMessage `json:"policy"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

// IsBuiltinToolName reports whether name is one of the built-in tools.
func IsBuiltinToolName(name string) bool {
	for _, n := range BuiltinToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// IsSearchProvider reports whether provider is an accepted web_search
// policy provider.
func IsSearchProvider(provider string) bool {
	for _, p := range SearchProviderNames {
		if p == provider {
			return true
		}
	}
	return false
}

// ValidateBuiltinPolicy checks a tool's policy JSON against the schema
// pinned in ADR-0012 §3. It returns field-level error messages (empty =
// valid). Unknown keys are rejected so a typo can never silently widen or
// narrow a tool's behaviour.
func ValidateBuiltinPolicy(name string, policy json.RawMessage) []string {
	if len(policy) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(policy, &raw); err != nil || raw == nil {
		return []string{"policy must be a JSON object"}
	}

	var allowed map[string]func(string, json.RawMessage) []string
	switch name {
	case BuiltinToolExec:
		allowed = map[string]func(string, json.RawMessage) []string{
			"timeoutSeconds": requirePositiveInt,
			"maxOutputBytes": requirePositiveInt,
			"deniedPatterns": requireRegexList,
		}
	case BuiltinToolWebFetch:
		allowed = map[string]func(string, json.RawMessage) []string{
			"timeoutSeconds":      requirePositiveInt,
			"maxBytes":            requirePositiveInt,
			"allowPrivateNetwork": requireBool,
		}
	case BuiltinToolWebSearch:
		allowed = map[string]func(string, json.RawMessage) []string{
			"timeoutSeconds": requirePositiveInt,
			"maxResults":     requirePositiveInt,
			"provider":       requireSearchProvider,
		}
	case BuiltinToolSendMessage, BuiltinToolSendInbox, BuiltinToolNotifyOwner:
		// S-164/S-193: send_message, send_inbox and notify_owner share the
		// same policy keys. timeoutSeconds bounds the peers-fetch/send (or
		// owner-POST) round trips; maxMessageChars caps the body so one
		// agent cannot flood a peer's context — or the owner's inbox —
		// with a novel. The notify_owner cap mirrors the server-side
		// maxInboxMessageChars cap (10000, S-229) so the tool fails fast
		// instead of being silently trimmed.
		allowed = map[string]func(string, json.RawMessage) []string{
			"timeoutSeconds":  requirePositiveInt,
			"maxMessageChars": requirePositiveInt,
		}
	case BuiltinToolMemorySearch:
		// S-212: timeoutSeconds bounds the embed-query + pgvector round
		// trips through the gateway; maxResults caps top-k recall.
		allowed = map[string]func(string, json.RawMessage) []string{
			"timeoutSeconds": requirePositiveInt,
			"maxResults":     requirePositiveInt,
		}
	default:
		return []string{fmt.Sprintf("unknown built-in tool %q", name)}
	}

	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys) // deterministic error ordering

	var errs []string
	for _, key := range keys {
		check, ok := allowed[key]
		if !ok {
			errs = append(errs, fmt.Sprintf("unknown policy key %q for tool %s", key, name))
			continue
		}
		errs = append(errs, check(key, raw[key])...)
	}
	return errs
}

func requirePositiveInt(key string, val json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(val))
	dec.UseNumber()
	var num json.Number
	if err := dec.Decode(&num); err != nil {
		return []string{fmt.Sprintf("%s must be an integer", key)}
	}
	n, err := num.Int64()
	if err != nil {
		return []string{fmt.Sprintf("%s must be an integer", key)}
	}
	if n <= 0 {
		return []string{fmt.Sprintf("%s must be a positive integer", key)}
	}
	return nil
}

func requireBool(key string, val json.RawMessage) []string {
	var b bool
	if err := json.Unmarshal(val, &b); err != nil {
		return []string{fmt.Sprintf("%s must be a boolean", key)}
	}
	return nil
}

func requireRegexList(key string, val json.RawMessage) []string {
	var list []string
	if err := json.Unmarshal(val, &list); err != nil {
		return []string{fmt.Sprintf("%s must be an array of strings", key)}
	}
	var errs []string
	for _, pattern := range list {
		if _, err := regexp.Compile(pattern); err != nil {
			errs = append(errs, fmt.Sprintf("%s contains invalid regex %q: %v", key, pattern, err))
		}
	}
	return errs
}

func requireSearchProvider(key string, val json.RawMessage) []string {
	var s string
	if err := json.Unmarshal(val, &s); err != nil {
		return []string{fmt.Sprintf("%s must be a string", key)}
	}
	if !IsSearchProvider(s) {
		return []string{fmt.Sprintf("%s must be one of: duckduckgo, brave, perplexity", key)}
	}
	return nil
}
