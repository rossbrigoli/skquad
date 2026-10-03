"use client";

import { useState } from "react";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { EmptyState } from "../../components/EmptyState";
import { EntityRow } from "../../components/EntityRow";
import { Modal, ModalForm } from "../../components/Modal";
import { PromptTemplatePicker } from "../../components/PromptTemplatePicker";
import { apiPost } from "../../lib/api";
import { useAuth } from "../../lib/auth";
import { useApi } from "../../lib/useApi";
import type { Squad } from "../../lib/api";
import { squadDisplayLabel } from "../../lib/menu";

export default function SquadsPage() {
  const { token, user } = useAuth();
  // S-166: platform admins see every squad, labelled with its owner.
  const isAdmin = user?.role === "platform_admin";
  const squads = useApi<Squad[]>(isAdmin ? "/squads?all=true" : "/squads");
  const allUsers = useApi<{ id: string; name?: string; email?: string }[]>(isAdmin ? "/users" : "");
  const ownerLabels = isAdmin
    ? {
        meId: user?.id,
        nameFor: (ownerId: string) => {
          const u = (allUsers.data ?? []).find((x) => x.id === ownerId);
          return u?.name || u?.email?.split("@")[0] || undefined;
        },
      }
    : undefined;
  const items = squads.data || [];
  const [creating, setCreating] = useState(false);

  return (
    <AuthGate>
      <AppShell>
        <div className="section-head">
          <h1 className="page-title" style={{ margin: 0 }}>
            Squads
          </h1>
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            + New squad
          </button>
        </div>
        {squads.error ? <div className="notice error">{squads.error}</div> : null}
        {!squads.loading && items.length === 0 ? (
          <EmptyState
            title="No squads yet"
            hint="A squad owns agents, a board, and a namespace. Create one to get started."
            action={
              <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
                + Create your first squad
              </button>
            }
          />
        ) : (
          <div className="entity-list">
            {items.map((squad) => (
              <EntityRow
                key={squad.id}
                href={`/squads/${squad.id}`}
                title={squadDisplayLabel(squad, ownerLabels)}
                meta={squad.mission || "no mission set"}
                side={squad.namespace ? <span className="mono">{squad.namespace}</span> : undefined}
              />
            ))}
          </div>
        )}
        {creating ? (
          <SquadCreateModal
            onClose={() => setCreating(false)}
            onCreated={() => {
              setCreating(false);
              squads.refresh();
            }}
            token={token}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

function SquadCreateModal({
  onClose,
  onCreated,
  token,
}: {
  onClose: () => void;
  onCreated: () => void;
  token: string;
}) {
  const [name, setName] = useState("");
  const [mission, setMission] = useState("");
  const [prompt, setPrompt] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Modal title="New squad" wider onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        onCancel={onClose}
        submitLabel="Create squad"
        submitDisabled={name.trim() === ""}
        onSubmit={async () => {
          setBusy(true);
          setError("");
          try {
            await apiPost<Squad>("/squads", token, {
              name: name.trim(),
              mission: mission.trim(),
              prompt: prompt.trim(),
            });
            onCreated();
          } catch (err) {
            setError(err instanceof Error ? err.message : "create failed");
            setBusy(false);
          }
        }}
      >
        <label className="field">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. release-team" autoFocus />
        </label>
        <label className="field">
          <span>Mission</span>
          <textarea value={mission} onChange={(e) => setMission(e.target.value)} placeholder="What this squad is for" />
        </label>
        <PromptTemplatePicker target="squad" currentPrompt={prompt} onApply={(content) => setPrompt(content)} />
        <label className="field">
          <span>Squad context (optional)</span>
          <textarea
            className="prompt-tall"
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="Operating instructions for every agent in this squad"
          />
          <small className="field-hint">
            Optional. Sits between the organization and agent context layers; you can keep editing it later on the
            squad&rsquo;s Squad Context tab.
          </small>
        </label>
      </ModalForm>
    </Modal>
  );
}
