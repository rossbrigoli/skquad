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
	BuiltinToolExec          = "exec"
	BuiltinToolWebFetch      = "web_fetch"
	BuiltinToolWebSearch     = "web_search"
	BuiltinToolSendMessage   = "send_message"
	BuiltinToolSendInbox     = "send_inbox"
	BuiltinToolNotifyOwner   = "notify_owner"
	BuiltinToolMemorySearch  = "memory_search"
	BuiltinToolSpawnSubagent = "spawn_subagent"
	// S-279: first-class FILE tools so agents can do coding work in their
	// task workspace exactly like in OpenClaw. All four are jailed to the
	// agent's workspace_dir (the same directory exec runs in) — any path
	// that resolves outside it (via ".." or symlinks) is rejected
	// runtime-side. That jail is the security boundary for these tools.
	BuiltinToolRead       = "read"
	BuiltinToolWrite      = "write"
	BuiltinToolEdit       = "edit"
	BuiltinToolApplyPatch = "apply_patch"
	// S-280: background process management (paired with exec's
	// background/yieldMs). Lets agents start long-running commands
	// detached in their task workspace and control them afterwards —
	// list/poll/log/write-stdin/kill/clear — mirroring the OpenClaw
	// process tool. Kill terminates the whole process group so spawned
	// children die too.
	BuiltinToolProcess = "process"
	// S-281: built-in CRON / scheduled tasks. Lets an agent schedule a
	// future agent turn (prompt delivery) for itself: one-shot ('at'),
	// fixed interval ('every') or 5-field cron expression with IANA
	// timezone ('cron'), plus list/cancel/pause/resume. The control
	// plane owns the clock: a scheduler goroutine claims due jobs and
	// fires them through the canonical agent-run path (materialized
	// task + busy mirror), so fires survive restarts and cannot
	// double-run.
	BuiltinToolCron = "cron"
	// S-282: session inspection tools. Read-only mirrors of OpenClaw's
	// sessions_list / sessions_history / session_status: an agent can
	// inspect its own tasks, message history, task context, peer roster
	// and current-run status beyond the intra-squad send_message. Every
	// read rides an EXISTING agent-scoped endpoint (/agents/me/tasks,
	// /agents/me/peers, /agents/me/messages/history,
	// /agents/me/tasks/{id}/context, /agents/{id}/chat, /agents/me/model)
	// under the same bearer credential — the server's existing
	// authorization is the visibility boundary; these tools never widen
	// it and cannot mutate anything.
	BuiltinToolSessionsList    = "sessions_list"
	BuiltinToolSessionsHistory = "sessions_history"
	BuiltinToolSessionStatus   = "session_status"
	// S-283: built-in BROWSER tool (ADR-0014 Browser-as-a-Service).
	// A thin client over the Tool Gateway's browser driver: the runtime
	// dispatches MCP tool calls (browser.navigate/click/type/screenshot/
	// extract/close_session) to the quarantined browser-service via
	// POST {gateway}/v1/mcp/{resourceID}/call. The agent never holds a
	// browser session token or talks to the service directly — session
	// brokering, navigation ceilings (deny_hosts, max_pages) and the
	// egress quarantine live gateway/service-side. The builtin policy
	// below only bounds the RUNTIME side of the round trip (timeout,
	// rendered text size, accepted screenshot size); the security-
	// relevant ceilings are the resource grant's (egresspolicy/browser.go).
	BuiltinToolBrowser = "browser"
	// S-292 (ADR-0016 D1): board.* builtins — agent-driven board work.
	// Each verb maps 1:1 to a validated control-plane transition; there
	// is no free-form status write. The server stays the single
	// enforcement point (column allowlists, leases, fencing, dependency
	// guard S-278, budget guard S-203). The tools are thin clients over
	// agent-authenticated endpoints under /agents/me — the caller's own
	// credential and the transition guards are the authorization
	// boundary. They ship enabled: every verb is guard-railed server-
	// side (backlog default, assignee-only reopen/request-changes,
	// self-approval policy on complete), and invisible-if-disabled would
	// defeat the point of the card.
	BuiltinToolBoardCreateTask     = "board.create_task"
	BuiltinToolBoardAssign         = "board.assign"
	BuiltinToolBoardClaim          = "board.claim"
	BuiltinToolBoardComplete       = "board.complete"
	BuiltinToolBoardBlock          = "board.block"
	BuiltinToolBoardReopen         = "board.reopen"
	BuiltinToolBoardRequestChanges = "board.request_changes"
	// S-294 (ADR-0016 D5/D8): leader approval of others' in-review
	// tasks. Ships enabled; the real gate is server-side (leader grant +
	// leader_approval_allowed, default deny).
	BuiltinToolBoardApprove = "board.approve"
)

// BuiltinToolNames lists the built-in tools in canonical order.
var BuiltinToolNames = []string{BuiltinToolExec, BuiltinToolWebFetch, BuiltinToolWebSearch, BuiltinToolSendMessage, BuiltinToolSendInbox, BuiltinToolNotifyOwner, BuiltinToolMemorySearch, BuiltinToolSpawnSubagent, BuiltinToolRead, BuiltinToolWrite, BuiltinToolEdit, BuiltinToolApplyPatch, BuiltinToolProcess, BuiltinToolCron, BuiltinToolSessionsList, BuiltinToolSessionsHistory, BuiltinToolSessionStatus, BuiltinToolBrowser, BuiltinToolBoardCreateTask, BuiltinToolBoardAssign, BuiltinToolBoardClaim, BuiltinToolBoardComplete, BuiltinToolBoardBlock, BuiltinToolBoardReopen, BuiltinToolBoardRequestChanges, BuiltinToolBoardApprove}

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
// spawn_subagent ships enabled to match its S-232 seed (migration 0042):
// the subagent inherits the parent's model, grants and composed prompt,
// so it can never reach beyond what the parent agent itself could; the
// runtime additionally gates the capability on SKQUAD_SUBAGENTS_ENABLED.
// The S-279 file tools (read/write/edit/apply_patch) ship enabled to
// match their migration 0055 seed: their blast radius is already bounded
// by the workspace jail (pod filesystem + realpath check), and an agent
// without file tools cannot do coding work at all — disabling them by
// default would make the capability invisible until an admin noticed.
func BuiltinToolDefaultEnabled(name string) bool {
	return name == BuiltinToolSendMessage || name == BuiltinToolSendInbox || name == BuiltinToolNotifyOwner ||
		name == BuiltinToolMemorySearch || name == BuiltinToolSpawnSubagent ||
		name == BuiltinToolRead || name == BuiltinToolWrite ||
		name == BuiltinToolEdit || name == BuiltinToolApplyPatch ||
		name == BuiltinToolProcess ||
		// S-281: cron ships enabled to match its migration 0057 seed.
		// The blast radius is bounded control-plane-side: jobs are scoped
		// to the creating agent, capped by maxActiveJobs, throttled by
		// the scheduler tick, and auto-disabled after repeated failures.
		name == BuiltinToolCron ||
		// S-282: the session inspection tools ship enabled to match
		// their migration 0058 seed. They are strictly read-only (GET-
		// only clients over endpoints the agent's own credential already
		// authorizes) and render sanitized output, so their blast radius
		// is "the agent can read what it could already read" — disabling
		// them by default would make the capability invisible.
		name == BuiltinToolSessionsList ||
		name == BuiltinToolSessionsHistory ||
		name == BuiltinToolSessionStatus ||
		// S-283: browser ships enabled to match its migration 0059 seed.
		// The tool is inert without a granted browser resource and a
		// configured tool gateway (both fail-closed runtime-side, and the
		// gateway denies without a grant), so default-enablement cannot
		// widen reach — invisible-if-disabled would defeat the purpose.
		name == BuiltinToolBrowser ||
		// S-292 (ADR-0016 D1): the board.* verbs ship enabled to match
		// their migration 0063 seed. Each is a thin client over a
		// server-guarded transition (see the constants block) — the
		// guards, not the tool surface, bound what an agent may do.
		name == BuiltinToolBoardCreateTask ||
		name == BuiltinToolBoardAssign ||
		name == BuiltinToolBoardClaim ||
		name == BuiltinToolBoardComplete ||
		name == BuiltinToolBoardBlock ||
		name == BuiltinToolBoardReopen ||
		name == BuiltinToolBoardRequestChanges ||
		// S-294 (ADR-0016 D5/D8): board.approve ships enabled to match
		// its migration 0064 seed — the server-side leader-grant +
		// leader_approval_allowed guards (default deny) are the boundary.
		name == BuiltinToolBoardApprove
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
	case BuiltinToolSpawnSubagent:
		// S-232: the subagent loop inherits the parent's runtime limits
		// (max steps, compaction, budget), so there are no admin-tunable
		// policy keys yet. The empty allow-set means only "{}" validates;
		// any key is rejected so a future key can be pinned deliberately.
		allowed = map[string]func(string, json.RawMessage) []string{}
	case BuiltinToolRead:
		// S-279: timeoutSeconds bounds the file read; maxBytes caps the
		// returned content so one read cannot flood the model context.
		allowed = map[string]func(string, json.RawMessage) []string{
			"timeoutSeconds": requirePositiveInt,
			"maxBytes":       requirePositiveInt,
		}
	case BuiltinToolWrite, BuiltinToolEdit, BuiltinToolApplyPatch:
		// S-279: maxBytes caps the total bytes written (for apply_patch,
		// the sum across the patch's files) so a runaway agent cannot fill
		// the workspace PVC. timeoutSeconds bounds the whole operation.
		allowed = map[string]func(string, json.RawMessage) []string{
			"timeoutSeconds": requirePositiveInt,
			"maxBytes":       requirePositiveInt,
		}
	case BuiltinToolProcess:
		// S-280: maxSessions caps concurrently-running background sessions
		// per task workspace; maxLogBytes caps each session's captured log
		// (process killed when the cap is exceeded) so a chatty loop cannot
		// fill the workspace PVC. Both are runtime-enforced limits.
		allowed = map[string]func(string, json.RawMessage) []string{
			"maxSessions": requirePositiveInt,
			"maxLogBytes": requirePositiveInt,
		}
	case BuiltinToolCron:
		// S-281: maxActiveJobs caps how many enabled scheduled jobs one
		// agent may hold at a time (default 10 when unset). Enforced
		// control-plane-side at create/resume so a runaway agent cannot
		// pile up an unbounded queue of future wakes.
		allowed = map[string]func(string, json.RawMessage) []string{
			"maxActiveJobs": requirePositiveInt,
		}
	case BuiltinToolBrowser:
		// S-283: runtime-side bounds only. timeoutSeconds bounds the
		// gateway round trip; maxSnapshotChars caps rendered page text;
		// maxScreenshotBytes caps what the runtime will persist for a
		// screenshot (the service caps capture at its own ceiling).
		// Session/navigation ceilings (max_sessions_per_agent, deny_hosts,
		// max_pages) are NOT builtin policy — they are the resource
		// grant's ceiling (egresspolicy/browser.go, ADR-0014).
		allowed = map[string]func(string, json.RawMessage) []string{
			"timeoutSeconds":     requirePositiveInt,
			"maxSnapshotChars":   requirePositiveInt,
			"maxScreenshotBytes": requirePositiveInt,
		}
	case BuiltinToolSessionsList, BuiltinToolSessionsHistory, BuiltinToolSessionStatus:
		// S-282: the session inspection tools have no admin-tunable
		// policy knobs — they are GET-only clients over existing
		// endpoints with fixed runtime-side sanitization. The empty
		// allow-set means only "{}" validates; any key is rejected so a
		// future knob is pinned deliberately (same posture as
		// spawn_subagent).
		allowed = map[string]func(string, json.RawMessage) []string{}
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
