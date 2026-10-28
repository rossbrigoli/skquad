"use client";

// S-291 (ADR-0016 D8): the Squad Configuration page. First section is
// the Board Policy (reopen cap, self-approve default, leader approval,
// per-agent leader grants — S-297), edited by the squad owner only via
// GET/PUT /squads/{id}/board-policy.
// The page is the designated home for future squad-level knobs.
import { useState } from "react";
import { useParams, useRouter } from "next/navigation";
import { AuthGate } from "../../../../components/AuthGate";
import { AppShell } from "../../../../components/AppShell";
import { ConfirmDialog } from "../../../../components/ConfirmDialog";
import { EmptyState } from "../../../../components/EmptyState";
import { useApi } from "../../../../lib/useApi";
import { useAuth } from "../../../../lib/auth";
import { apiDelete, apiGet, apiPut, type Agent, type BoardPolicyResponse, type LeaderGrant, type Squad } from "../../../../lib/api";

export default function SquadSettingsPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id ?? "");
  const router = useRouter();
  const { user, token } = useAuth();
  const squads = useApi<Squad[]>("/squads");
  const squad = (squads.data || []).find((item) => item.id === squadId);
  // S-304: the squad Danger zone moved here from the Overview tab —
  // destructive squad actions belong under Settings.
  const [deleting, setDeleting] = useState(false);
  // Mirror the backend gate (loadOwnedSquad): board policy is strictly
  // squad-owner only.
  const isOwner = !!squad && !!user && squad.owner_id === user.id;
  const policy = useApi<BoardPolicyResponse>(isOwner ? `/squads/${squadId}/board-policy` : "", 0);
  // S-297: the leader-grants table lists the squad's agents (existing
  // endpoint; leader-capable = holds any grant, per ADR-0016 D5).
  const agents = useApi<Agent[]>(isOwner ? `/squads/${squadId}/agents` : "", 0);

  return (
    <AuthGate>
      <AppShell>
        <div className="section-head">
          <h1 className="page-title" style={{ margin: 0 }}>
            {squad?.name ?? "Squad"} settings
          </h1>
        </div>

        {!squad && !squads.loading ? (
          <EmptyState title="Squad not found" hint="It may have been removed." />
        ) : null}

        {squad && !isOwner ? (
          <div className="notice" role="status">
            Only the squad owner can view and edit board policy.
          </div>
        ) : null}

        {isOwner && policy.error ? (
          <div className="notice error" role="alert">
            {policy.error}
          </div>
        ) : null}

        {isOwner && policy.data ? (
          // Remount on fresh server state so the form re-initialises
          // from the latest effective values *and* raw leader_grants
          // after each save (S-297: raw carries the grants).
          <BoardPolicyForm
            key={JSON.stringify(policy.data)}
            squadId={squadId}
            policy={policy.data}
            agents={agents.data ?? []}
            agentsError={agents.error}
            onSaved={policy.refresh}
          />
        ) : null}

        {/* S-304: Danger zone — destructive delete lives here at the
            bottom of the Settings tab, reusing the agent page's S-178
            pattern (.danger-zone / .danger-zone-row / .danger-zone-title). */}
        <section className="danger-zone" aria-label="Danger zone">
          <h2>Danger zone</h2>
          <div className="danger-zone-row">
            <div>
              <div className="danger-zone-title">Delete this squad</div>
              <p className="field-hint" style={{ margin: 0 }}>
                Removes the squad, its agents, board, tasks, grants and Kubernetes namespace.
                This cannot be undone.
              </p>
            </div>
            <button type="button" className="btn btn-danger" onClick={() => setDeleting(true)} disabled={!squad}>
              Delete squad
            </button>
          </div>
        </section>

        {deleting && squad ? (
          <ConfirmDialog
            title={`Delete squad “${squad.name}”?`}
            body="This removes the squad, its agents, board, tasks, grants and Kubernetes namespace. This cannot be undone."
            confirmText={squad.name}
            onConfirm={async () => {
              await apiDelete(`/squads/${squadId}`, token);
              router.push("/squads");
            }}
            onClose={() => setDeleting(false)}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

// Normalise a grants map: drop all-false/empty entries (equivalent to
// no grant per ADR-0016 D5) and coerce booleans, so dirty-comparison
// and PUT payloads are canonical regardless of server-side shape.
function normalizeGrants(grants: Record<string, LeaderGrant> | null | undefined): Record<string, LeaderGrant> {
  const out: Record<string, LeaderGrant> = {};
  for (const [agentId, grant] of Object.entries(grants ?? {})) {
    const canPrioritize = !!grant?.can_prioritize;
    const canAssignOthers = !!grant?.can_assign_others;
    if (canPrioritize || canAssignOthers) out[agentId] = { can_prioritize: canPrioritize, can_assign_others: canAssignOthers };
  }
  return out;
}

// Read-modify-write merge for the whole-document PUT: unknown agent ids
// keep the freshly-fetched values (never clobbered by a stale local
// copy); known squad agents take this form's edited grants.
function mergeGrants(
  freshRaw: Record<string, LeaderGrant> | null | undefined,
  localGrants: Record<string, LeaderGrant>,
  knownAgentIds: Set<string>,
): Record<string, LeaderGrant> {
  const out = normalizeGrants(freshRaw);
  for (const agentId of Object.keys(out)) {
    if (knownAgentIds.has(agentId)) delete out[agentId];
  }
  for (const [agentId, grant] of Object.entries(localGrants)) {
    if (knownAgentIds.has(agentId)) out[agentId] = grant;
  }
  return out;
}

function BoardPolicyForm({
  squadId,
  policy,
  agents,
  agentsError,
  onSaved,
}: {
  readonly squadId: string;
  readonly policy: BoardPolicyResponse;
  readonly agents: Agent[];
  readonly agentsError: string;
  readonly onSaved: () => void;
}) {
  const { token } = useAuth();
  // The form shows the EFFECTIVE values as the current state (ADR-0016
  // D8); saving PUTs the whole document, so every field is always sent.
  const [reopenCap, setReopenCap] = useState(String(policy.effective.reopen_cap_per_day));
  const [selfApprove, setSelfApprove] = useState(policy.effective.self_approve_default);
  const [leaderApproval, setLeaderApproval] = useState(policy.effective.leader_approval_allowed);
  // S-297: per-agent leader grants (ADR-0016 D5). Initialised from the
  // RAW document; absent agent = no grant = both toggles off.
  const [grants, setGrants] = useState<Record<string, LeaderGrant>>(
    () => normalizeGrants(policy.raw.leader_grants),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [savedNote, setSavedNote] = useState("");

  const capInvalid = reopenCap.trim() === "" || !Number.isInteger(Number(reopenCap)) || Number(reopenCap) < 0;
  const grantsDirty =
    JSON.stringify(grants) !== JSON.stringify(normalizeGrants(policy.raw.leader_grants));
  const dirty =
    Number(reopenCap) !== policy.effective.reopen_cap_per_day ||
    selfApprove !== policy.effective.self_approve_default ||
    leaderApproval !== policy.effective.leader_approval_allowed ||
    grantsDirty;

  function setGrant(agentId: string, key: keyof LeaderGrant, value: boolean) {
    setGrants((prev) => {
      const next: Record<string, LeaderGrant> = { ...prev };
      const current = next[agentId] ?? { can_prioritize: false, can_assign_others: false };
      const updated = { ...current, [key]: value };
      // Drop all-false entries: equivalent to no grant, keeps the
      // stored document tidy (domain treats absent == all-false).
      if (!updated.can_prioritize && !updated.can_assign_others) delete next[agentId];
      else next[agentId] = updated;
      return next;
    });
    setSavedNote("");
  }

  async function save() {
    setBusy(true);
    setError("");
    setSavedNote("");
    try {
      // S-297: the PUT replaces the whole document, so re-fetch it
      // fresh and layer this form's edits on top. Any board_policy key
      // this UI doesn't know about (or that changed since page load)
      // is preserved instead of silently dropped.
      const fresh = await apiGet<BoardPolicyResponse>(`/squads/${squadId}/board-policy`, token);
      await apiPut(`/squads/${squadId}/board-policy`, token, {
        ...(fresh.raw ?? {}),
        reopen_cap_per_day: Number(reopenCap),
        self_approve_default: selfApprove,
        leader_approval_allowed: leaderApproval,
        // S-294/S-297: this form owns grants for agents listed in the
        // squad table; grants for any other agent id pass through from
        // the freshly-fetched document untouched.
        leader_grants: mergeGrants(fresh.raw?.leader_grants, grants, new Set(agents.map((a) => a.id))),
      });
      setSavedNote("Board policy saved.");
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "save failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <section style={{ marginTop: "var(--space-5)" }}>
      <div className="section-head">
        <h2>Board Policy</h2>
      </div>
      <div className="field-row">
        <label className="field">
          <span>Reopen cap per day</span>
          <input
            value={reopenCap}
            onChange={(e) => {
              setReopenCap(e.target.value.replace(/\D/g, ""));
              setSavedNote("");
            }}
            inputMode="numeric"
            aria-label="Reopen cap per day"
          />
          <span className="field-hint">
            Agent-initiated reopens per task per rolling 24h; 0 disables agent reopens.
            Currently effective: {policy.effective.reopen_cap_per_day}.
          </span>
        </label>
      </div>
      {capInvalid ? (
        <p className="field-hint" style={{ color: "var(--danger, #c0392b)" }}>
          Must be a whole number of 0 or more.
        </p>
      ) : null}

      <div className="field">
        <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", cursor: "pointer" }}>
          <input
            type="checkbox"
            checked={selfApprove}
            onChange={(e) => {
              setSelfApprove(e.target.checked);
              setSavedNote("");
            }}
          />
          <span>Self-approve by default</span>
        </label>
        <p className="field-hint">
          Agents may self-close straightforward, self-evident tasks to Done.
          Currently effective: {policy.effective.self_approve_default ? "on" : "off"}. Per-agent overrides win on the
          agent&apos;s Configuration tab.
        </p>
      </div>

      <div className="field">
        <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", cursor: "pointer" }}>
          <input
            type="checkbox"
            checked={leaderApproval}
            onChange={(e) => {
              setLeaderApproval(e.target.checked);
              setSavedNote("");
            }}
          />
          <span>Leader may approve others&apos; tasks</span>
        </label>
        <p className="field-hint">
          Leader-assigned agents can move in-review &rarr; done for other agents&rsquo; tasks;
          off = human approval only. Currently effective: {policy.effective.leader_approval_allowed ? "on" : "off"}.
        </p>
      </div>

      {/* S-297 (ADR-0016 D5): per-agent leader capabilities. A "leader"
          is simply an agent holding at least one grant — the badges below
          reflect that; there is no separate leader designation. */}
      <div className="field">
        <span style={{ fontWeight: 600 }}>Leader capabilities per agent</span>
        {agentsError ? (
          <p className="field-hint" style={{ color: "var(--danger, #c0392b)" }}>
            Could not load squad agents: {agentsError}
          </p>
        ) : null}
        {agents.length === 0 && !agentsError ? (
          <p className="field-hint">No agents in this squad yet.</p>
        ) : null}
        <table aria-label="Leader capabilities per agent" style={{ width: "100%", borderCollapse: "collapse", marginTop: "0.5rem" }}>
          <thead>
            <tr>
              <th style={{ textAlign: "left" }}>Agent</th>
              <th style={{ textAlign: "left" }}>Role</th>
              <th style={{ textAlign: "left" }}>Can prioritize</th>
              <th style={{ textAlign: "left" }}>Can assign others</th>
            </tr>
          </thead>
          <tbody>
            {agents.map((agent) => {
              const grant = grants[agent.id] ?? { can_prioritize: false, can_assign_others: false };
              const isLeaderCapable = grant.can_prioritize || grant.can_assign_others;
              return (
                <tr key={agent.id}>
                  <td>
                    {agent.name}
                    {isLeaderCapable ? (
                      <span className="entity-meta" aria-label={`${agent.name} is leader-capable`}> &bull; leader</span>
                    ) : null}
                  </td>
                  <td className="entity-meta">{agent.role || "—"}</td>
                  <td>
                    <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", cursor: "pointer" }}>
                      <input
                        type="checkbox"
                        checked={grant.can_prioritize}
                        onChange={(e) => setGrant(agent.id, "can_prioritize", e.target.checked)}
                        aria-label={`${agent.name} can prioritize`}
                      />
                      <span className="field-hint">create tasks ready-to-do</span>
                    </label>
                  </td>
                  <td>
                    <label style={{ display: "flex", alignItems: "center", gap: "0.4rem", cursor: "pointer" }}>
                      <input
                        type="checkbox"
                        checked={grant.can_assign_others}
                        onChange={(e) => setGrant(agent.id, "can_assign_others", e.target.checked)}
                        aria-label={`${agent.name} can assign others`}
                      />
                      <span className="field-hint">assign tasks to teammates</span>
                    </label>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
        <p className="field-hint">
          Grants are off by default. &ldquo;Leader&rdquo; here means leader-capable: an agent with at least one
          capability. Toggling both off revokes that agent&rsquo;s leader capabilities on save.
        </p>
      </div>

      <div className="config-save-bar">
        <button
          type="button"
          className="btn btn-primary"
          disabled={!dirty || busy || capInvalid}
          onClick={() => {
            void save();
          }}
        >
          {busy ? "Saving…" : "Save board policy"}
        </button>
        {dirty ? <span className="entity-meta">Unsaved changes.</span> : null}
        {savedNote && !dirty ? <span className="field-hint">{savedNote}</span> : null}
        {capInvalid ? <span className="entity-meta">Reopen cap is invalid.</span> : null}
      </div>
      {error ? (
        <div className="notice error" role="alert">
          {error}
        </div>
      ) : null}
    </section>
  );
}
