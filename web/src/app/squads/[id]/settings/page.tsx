"use client";

// S-291 (ADR-0016 D8): the Squad Configuration page. First section is
// the Board Policy (reopen cap, self-approve default, leader approval),
// edited by the squad owner only via GET/PUT /squads/{id}/board-policy.
// The page is the designated home for future squad-level knobs.
import { useState } from "react";
import { useParams } from "next/navigation";
import { AuthGate } from "../../../../components/AuthGate";
import { AppShell } from "../../../../components/AppShell";
import { EmptyState } from "../../../../components/EmptyState";
import { useApi } from "../../../../lib/useApi";
import { useAuth } from "../../../../lib/auth";
import { apiPut, type BoardPolicyResponse, type Squad } from "../../../../lib/api";

export default function SquadSettingsPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id ?? "");
  const { user } = useAuth();
  const squads = useApi<Squad[]>("/squads");
  const squad = (squads.data || []).find((item) => item.id === squadId);
  // Mirror the backend gate (loadOwnedSquad): board policy is strictly
  // squad-owner only.
  const isOwner = !!squad && !!user && squad.owner_id === user.id;
  const policy = useApi<BoardPolicyResponse>(isOwner ? `/squads/${squadId}/board-policy` : "", 0);

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
          // from the latest effective values after each save.
          <BoardPolicyForm
            key={JSON.stringify(policy.data.effective)}
            squadId={squadId}
            policy={policy.data}
            onSaved={policy.refresh}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

function BoardPolicyForm({
  squadId,
  policy,
  onSaved,
}: {
  readonly squadId: string;
  readonly policy: BoardPolicyResponse;
  readonly onSaved: () => void;
}) {
  const { token } = useAuth();
  // The form shows the EFFECTIVE values as the current state (ADR-0016
  // D8); saving PUTs the whole document, so every field is always sent.
  const [reopenCap, setReopenCap] = useState(String(policy.effective.reopen_cap_per_day));
  const [selfApprove, setSelfApprove] = useState(policy.effective.self_approve_default);
  const [leaderApproval, setLeaderApproval] = useState(policy.effective.leader_approval_allowed);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [savedNote, setSavedNote] = useState("");

  const capInvalid = reopenCap.trim() === "" || !Number.isInteger(Number(reopenCap)) || Number(reopenCap) < 0;
  const dirty =
    Number(reopenCap) !== policy.effective.reopen_cap_per_day ||
    selfApprove !== policy.effective.self_approve_default ||
    leaderApproval !== policy.effective.leader_approval_allowed;

  async function save() {
    setBusy(true);
    setError("");
    setSavedNote("");
    try {
      await apiPut(`/squads/${squadId}/board-policy`, token, {
        reopen_cap_per_day: Number(reopenCap),
        self_approve_default: selfApprove,
        leader_approval_allowed: leaderApproval,
        // S-294: PUT replaces the whole document — pass the raw
        // leader_grants through so saving knobs here never revokes
        // leader capabilities the owner granted via the API.
        leader_grants: policy.raw.leader_grants ?? {},
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
