# ADR-0012: Built-in Platform Tools (exec, web_fetch, web_search)

- Status: Accepted (v1 contract)
- Date: 2026-09-27
- Deciders: Ross, Sherlock
- Related: ADR-0011 (layered prompts, fetch-at-wake), S-122 (chat tool loop)

> **Amended by S-164 (2026-09-28):** the builtin universe widened to four —
> `send_message` (agent-to-agent messaging within the squad) was added via
> migration `0023`. Unlike the original three it is seeded **enabled**:
> intra-squad messaging is the core squad primitive, bounded by squad
> isolation, control-plane grant checks for cross-squad sends, and a
> 12-message per-correlation-chain budget. Policy keys: `timeoutSeconds`,
> `maxMessageChars`. See
> [`collaboration-messaging.md`](../collaboration-messaging.md) §10.

## Context

Tools in Skquad today come from external plugin modules loaded via
`SKQUAD_PLUGIN_MODULES` / `SKQUAD_ENABLED_PLUGINS`. No concrete plugins ship or
are configured, so live agents have an empty tool list. Ross wants three tools as
**first-class platform built-ins**, configurable only by the platform admin:

1. `exec` — run terminal commands
2. `web_fetch` — fetch a URL (OpenClaw-style readable extract)
3. `web_search` — web search (OpenClaw-style)

## Decision

### 1. Config source of truth: control plane

New Postgres table (embedded migration), one row per built-in tool:

```sql
CREATE TABLE builtin_tools_config (
  name        TEXT PRIMARY KEY CHECK (name IN ('exec','web_fetch','web_search')),
  enabled     BOOLEAN NOT NULL DEFAULT FALSE,
  policy      JSONB   NOT NULL DEFAULT '{}',
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by  TEXT    NOT NULL DEFAULT ''
);
-- seed: all three rows enabled=false
```

In-memory dev storage mirrors the same shape. Policy JSON is validated
server-side on write (unknown keys → 400).

### 2. Control-plane API

Admin endpoints (platform_admin only — same IdP-group role binding as existing
admin surface; every PATCH is audit-logged with actor):

```
GET   /api/v1/admin/tools
  -> { "tools": [ {"name","enabled","policy","updatedAt","updatedBy"} ] }

PATCH /api/v1/admin/tools/{name}
  body: { "enabled"?: bool, "policy"?: object }   # merge semantics
  -> 200 full tool config | 400 invalid policy | 403 not admin
```

Agent-facing config (agent-credential auth, ETag — mirrors composed-prompt
pattern from ADR-0011 D4):

```
GET /api/v1/agents/me/tools      If-None-Match supported
  -> { "tools": [ {"name","enabled","policy"} ] }   # NEVER contains secrets
```

Search proxy (agent-credential auth) — the search provider API key lives ONLY
in the control plane and never crosses to the runtime:

```
POST /api/v1/tools/web_search
  body: { "query": string, "maxResults"?: int }
  -> { "results": [ {"title","url","snippet"} ] } | 403 disabled | 502 provider error
```

### 3. Tool policies (validated JSON)

`exec`:
```json
{ "timeoutSeconds": 60, "maxOutputBytes": 65536,
  "deniedPatterns": ["regex", "..."] }
```
- Runs in the agent pod (the container IS the sandbox), cwd = the agent's task
  workspace dir (hard-coded; not configurable).
- `deniedPatterns` are regexes matched against the full command string BEFORE
  execution; match → refusal (tool result `ok=false`, denial recorded).
- Defaults on enable: timeout 60s, 64 KiB output cap, seeded denylist
  (`rm -rf /`, `mkfs`, `dd of=/dev/`, fork bombs, `shutdown|reboot`,
  `curl|wget` piped to `sh`).
- Env scrub: subprocess env excludes every `SKQUAD_*`, `*_API_KEY`, `*_TOKEN`,
  `*_SECRET` var from the runtime process.

`web_fetch`:
```json
{ "timeoutSeconds": 30, "maxBytes": 262144, "allowPrivateNetwork": false }
```
- GET only; follows ≤3 redirects (re-checking SSRF guard on each hop).
- SSRF guard: block loopback, RFC1918, CGNAT, link-local incl. cloud metadata
  (169.254.169.254), and IPv6 ULA — unless `allowPrivateNetwork: true`.
- Returns extracted readable text (html→text), truncated at `maxBytes`.
- **BT-6 (2026-09-28): execution location moved to the control plane.**
  Agent pods run under a default-deny egress NetworkPolicy (only DNS +
  skquad-system reachable), so the runtime cannot fetch arbitrary URLs.
  The guarded fetch now runs at `POST /api/v1/tools/web_fetch` (agent-
  credential auth), mirroring the web_search proxy. The SSRF guard is
  enforced at dial time (`DialContext` IP check), which also closes the
  DNS-rebinding TOCTOU the old resolve-then-connect runtime guard had.
  The runtime keeps only scheme validation + html→text extraction.
  Policy fields and semantics are unchanged.

`web_search`:
```json
{ "timeoutSeconds": 20, "maxResults": 8, "provider": "duckduckgo" }
```
- `provider` ∈ `duckduckgo` (keyless, default) | `brave` | `perplexity`.
- Provider credentials are control-plane-side (secrets on the CP deployment);
  the runtime NEVER sees keys — it calls the CP search proxy.

### 4. Runtime integration (agent-runtime)

- New module `skquad_runtime/builtin_tools.py`: `ExecTool`, `WebFetchTool`,
  `WebSearchTool` implementing the existing `RuntimePlugin` protocol
  (`name`, `tools()`, `invoke(call, config)`), so both chat (S-122) and task
  tool loops work unchanged.
- New `BuiltinToolsConfigCache` — fetch-at-wake from
  `GET /api/v1/agents/me/tools` with ETag, same failure policy as composed
  prompt: network/5xx → wake fails loudly; 404 (CP predates endpoint) →
  built-ins absent (transitional).
- Tool list assembly: built-ins (enabled ∩ fetched) first, then plugins.
  Name collision with a plugin → built-in wins, collision logged as ERROR.
- Tool results are wrapped as untrusted data (S-PROMPT WP3 `wrap_untrusted`).
- Kill switch: `SKQUAD_BUILTIN_TOOLS_ENABLED=false` env on the agent pod
  disables the whole feature regardless of platform config (operator-level
  override).

### 5. Web UI (web/, Next.js)

- New page under Settings: **"Built-in Tools"**, rendered only for
  `platform_admin` (existing role-check pattern).
- Per-tool card: enable toggle + policy form (timeouts, output caps, deny
  patterns list, allowPrivateNetwork switch, provider/maxResults).
- Save → `PATCH /api/v1/admin/tools/{name}`; inline validation errors;
  shows `updatedAt`/`updatedBy`.

## Security posture / failure modes

| Risk | Mitigation |
|---|---|
| exec = RCE in agent pod | Container sandbox + pod resource limits; denylist; default disabled; env scrub |
| web_fetch SSRF (cloud metadata, internal admin) | Private-network block by default, per-hop recheck |
| Search key exfil | Key never leaves control plane (proxy) |
| Prompt injection via tool output | Existing untrusted-wrapping (WP3) |
| Config drift / stale runtime | ETag fetch every wake, fail-loud |
| UI bypass | Server-side role check on admin endpoints (UI check is cosmetic) |

## Rollout (card order)

1. **BT-2** Control plane: migration, storage, admin API, agent fetch, search proxy, tests.
2. **BT-3** Runtime: builtin_tools.py, config cache, handler wiring, tests.
3. **BT-4** Web UI: admin settings page, tests.
4. **BT-5** Operator/Helm wiring, e2e validation, docs, security pass.

Directories are disjoint (control-plane / agent-runtime / web) — BT-2/3/4
parallelize safely against this pinned contract.
