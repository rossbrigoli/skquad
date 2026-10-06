# TG-6 Browser Service — Pinned Protocol (v1)

Implements docs/tool-gateway.md §5.3 + §6.4 and ADR-0014. This document is the
single source of truth for the wire contract between the tool-gateway `browser`
driver and the `browser-service`. Components: `browser-service/` (TypeScript),
`browser-proxy/` (Go egress sidecar), `tool-gateway/internal/drivers/browser/`.

## 1. Identity & auth

- Browser service listens on `:8090` (HTTP, cleartext in-cluster).
- **Control plane ↔ service and gateway ↔ service control calls** carry
  `Authorization: Bearer <SKQUAD_BROWSER_INTERNAL_TOKEN>` (same generation
  pattern as `SKQUAD_GATEWAY_INTERNAL_TOKEN`, TG-5). Missing/wrong → 401.
- MCP tool calls are authenticated by the **session token** (see §3), not the
  internal token. The service accepts MCP calls only with a valid session token.

## 2. Session lifecycle (non-MCP, internal-token auth)

### POST /v1/sessions
Request:
```json
{
  "agent_id": "…", "task_id": "…", "resource_id": "…",
  "policy": {
    "deny_hosts": ["*.internal", "metadata.google.internal"],
    "max_pages": 50,
    "max_screenshot_bytes": 2097152,
    "idle_timeout_s": 600,
    "max_session_minutes": 30
  }
}
```
Response `201`:
```json
{ "session_id": "bs_<32hex>", "token": "<48hex>", "expires_at": "<RFC3339>" }
```
- Session bound immutably to (agent_id, task_id, resource_id).
- `expires_at = now + max_session_minutes` (hard cap) and idle timeout applies
  independently (default 600 s).
- No capacity (pool exhausted + queue timeout 30 s) → `503 {"error":"browser_busy"}`.

### GET /v1/sessions
List active sessions (id, agent_id, task_id, resource_id, pages_used,
idle_seconds, expires_at). Internal token only.

### DELETE /v1/sessions/{id}
Force-close (orphan sweeper / admin). `204` always (idempotent).

### POST /v1/sweeper/pass
Runs orphan sweep immediately (also runs every 60 s internally): reaps sessions
whose token has had no activity past idle timeout, past hard cap, or whose
instance crashed. `200 {"reaped": N}`.

## 3. MCP surface (streamable HTTP, POST /mcp)

Standard JSON-RPC 2.0, synchronous JSON responses (SSE not required).
Every tool argument object MUST include `"session": "<token>"` (injected by the
gateway driver; agents never see or supply it).

- `initialize` → `{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"skquad-browser-service","version":"…"}}`
- `tools/list` → the six tools below with JSON Schema `inputSchema`.
- `tools/call` → MCP `CallToolResult`: `{"content":[{"type":"text","text":"…"}],"isError":bool}`.

Errors (JSON-RPC level, not tool results):
- `-32001 session_invalid` — missing/unknown/expired/wrong-agent token.
- `-32002 ceiling_exceeded` — navigation/policy violation (host denied, max_pages, size cap).
- `-32003 browser_busy` — no instance available.

### Tools (exact names)
| Tool | Args (besides `session`) | Result text |
|---|---|---|
| `browser.navigate` | `url` (http/https only) | JSON `{url, title, page_index}` |
| `browser.click` | `selector` (CSS) | JSON `{clicked: selector}` |
| `browser.type` | `selector`, `text`, `submit?` | JSON `{typed: selector}` |
| `browser.screenshot` | `full_page?` | JSON `{screenshot_b64, bytes, mime:"image/png"}` (capped) |
| `browser.extract` | — | JSON `{text}` — visible body text, ≤ 200 KB, wrapped as untrusted by gateway |
| `browser.close_session` | — | JSON `{closed: session_id}` |

## 4. In-service enforcement (browser-service)

- **Navigation ceiling:** enforced via `context.route('**/*', …)` on EVERY
  request (top-level, subresources, fetch/XHR, WebSocket): deny if host
  matches `deny_hosts` (netguard-style suffix/glob), deny any non-http(s)
  scheme, deny private/loopback/link-local/CGNAT hosts (resolve + check;
  fail closed). Top-level navigations count toward `max_pages`; exceeding →
  deny.
- **Screenshot cap:** larger than `max_screenshot_bytes` → clip to viewport
  (non-full-page); still larger → error `ceiling_exceeded`.
- **Session isolation:** one Chromium instance + one BrowserContext per
  session; destroyed on close/idle/reap. No profile persistence.
- **Process isolation:** warm pool hands out instances EXCLUSIVELY; two
  sessions never share a Chromium process. Pool size configurable
  (`POOL_SIZE`, default 4; each instance ≈ 150–300 MB).

## 5. Egress proxy (browser-proxy sidecar, Go)

- Forward proxy on `:8888` (HTTP CONNECT + absolute-URI GET). Chromium is
  launched with `--proxy-server=http://127.0.0.1:8888`.
- Uses `shared/netguard` Guard: dial-time IP pinning, blocks loopback/RFC1918/
  CGNAT/link-local/metadata/IPv6 ULA. Only ports 80/443 allowed.
- No auth (same-pod only; netpol ensures nothing else can reach the pod).
- Logs one line per connection: `ts target_ip port allowed reason` (no secrets).

## 6. Gateway `browser` driver

- Registered resource kind: `mcp` whose `url` targets the browser service MCP
  endpoint; driver selected by grant `driver: "browser"` (see CP slice).
- On first tool call for an agent without an active session: driver calls
  `POST /v1/sessions` with binding + grant-derived policy, caches
  (agent→session) in its session registry (in-memory, TTL-checked).
- `max_sessions_per_agent` (default 1, enforced at driver; >1 rejected at
  create with `session_limit`).
- Driver injects `session` token into tool args before forwarding; strips it
  from anything agent-supplied (agent value ignored/overwritten).
- `browser.close_session` → forward, then unbind. Driver also unbinds on
  expiry (checks `expires_at` before use; stale → create new).
- All actions audited via the standard gateway audit path.

## 7. NetworkPolicy topology (chart)

- ns `skquad-browser`, default-deny all ingress+egress.
- Browser pod egress: DNS (kube-dns), and `skquad-system` tool-gateway ONLY
  for nothing — actually: browser pod egress = nothing except via its own
  localhost proxy (localhost is not policy-filtered); proxy egress = public
  internet 80/443 (netpol: deny all pod-CIDR ranges explicitly + netguard
  floor). No ingress to browser pod except tool-gateway (control + MCP).
- Gateway egress += `skquad-browser` pods on 8090.

## 8. v1 non-goals

- Authenticated browsing / cookie vault (TG-7).
- Pod-per-session gVisor isolation (future).
- Screenshot object-storage refs: v1 returns base64 preview through the
  gateway; object-storage offload is a follow-up (card note).
