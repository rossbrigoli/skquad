# Skquad UI v2 — Build & A/B Deployment Plan

**Date:** 2026-09-22 · **Owner:** Sherlock · **Design basis:** `docs/UI-REDESIGN-PROPOSAL.md`

Ross's brief: build the redesigned UI **from scratch** as a **new app** in the same repo
(`web-v2/`), **do not touch** the current `web/` app, deploy to the k3s cluster so
v1 (`skquad.rossbrigoli.com`) and v2 (`skquad-v2.rossbrigoli.com`) run side-by-side
for A/B testing.

## 1. Guiding decisions

| Decision | Choice | Rationale |
|---|---|---|
| Location | `web-v2/` in the skquad repo | Same repo per brief; independent build context |
| Stack | Next.js (App Router) + TypeScript, same as v1 | Team familiarity, reuse of typed API client patterns |
| API | Same control-plane API (`/api/v1`), no v2 API | v1 API already returns everything needed for Phases 1–2 |
| Isolation | Separate Helm subchart resources (`webV2.*` values), separate Deployment/Service/IngressRoute | v1 untouched; v2 can be disabled by flipping one value |
| v2 hostnames | `skquad-v2.lab` + `skquad-v2.rossbrigoli.com` | Standard pattern per TOOLS.md (both hosts in one IngressRoute) |
| Auth | Same Cloudflare Access + same token model as v1 | A/B compares UX, not auth |
| Feature parity | v2 implements the new IA; not a 1:1 port | v1 stays available as fallback throughout |

## 2. Target IA (from redesign proposal)

```
/inbox                      Attention queue (badge)
/squads                     Squad list
/squads/[id]                Cockpit (mission, live runs, cost, activity)
/squads/[id]/board          Kanban
/squads/[id]/tasks/[tid]    Task thread (thread, runs, actions)
/squads/[id]/agents/[aid]   Agent profile (leases, runs, spend, grants)
/costs                      Cross-squad spend + budgets
/settings/*                 Providers, Resources, Grants, Audit (demoted)
```

Two-rail layout: persistent primary rail (Inbox · Squads · Org · Costs · Settings)
+ contextual secondary rail for the selected squad/agent/task.

## 3. Work packages (Kanbunny cards)

### UIv2-1 — Scaffold + design tokens + shared primitives
- `create-next-app` scaffold in `web-v2/` (TS, App Router, ESLint, Vitest)
- Token layer in `globals.css`: semantic colors, status tokens
  (`running/stalled/blocked/in-review/idle/error/over-budget/paused`),
  monospace token for machine values, spacing/radius scale
- Shared primitives: `StatusChip`, `EmptyState`, `MetricTile`, `EntityRow`,
  `AppShell` (rails), typed API client (adapted from v1 `lib/api.ts`)
- `npm run lint && tsc --noEmit && vitest run && next build` green

### UIv2-2 — Ship pipeline: image + Helm + ArgoCD + ingress (A/B reachable)
- `web-v2/Dockerfile` (same pattern as v1)
- `images.yml`: add `web-v2 → skquad-web-v2` matrix entry
- Chart: `webV2.{enabled,image,port,resources}` → `web-v2` Deployment + Service;
  extend IngressRoute with `skquad-v2.lab` + `skquad-v2.rossbrigoli.com`
  (`/` → web-v2, `/api` → api-server)
- `k3s-cluster/apps/app-skquad.yaml`: enable `webV2` + tag
- Ask Ross: Cloudflare Tunnel `skquad-v2.rossbrigoli.com → skquad-v2.lab`
- Verify: v2 responds, v1 unchanged

### UIv2-3 — Nav shell + squads list + cockpit routes
- Primary rail + contextual secondary rail (squad tabs), URL-driven
- `/squads` list; `/squads/[id]` cockpit: mission header, live-runs strip
  (agent × task × lease age), cost tile, activity feed — all deep-linked

### UIv2-4 — Attention-first Inbox
- Client-side merge: stalled/blocked tasks, old reviews, budget warnings,
  unread messages, agent errors → prioritized queue with reason chips + deep links
- Nav badge count

### UIv2-5 — Task detail thread
- Thread (task-scoped messages + composer), run history, status timeline,
  actions: reassign / block / pause-resume / move

### UIv2-6 — Agent profile + Costs
- Agent page: status, current lease, recent runs, spend, granted resources
- `/costs`: per-squad/agent/provider spend; budget threshold display
  (budget *policy* API deferred — show spend vs. configured warn levels if available)

### UIv2-7 — Polish
- Mobile: rails → overlay drawer; empty states copy pass; buttons name actions

## 4. Sequencing & A/B mechanics

1. UIv2-1 → UIv2-2 first: **v2 reachable early**, even with skeleton screens.
2. UIv2-3/4/5 build the daily-driver loop (inbox → squad → task).
3. UIv2-6/7 round it out.
4. A/B: Ross uses both URLs against the same live cluster/API; feedback lands as
   new cards. No data divergence — same backend.
5. Promotion (later, Ross's call): v2 replaces `web/` or repo keeps both.

## 5. Risks

| Risk | Mitigation |
|---|---|
| Same-API load from two UIs | Negligible (static-heavy, same endpoints); v2 polls only visible screens |
| Chart drift between v1/v2 web resources | Shared `_helpers.tpl`, one values file, v2 gated by `webV2.enabled` |
| Cloudflare tunnel latency on new host | Only needed for external A/B; `.lab` works immediately internally |
| Scope creep toward Paperclip feature-set | Cards are fixed; new wants become new cards, not silent additions |
