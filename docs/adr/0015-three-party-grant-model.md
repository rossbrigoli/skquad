# ADR-0015: Three-Party Grant Model with No-Escalation Invariant

- Status: Proposed (v1, for Ross's review)
- Date: 2026-10-06
- Deciders: Ross (pending), Sherlock
- Related: [tool-gateway.md](../tool-gateway.md) §8–9, [resource-registry.md](../resource-registry.md)

## Context

Skquad users create their own agents. Egress resources (REST APIs, MCP servers,
browser) carry credentials and risk that vary by owner and purpose. The question:
who may grant an agent access to a resource? Candidates: platform admin only,
agent owner only, or a split. Admin-only doesn't scale (helpdesk queue) and
destroys attribution ("IT did it"). Owner-only enables privilege escalation and
unvetted capabilities.

## Decision

A **three-party model** (**"resource owner" is an attribute, not a new RBAC
role** — realized as `resources.owner_user_id` + optional co-owners, per
Ross's clarification 2026-10-06; the model collapses onto the existing
`platform_admin` + user roles wearing different hats per resource):

| Party | Owns |
|---|---|
| Platform admin | Resource existence (catalog), policy ceilings, risk tiers, `internal` egress class, global kill switch |
| Resource owner | The credential/identity behind a resource (BYO user, or owning team for shared org resources) |
| Agent/squad owner | Which of their agents use which resources, within constraints |

**Credential provenance routes the approval path:**

- BYO credentials → owner self-grants (zero friction; expected majority).
- Shared org resource → resource-owner approval workflow.
- High risk tier → resource-owner approval **+ admin co-sign**.

**No-escalation invariant (enforced in CP, exhaustively tested):**

```
effective grant ⊆ (admin policy ceiling) ∩ (grantor's delegable scope)
```

Grants *transfer* capability; they never *amplify* it. Grant `constraints`
(methods, tools, rates, sizes, egress class) are validated as a subset of the
resource ceiling at write time; violations rejected with structured errors.

Agents act under **their own identity** end-to-end (ADR-0007); the owner link
is recorded, never impersonated. Revocation = delete grant row; effective
≤ 30 s via gateway cache TTL; resource disable = immediate global deny.

## Alternatives considered

1. **Platform-admin grants everything.** Unscalable; useless attribution;
   admins become a bottleneck for BYO self-service. Rejected.
2. **Agent owners grant anything in the catalog.** Escalation vector: a user
   with no rights to prod could mint an agent with prod access the moment
   someone registers it. Rejected.
3. **Flat RBAC (role→permission) without ceilings.** Cannot express "broad web
   read yes, but not this MCP's merge tool." Rejected in favor of
   per-resource ceilings + per-grant constraints.

## Consequences

- (+) Self-service where safe, governed where risky; audit answers "who let
  this agent touch X" at every hop (owner, approver, ceiling-setter).
- (+) Compromised agent is revocable at agent level without touching human
  access; humans can't be framed for agent actions (non-repudiation).
- (−) Approval workflow UI/state machine required for medium/high tiers
  (Phase 4 / TG-8).
- (−) "Delegable scope" for shared org resources needs a defined
  representation (resource-owner ACL on the registry row).
