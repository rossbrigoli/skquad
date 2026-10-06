# ADR-0013: Dedicated Tool Gateway for Governed Agent Egress

- Status: Proposed (v1, for Ross's review)
- Date: 2026-10-06
- Deciders: Ross (pending), Sherlock
- Related: ADR-0007 (agent identity), ADR-0012 (builtin tools, BT-6),
  [resource-registry.md](../resource-registry.md), [tool-gateway.md](../tool-gateway.md)

## Context

Agent pods are network-isolated (default-deny). The only governed egress today is
`web_fetch`/`web_search` proxied by the control plane (ADR-0012 BT-6). Users now
need agents to reach arbitrary-but-governed external capabilities: REST APIs,
MCP servers, headless browsing. Requirements: zero credentials in sandboxes,
per-agent grants with admin ceilings, no privilege escalation, semantic audit,
≤30 s revocation, SSRF safety.

## Decision

Introduce a **Tool Gateway**: a separate, stateless, horizontally scalable
component in `skquad-system` that is the **only** skquad component (besides the
quarantined Browser Service) with general internet egress. It:

1. Authenticates agents with existing agent credentials (ADR-0007).
2. Resolves grants + ceilings from the control plane (cached, TTL 30 s,
   fail-closed).
3. Executes calls through **protocol-aware drivers**: `web`, `rest`, `mcp`,
   `browser`.
4. Injects credentials from the vault at call time; agents never see secrets.
5. Enforces ceilings, grant constraints, rate limits, and confirmation gates.
6. Emits semantic audit events and metering samples per call.

The resource registry is extended with typed `endpoint_config`,
`policy_ceiling`, `risk_tier`, `egress_class` columns; grants gain
`constraints` (validated ⊆ ceiling). The CP's existing `web_fetch` façade is
retargeted at the gateway during rollout.

## Alternatives considered

1. **Grow the control plane into the proxy.** Cheapest short-term (BT-6 pattern
   extended). Rejected: puts the component that terminates arbitrary hostile
   external connections in the same process/blast radius as identity, grants,
   and vault pointers. Every SSRF/parser/browser-adjacent CVE becomes a CP-root
   CVE. Also couples egress scaling to CP scaling.
2. **Per-pod sidecar proxies.** Fine-grained per-agent enforcement, but policy
   distribution and revocation become a fleet-wide problem (thousands of pods,
   cache invalidation per sidecar), and sidecars in the *agent's* trust domain
   widen the credential-injection surface. Rejected.
3. **Service mesh (Istio/Linkerd) + OPA.** Heavy infra for a homogenous Go
   stack; mesh gives L4/L7 primitives but the semantic layer we need (MCP
   tool allowlists, snapshot drift, confirmation gates, browser session
   brokering) must be built anyway. Rejected as primary; not excluded later.
4. **Virtualize everything per-call in CP (pure "tool API", no gateway).**
   Rejected for the same blast-radius reason as (1).

## Consequences

- (+) Blast-radius separation: gateway compromise ≠ credential/identity
  compromise; CP has no internet egress at all.
- (+) Single semantic audit spine for all agent external interactions.
- (+) Protocol drivers make policy expressive per type (verbs, tool names,
  operations) instead of host-level.
- (−) One more service to deploy/operate (Go binary + Helm chart + netpol).
- (−) Cache TTL introduces a ≤30 s window between revoke and effect
  (accepted; kill switch is immediate via resource disable).
- (−) Driver development cost per protocol (web is a port; rest and mcp are
  new build).
