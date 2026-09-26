# skquad — Web App & UX Design

> **Status:** v1-era design document. The IA described below (Admin/Providers
> main-menu sections, squad-centric sidebar) reflects the **retired v1 app**.
> As of 2026-09-26 the only UI is the v2 app in `web/` — see
> [`UI-V2-PLAN.md`](UI-V2-PLAN.md) and
> [`UI-REDESIGN-PROPOSAL.md`](UI-REDESIGN-PROPOSAL.md) for the current IA
> (Dashboard · Inbox · Squads · Costs · Settings, slim breadcrumb top bar).
> Platform-admin surfaces live under **Settings** (AI Models / Resources /
> Access); see §4.9 for the current shape.
>
> The web app is a **SPA** (React / Next.js) — the primary interface for users.
> It is optimised for **simplicity**: the onboarding path is a few clicks, and
> the **Kanban board** is the centre of daily use.
>
> First-pass workflows are implemented; richer browser UX remains in progress.
> See [`implementation-status.md`](implementation-status.md).

---

## 1. UX Principles

- **Minutes to first agent** — the onboarding flow is the north star; keep it
  to a handful of clicks.
- **Board-first** — the Kanban board is the primary surface; everything else
  supports it.
- **Role-aware** — the UI adapts to the user's role (platform admin vs. user)
  and ownership (owner vs. granted).
- **Visible progress** — users can always see what each agent is doing and the
  squad's overall state.
- **Safe by default** — destructive actions (delete squad/agent, revoke grants)
  are confirmed and audited.

---

## 2. Onboarding — the "Minutes" Flow

```
1. Login (OIDC)
2. "Create a squad" → name + mission + LLM provider and model
3. "Add an agent" → name + role (inherits the squad's LLM; pick one of its models)
   → one-click "Create agent identity"
4. Done → the squad board is ready
```

- Each step is a single screen with sensible defaults.
- The **LLM is chosen once, when the squad is created**, from the predefined
  providers a platform admin has registered (no key entry). The first active
  provider is preselected, so the common path needs no extra clicks.
- A **progress indicator** shows how close the user is to "done."
- After onboarding, the user lands on the **squad board**.

---

## 3. Layout & Navigation

```
┌──────────────────────────────────────────────────────────────┐
│ [sq] skquad   [Open tasks · N] [Mode · Dev] [API ●]     [👤] │
├──────────┬───────────────────────────────────────────────────┤
│ Inbox    │  Squad: <name>                                    │
│ Squads   │  [Overview] [Agents] [Tasks] [Access Grants]      │
│ Resources│  ┌──────────────────────────────────────────────┐ │
│  · Skills│  │ TODO │ IN-PROGRESS │ IN-REVIEW │ DONE │ BLOCK │ │
│  · Tools │  │ ┌───┐│  ┌───┐      │           │      │       │ │
│  · APIs  │  │ └───┘│  └───┘      │           │      │       │ │
│  · KBs   │  └──────────────────────────────────────────────┘ │
│ Admin*   │                                                   │
│ Providers*│                                                  │
└──────────┴───────────────────────────────────────────────────┘
```

- **Top navbar:** brand + title on the left; status pills (open tasks in the
  selected squad, auth mode, API connectivity) and the user profile avatar on
  the right. The avatar opens a dropdown with name, email, role, and the API
  token form.
- **Sidebar (main menu):** Inbox, Squads, Resources (with one subsection per
  resource type: Skills, Tools, APIs, Knowledge Bases, Project Workspaces),
  Providers, and Admin. Providers and Admin are shown only to `platform_admin`
  users; dev mode auto-promotes the dev principal to `platform_admin`, so they
  are visible in DEV deployments. The API routes behind Resources remain
  `/registry/*`.
- **Squad-centric IA:** Agents and Tasks are not top-level menu items. They are
  tabs inside the selected squad's detail view (Overview / Agents / Tasks /
  Access Grants), so agents are always created inside a squad and there are no
  orphaned agents.
- **LLMs belong to squads:** LLM providers are not a Resources subsection. A
  squad picks its LLM when it is created, and its agents inherit it.
- **Create actions open dialogs:** each list has a **+ New …** (or **+
  Register …**) button in its header, and the form opens in a modal dialog
  rather than sitting permanently beside the list, which gives lists and the
  task board the full width. A dialog closes only when the save succeeds; on
  an error it stays open with the message inside it and the user's input
  intact. Escape, Cancel and the close button dismiss it, a click on the
  backdrop does not, and drafts survive being dismissed. Below tablet width the
  dialog becomes a bottom sheet. Editing in place (squad settings) and the
  agent chat composer stay inline. Deleting a squad, agent or task asks for
  confirmation in the same kind of dialog, with a red confirm button, rather
  than the browser's pop-up.
- **Theme:** white / light-grey surfaces with an orange accent, an
  enterprise-oriented light theme.

---

## 4. Key Screens

### 4.1 Squad Board (primary)
- **Columns:** TODO, IN-PROGRESS, IN-REVIEW, DONE, BLOCKED.
- **Cards:** task title, assignee agent, status, age.
- **Interactions:** create task, drag between columns, assign to an agent, open
  task detail.
- **Task detail:** description, metadata, activity (who moved it when), assignee,
  (for agent-created tasks) the creating agent.
- **Live updates:** columns update as agents work (via polling or WebSocket).

Current implementation: users can create tasks, move them between statuses,
assign/reassign agents, and delete tasks. Rich task detail, activity, drag and
drop, and live updates remain follow-up work.

### 4.2 Agent Panel
- **List of agents** in the squad (name, role, state: idle/busy).
- **Agent detail:**
  - Role, and the **model** it uses — always one served by the squad's LLM
    provider.
  - **Identity** — status, "Create identity" / "Rotate" (owner).
  - **Permissions** — which resources the agent may use (grant/revoke). LLM
    access is not granted here: it comes from the squad's LLM.
  - **Metering** — tokens + cost for this agent.
- **Chat** — open a 1:1 chat with the agent.

Current implementation: users can create agents (choosing a model from the
squad's LLM provider), create/rotate their identities, select an agent, view
queued chat history, enqueue consult messages, and manage the selected agent's
resource permissions. An agent that is not on the squad's LLM — one created
before squads had an LLM, or after the squad's LLM changed, or one left with a
grant for another provider — shows an **Apply squad LLM** action. Apply grants
the squad's provider first, then switches the agent's provider and model, and
removes other provider grants last, so an interrupted Apply never leaves the
agent pointing at a provider it has no grant for; running it again finishes
the job. Per-agent metering panels remain follow-up work.

### 4.3 Chat (secondary)
- A 1:1 conversation with an agent (ad-hoc questions / steering).
- Distinct from tasks — does not reset the task context.
- Available to the owner + granted users.

### 4.4 Squad Settings
- **Mission** — what the squad is for.
- **LLM** — the provider and default model the squad's agents use. Chosen at
  creation and changeable later from the squad's Overview tab, where it can
  also be cleared ("No LLM provider"); a squad without an LLM cannot add agents
  until one is chosen again. A change applies
  to agents added afterwards; existing agents move over with **Apply squad
  LLM**, and an agent that already has an identity needs it rotated before the
  LLM gateway serves the new provider.
- **Operating model** — the role of each agent + how they collaborate (editable
  structured form).
- **Access grants** — grant/revoke other users (or other squads' agents) access
  to talk to the squad's agents.

Current implementation: mission and LLM are editable on the Overview tab. The
squad's LLM is stored in its `operating_model` as `llm.provider_id` and
`llm.model` until the control plane has a first-class field for it.

### 4.5 Metering
- **Per squad** — aggregate tokens + cost over time (charts).
- **Per agent** — tokens + cost.
- **Per provider/model** — breakdown.
- **Platform-wide** (admin) — across all squads.
- Cost shown only where the provider has pricing configured.

### 4.6 Resources (platform admin)
- **Subsections** by resource type: Skills, Tools, APIs, Knowledge Bases,
  Project Workspaces (sidebar sub-navigation). LLM providers are not listed
  here — see 4.8.
- **Register** a resource (definition + credential ref).
- **Deprecate** a resource.

Current implementation: platform admins can register and deprecate resources,
other users see a read-only catalog, and squad owners can grant/revoke selected
resources to the selected agent.

### 4.7 Audit (admin / owner)
- Queryable log: filter by actor, squad, action, time range.
- Shows who did what (user + agent actions).

Current implementation: the admin screen loads platform audit and metering
summary endpoints when the current user has access.

### 4.8 Providers (platform admin)
- **LLM providers** — register and deprecate the providers squads choose from:
  endpoint, models, and per-token pricing. Providers are not grantable
  resources: a squad picks one at creation and its agents inherit it, so
  provider management lives in its own main-menu section rather than under
  Resources or Admin.

Current implementation: platform admins register and deprecate LLM providers
here (`ProvidersSection`).

### 4.9 Settings → Access (platform admin)

Current implementation (v2 app): the admin **Settings** page carries the
platform-admin surfaces as tabs:

- **AI Models** — LLM providers and their models (register, deprecate,
  pricing on models).
- **Resources** — the registry (skills, tools, APIs, knowledge bases,
  project workspaces).
- **Access** — **Users & access**: every user appears after their first
  OIDC sign-in. Per user: **Make admin / Demote** (explicit confirm
  dialog, `(you)` marker, server-side last-admin guard surfaced as a
  readable message; changes audited as `user.role_changed`) and
  **Manage models** (per-user AI model grants; removals in use by running
  agents require an explicit force retry).

Activate/deactivate of users is **not implemented**; role is the only
per-user control. Platform config (OIDC, idle timeout) is operator-level
(Helm values / env), not an in-app screen.

---

## 5. Role-Aware Views

| Screen | platform_admin | squad owner | granted user |
|--------|:--------------:|:-----------:|:------------:|
| Squad board | ✅ | ✅ (own) | ✅ (`read` grant) |
| Agent panel (identity/permissions) | ✅ | ✅ (own) | — |
| Squad settings (mission/LLM/operating model) | ✅ | ✅ (own) | — |
| Access grants | ✅ | ✅ (own) | — |
| Chat with agent | ✅ | ✅ (own) | ✅ (`talk` grant) |
| Metering (squad) | ✅ | ✅ (own) | — |
| Resources | ✅ | read-only | read-only |
| LLM providers | ✅ | choose for own squads | — |
| Audit | ✅ (all) | ✅ (own) | — |
| User management | ✅ | — | — |

---

## 6. Real-Time Updates

- **Board updates** as agents move tasks — via **WebSocket** (preferred) or
  short **polling** (fallback).
- **Agent state** (idle/busy) updates live.
- **Chat** messages stream in real time.
- The backend exposes a subscription endpoint (or SSE) for live updates.

---

## 7. Accessibility & Polish

- Keyboard navigation for the board (create/assign/move tasks).
- Clear empty states ("No tasks yet — create one").
- Confirmation dialogs for destructive actions.
- Loading + error states for all async operations.
- Responsive layout (desktop-first; usable on tablet).

---

## 8. Tech Notes

- **Framework:** React / Next.js (TypeScript).
- **State:** client state (e.g. TanStack Query for server state + a store for UI).
- **Board:** a Kanban component (e.g. dnd-kit for drag-and-drop).
- **Auth:** OIDC redirect flow; store the JWT securely; attach to API calls.
- **Real-time:** WebSocket / SSE client for board + chat updates.
- **Charts:** a charting lib (e.g. Recharts) for metering.

---

## 9. Open Points

- **Multi-squad dashboard** — an overview across all of a user's squads (later).
- **Task templates** — quick-create from templates (later).
- **Notifications** — in-app + (optional) external notifications (later).
- **Theming / branding** — for self-hosted deployments (later).
- **i18n** — internationalisation (later).
