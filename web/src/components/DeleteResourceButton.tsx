"use client";

// S-103 (extracted to a shared component for S-204): tries a plain
// delete; if the API reports the resource is granted to agents (409),
// warns with the usage list and offers a force delete that also revokes
// those grants.

import { useState } from "react";
import { apiDelete, ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import { ConfirmDialog } from "./ConfirmDialog";

export type DeleteUsage = { agent_id: string; agent_name: string; squad_id: string };

export function DeleteResourceButton({
  path,
  name,
  onDeleted,
}: {
  readonly path: string;
  readonly name: string;
  readonly onDeleted: () => void;
}) {
  const { token } = useAuth();
  const [usage, setUsage] = useState<DeleteUsage[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function attempt(force: boolean) {
    setBusy(true);
    setError("");
    try {
      await apiDelete(path + (force ? "?force=true" : ""), token);
      setUsage(null);
      onDeleted();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        const body = err.body as { usage?: DeleteUsage[] } | undefined;
        setUsage(body?.usage ?? []);
      } else {
        setError(err instanceof Error ? err.message : "delete failed");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <button
        type="button"
        className="btn btn-sm btn-danger"
        disabled={busy}
        onClick={() => {
              attempt(false);
            }}
      >
        Delete
      </button>
      {error ? (
        <span className="notice error" role="alert" style={{ marginLeft: 8 }}>
          {error}
        </span>
      ) : null}
      {usage !== null ? (
        <ConfirmDialog
          title={`Delete “${name}”?`}
          body={
            usage.length > 0
              ? `This is currently granted to ${usage.length} agent(s): ${usage
                  .map((u) => u.agent_name)
                  .join(", ")}. Deleting will revoke those grants.`
              : "Delete this resource?"
          }
          confirmLabel="Delete and revoke"
          onConfirm={async () => {
            await attempt(true);
          }}
          onClose={() => setUsage(null)}
        />
      ) : null}
    </>
  );
}
