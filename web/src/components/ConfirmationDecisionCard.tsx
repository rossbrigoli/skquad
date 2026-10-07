"use client";

// TG-8 slice D: the owner's 3-action confirmation card, rendered inside
// the inbox reading view for action_required messages that are gated-call
// confirmations (docs/tg8-grant-approvals-spec.md §C/§D).
//
// Join strategy: the C1 inbox message carries NO structured confirmation
// id — the confirmation row carries `inbox_message_id` — so the card
// fetches the caller's pending confirmations (GET /confirmations?mine=true
// &state=pending) and matches on that link. No match ⇒ renders nothing
// (the action_required message is some other flow, e.g. a grant review).
//
// THREE buttons exactly: Deny (opens a reason input) / Approve (one-shot)
// / Approve This and Future (expiry picker, default +90 days). Optimistic
// UI: the card flips to its decided badge on click; a failed call reverts
// the card and surfaces an error toast.

import { useCallback, useEffect, useState } from "react";
import { useAuth } from "../lib/auth";
import {
  approveConfirmationOnce,
  approveConfirmationStanding,
  denyConfirmation,
  listConfirmations,
} from "../lib/grantsApi";
import {
  confirmationDecisionChip,
  confirmationDecisionLabel,
  dateInputToIso,
  defaultExpiryDateString,
  matchConfirmationForInboxMessage,
  shortArgsHash,
  type ConfirmationState,
  type PendingConfirmation,
} from "../lib/grantsWorkflow";

type Decision = "approved_once" | "approved_standing" | "denied";

function errMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

export function ConfirmationDecisionCard({ messageId }: { readonly messageId: string }) {
  const { token } = useAuth();
  const [confirmation, setConfirmation] = useState<PendingConfirmation | null>(null);
  const [decided, setDecided] = useState<ConfirmationState | "">("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [showDeny, setShowDeny] = useState(false);
  const [denyReason, setDenyReason] = useState("");
  const [showStanding, setShowStanding] = useState(false);
  const [expiryDate, setExpiryDate] = useState(() => defaultExpiryDateString());

  useEffect(() => {
    let cancelled = false;
    listConfirmations(token, { mine: true, state: "pending" })
      .then((list) => {
        if (cancelled) return;
        const match = matchConfirmationForInboxMessage(Array.isArray(list) ? list : [], messageId);
        if (match && match.state === "pending") setConfirmation(match);
      })
      .catch(() => {
        // A failed lookup hides the card; the inbox message itself still
        // renders. The decision can be retried by reopening the message.
      });
    return () => {
      cancelled = true;
    };
  }, [token, messageId]);

  const runDecision = useCallback(
    async (kind: Decision, action: () => Promise<unknown>) => {
      if (!confirmation || busy) return;
      const optimistic: ConfirmationState =
        kind === "approved_once" ? "approved_once" : kind === "approved_standing" ? "approved_standing" : "denied";
      // Optimistic flip: badge + disabled buttons land immediately.
      setBusy(true);
      setError("");
      setShowDeny(false);
      setShowStanding(false);
      setDecided(optimistic);
      try {
        await action();
      } catch (err) {
        // Revert so the owner can retry; toast the failure.
        setDecided("");
        setError(errMessage(err, "decision failed"));
      } finally {
        setBusy(false);
      }
    },
    [confirmation, busy],
  );

  if (!confirmation) return null;

  const decidedState = decided || confirmation.state;
  const isDecided = decidedState !== "pending";

  return (
    <section className="confirmation-card" aria-label="Confirmation decision">
      <div className="confirmation-card-head">
        <span className="confirmation-tool">{confirmation.tool}</span>
        <span className="confirmation-meta">
          agent <strong>{confirmation.agent_id}</strong> · resource{" "}
          <strong>{confirmation.resource_id}</strong> · args{" "}
          <code>{shortArgsHash(confirmation.args_hash)}</code>
        </span>
        {isDecided ? (
          <span className={confirmationDecisionChip(decidedState)}>
            {confirmationDecisionLabel(decidedState)}
          </span>
        ) : null}
      </div>
      {error ? (
        <div className="notice error confirmation-toast" role="alert">
          {error}
        </div>
      ) : null}
      {isDecided ? (
        <p className="confirmation-decided-note">
          Decision recorded — this card is final. Manage standing authorizations under
          Settings → Standing Grants.
        </p>
      ) : (
        <div className="confirmation-actions">
          {showDeny ? (
            <div className="confirmation-deny-form">
              <label className="field">
                <span>Reason (shared with the agent)</span>
                <input
                  value={denyReason}
                  onChange={(e) => setDenyReason(e.target.value)}
                  placeholder="Why is this denied?"
                  aria-label="Denial reason"
                />
              </label>
              <div className="confirmation-action-row">
                <button
                  type="button"
                  className="btn btn-danger"
                  disabled={busy}
                  onClick={() => runDecision("denied", () => denyConfirmation(token, confirmation.id, denyReason.trim() || "denied"))}
                >
                  Confirm Deny
                </button>
                <button type="button" className="btn" disabled={busy} onClick={() => setShowDeny(false)}>
                  Cancel
                </button>
              </div>
            </div>
          ) : showStanding ? (
            <div className="confirmation-standing-form">
              <label className="field">
                <span>Standing grant expires</span>
                <input
                  type="date"
                  value={expiryDate}
                  min={new Date().toISOString().slice(0, 10)}
                  onChange={(e) => setExpiryDate(e.target.value)}
                  aria-label="Standing grant expiry date"
                />
              </label>
              <div className="confirmation-action-row">
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={busy || dateInputToIso(expiryDate) === ""}
                  onClick={() =>
                    runDecision("approved_standing", () =>
                      approveConfirmationStanding(token, confirmation.id, dateInputToIso(expiryDate)),
                    )
                  }
                >
                  Confirm standing approval
                </button>
                <button type="button" className="btn" disabled={busy} onClick={() => setShowStanding(false)}>
                  Cancel
                </button>
              </div>
            </div>
          ) : (
            <>
              <button
                type="button"
                className="btn btn-danger confirmation-btn-deny"
                disabled={busy}
                onClick={() => setShowDeny(true)}
              >
                Deny
              </button>
              <button
                type="button"
                className="btn btn-primary confirmation-btn-approve"
                disabled={busy}
                onClick={() => runDecision("approved_once", () => approveConfirmationOnce(token, confirmation.id))}
              >
                Approve
              </button>
              <button
                type="button"
                className="btn confirmation-btn-standing"
                disabled={busy}
                onClick={() => setShowStanding(true)}
              >
                Approve This and Future
              </button>
            </>
          )}
        </div>
      )}
    </section>
  );
}
