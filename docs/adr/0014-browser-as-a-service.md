# ADR-0014: Browser-as-a-Service (quarantined Playwright MCP)

- Status: Proposed (v1, for Ross's review)
- Date: 2026-10-06
- Deciders: Ross (pending), Sherlock
- Related: ADR-0013 (tool gateway), [tool-gateway.md](../tool-gateway.md) §5.3, §6.4

## Context

Some agents (e.g., a job-hunter "Maria") need interactive browsing: JS-rendered
sites, form filling, screenshots. A headless browser **inside the agent sandbox**
would require granting the sandbox real network egress (breaking the
no-web-path invariant) and exposes the agent to drive-by exploits, uncontrolled
subresource fetches (JS, WebSockets, service workers) that no per-verb policy
can govern.

## Decision

Run headless browsing as an **external managed resource**: a Playwright-based
**MCP server** in a dedicated quarantine namespace (`skquad-browser`),
registered as an `mcp` ToolEndpoint and driven through the Tool Gateway.

- **Tools:** `browser.navigate`, `browser.click`, `browser.type`,
  `browser.screenshot`, `browser.extract`, `browser.close_session`.
- **Session brokering:** the gateway's `browser` driver creates sessions bound
  to `(agent_id, task_id, resource_id)`; the browser service enforces the
  binding and a gateway-supplied navigation ceiling (deny-hosts, max pages).
- **Ephemeral contexts:** one browser context per session; destroyed on close,
  idle timeout (10 min), or sweeper pass. No persistent profiles in v1.
- **Quarantine egress:** default-deny netpol; egress only to public web via a
  forward-proxy sidecar running the shared SSRF/IP-pin guard. No route to any
  cluster-internal workload, including `skquad-system`.
- **v1 scope:** unauthenticated browsing only. Authenticated sessions
  (BYO login, vault-backed cookie injection) are a separate, higher risk tier
  (Phase 3.5 / TG-7).

## Alternatives considered

1. **In-sandbox Playwright + proxy env vars.** Requires sandbox egress to a
   proxy; browser JS execution inside the agent's trust domain; exploit →
   agent pod compromise with the agent's own credentials. Rejected.
2. **No browser at all (fetch-only).** Fails the Maria-class use case; JS-heavy
   job sites are unusable via plain GET. Rejected.
3. **Commercial browser API (Browserless/browserbase SaaS).** Faster to adopt
   but data leaves the platform and per-session governance depends on vendor
   capabilities. Acceptable as an *alternative backend* later (the MCP
   abstraction allows swapping), but v1 builds on-cluster Playwright for
   control and cost. Deferred, not excluded.

## Consequences

- (+) Agent never renders hostile web content; exploits land in a sacrificial,
  network-fenced zone.
- (+) Browser actions get the same semantic audit spine as REST/MCP.
- (+) MCP abstraction allows swapping the backend (self-hosted ↔ SaaS) without
  touching agents or grants.
- (−) New service + Chromium image to maintain (pinned, signed, patched
  cadence required).
- (−) Latency per action vs. local browser (acceptable for agent workloads).
- (−) Session-state realism limited in v1 (no logins) — covers research/
  browsing, not full authenticated workflows until TG-7.
