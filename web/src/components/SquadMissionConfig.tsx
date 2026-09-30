"use client";

// S-179: inline squad mission configuration. Replaces the old "Edit
// squad" dialog on the squad screen — the mission textarea lives in the
// Configuration section with a Save button next to it. The squad name is
// deliberately absent: it is immutable (bound to the K8s namespace).
import { useState } from "react";
import { apiPatch } from "../lib/api";
import type { Squad } from "../lib/api";

export function SquadMissionConfig({
  squad,
  token,
  onSaved,
}: {
  squad: Squad;
  token: string;
  onSaved: () => void;
}) {
  const [mission, setMission] = useState(squad.mission ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [justSaved, setJustSaved] = useState(false);

  const dirty = mission.trim() !== (squad.mission ?? "").trim();

  async function save() {
    setBusy(true);
    setError("");
    setJustSaved(false);
    try {
      await apiPatch<Squad>(`/squads/${squad.id}`, token, { mission: mission.trim() });
      setJustSaved(true);
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "update failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="mission-config">
      <div style={{ display: "flex", gap: "var(--space-3)", alignItems: "flex-start" }}>
        <label className="field" style={{ flex: 1, marginBottom: 0 }}>
          <span>Mission</span>
          <textarea
            value={mission}
            placeholder="What is this squad for?"
            onChange={(e) => {
              setMission(e.target.value);
              setJustSaved(false);
            }}
          />
          <small className="field-hint">
            The mission is injected into every agent&apos;s system prompt on its next wake.
          </small>
        </label>
        <button
          type="button"
          className="btn btn-primary"
          onClick={save}
          disabled={!dirty || busy}
          style={{ marginTop: "var(--space-6)" }}
        >
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
      {error ? (
        <p role="alert" style={{ color: "#c0392b", fontSize: "var(--text-sm)", marginBottom: 0 }}>
          {error}
        </p>
      ) : justSaved && !dirty ? (
        <p style={{ color: "var(--ink-muted)", fontSize: "var(--text-sm)", marginBottom: 0 }}>Saved.</p>
      ) : null}
    </div>
  );
}
