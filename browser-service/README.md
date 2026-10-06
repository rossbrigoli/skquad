# browser-service (TG-6)

TypeScript Playwright MCP server implementing the pinned TG-6 browser protocol
([`docs/tg6-browser-protocol.md`](../docs/tg6-browser-protocol.md)). Provides
session-scoped headless browsing with in-service navigation-ceiling enforcement
for the Skquad tool-gateway `browser` driver.

## What it does

- **Sessions** (`POST/GET/DELETE /v1/sessions`, internal-token auth): each
  session is immutably bound to `(agent_id, task_id, resource_id)`, owns one
  Chromium instance + one BrowserContext (no profile persistence), and carries
  its policy (`deny_hosts`, `max_pages`, `max_screenshot_bytes`,
  `idle_timeout_s`, `max_session_minutes`).
- **Warm pool** (`POOL_SIZE`, default 4): instances handed out exclusively —
  two sessions never share a Chromium process. Exhausted pool + 30 s queue
  timeout → `503 {"error":"browser_busy"}`.
- **MCP surface** (`POST /mcp`, JSON-RPC 2.0): `initialize`, `tools/list`,
  `tools/call` for `browser.navigate`, `browser.click`, `browser.type`,
  `browser.screenshot`, `browser.extract`, `browser.close_session`. Every
  tool call requires `arguments.session` (gateway-injected token); validated
  before any browser work. Error codes: `-32001 session_invalid`,
  `-32002 ceiling_exceeded`, `-32003 browser_busy`.
- **Enforcement core**: `context.route('**/*')` checks EVERY request
  (top-level, subresources, fetch/XHR/WebSocket): deny-listed hosts
  (netguard-style suffix/glob), http(s)-only schemes, and
  private/loopback/link-local/CGNAT/ULA addresses (DNS-resolved, fail-closed).
  Top-level navigations count toward `max_pages`. Screenshots over
  `max_screenshot_bytes` are clipped to viewport; still over → `ceiling_exceeded`.
- **Sweeper**: every 60 s (and on `POST /v1/sweeper/pass`) reaps idle,
  hard-capped, or crashed-instance sessions.

## Run

```bash
npm install
npm run build
BROWSER_INTERNAL_TOKEN=*** node dist/src/server.js
```

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `BROWSER_INTERNAL_TOKEN` | — (required) | Bearer token for the control API (`/v1/*`). Service refuses to start without it. |
| `PORT` | `8090` | HTTP listen port. |
| `POOL_SIZE` | `4` | Warm pool size (each Chromium instance ≈ 150–300 MB). |
| `BROWSER_EXECUTABLE` | `chromium` | Chromium executable path for `playwright-core`. Browsers are NOT downloaded at build time. |
| `BROWSER_ACQUIRE_TIMEOUT_MS` | `30000` | Queue timeout before `browser_busy`. |

## Test

```bash
npm test   # builds, then node --test on compiled output (FakeBrowserInstance — no real Chromium needed)
```

## Layout

- `src/browser.ts` — `BrowserInstance` abstraction: `PlaywrightBrowserInstance`
  (playwright-core, headless) + `FakeBrowserInstance` (test double that fires
  route handlers on navigation).
- `src/pool.ts` — exclusive warm pool, queue + `BrowserBusyError`.
- `src/sessions.ts` — session store, token generation, sweeper, route wiring.
- `src/enforce.ts` — navigation ceiling: deny-hosts, scheme, private-IP
  (fail-closed), max-pages.
- `src/server.ts` — `node:http` control API + MCP JSON-RPC dispatch.

Runtime dependency: `playwright-core` only.
