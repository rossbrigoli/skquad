"use client";

import { useState } from "react";
import { Modal } from "./Modal";

// Destructive-action confirm. Requires the resource name typed in when
// `confirmText` is provided (squads/agents are hard deletes).
export function ConfirmDialog({
  title,
  body,
  confirmText = "",
  confirmLabel = "Delete",
  onConfirm,
  onClose,
}: {
  title: string;
  body: string;
  confirmText?: string;
  confirmLabel?: string;
  onConfirm: () => Promise<void> | void;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const ready = confirmText === "" || typed.trim() === confirmText;

  return (
    <Modal title={title} onClose={onClose} danger>
      <p style={{ marginTop: 0, color: "var(--ink-muted)" }}>{body}</p>
      {confirmText !== "" ? (
        <label className="field">
          <span>Type “{confirmText}” to confirm</span>
          <input value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
        </label>
      ) : null}
      {error ? <div className="notice error">{error}</div> : null}
      <div className="modal-foot">
        <button type="button" className="btn" onClick={onClose} disabled={busy}>
          Cancel
        </button>
        <button
          type="button"
          className="btn btn-danger"
          disabled={!ready || busy}
          onClick={async () => {
            setBusy(true);
            try {
              await onConfirm();
            } catch (err) {
              setError(err instanceof Error ? err.message : "delete failed");
              setBusy(false);
            }
          }}
        >
          {busy ? "Deleting…" : confirmLabel}
        </button>
      </div>
    </Modal>
  );
}
