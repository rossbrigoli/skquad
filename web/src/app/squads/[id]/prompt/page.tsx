"use client";

import { useEffect, useState } from "react";
import { useParams } from "next/navigation";
import { AuthGate } from "../../../../components/AuthGate";
import { AppShell } from "../../../../components/AppShell";
import { EmptyState } from "../../../../components/EmptyState";
import { Modal } from "../../../../components/Modal";
import { EffectivePromptPanel } from "../../../../components/EffectivePromptPanel";
import { PromptRevisionsPanel } from "../../../../components/PromptRevisionsPanel";
import { PromptTierEditor } from "../../../../components/PromptTierEditor";
import { buildContextSavePayload } from "../../../../lib/squadStats";
import { useApi } from "../../../../lib/useApi";
import { useAuth } from "../../../../lib/auth";
import { apiPatch, type Agent, type Squad } from "../../../../lib/api";

// Squad Context tab (S-PROMPT WP4, plan §6.2). Layer-3 editor for the
// squad prompt. S-211: the mission field moved here from the Overview
// Configuration section and is displayed ABOVE the context editor — a
// single "Save context" action persists both. Mission keeps its own DB
// field; the control plane prepends it to the squad context at
// injection time (prompt_handlers.go, S-179 composition).
export default function SquadPromptPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id ?? "");
  const { token } = useAuth();
  const squads = useApi<Squad[]>("/squads", 0);
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 0);
  const squad = (squads.data || []).find((item) => item.id === squadId);

  const [prompt, setPrompt] = useState("");
  const [mission, setMission] = useState("");
  const [loadedOnce, setLoadedOnce] = useState(false);
  const [previewAgent, setPreviewAgent] = useState<Agent | null>(null);

  // Prefill from the server once; don't clobber in-flight edits on refresh.
  useEffect(() => {
    if (squad && !loadedOnce) {
      // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot prefill from the loaded entity
      setPrompt(squad.prompt ?? "");
      setMission(squad.mission ?? "");
      setLoadedOnce(true);
    }
  }, [squad, loadedOnce]);

  // S-211: one click saves BOTH the mission and the squad context.
  async function save() {
    await apiPatch<Squad>(`/squads/${squadId}`, token, buildContextSavePayload(mission, prompt));
    setLoadedOnce(false);
    squads.refresh();
  }

  const agentItems = agents.data || [];

  return (
    <AuthGate>
      <AppShell>
        <div className="section-head">
          <h1 className="page-title" style={{ margin: 0 }}>
            Squad Context
          </h1>
        </div>
        <p className="field-hint" style={{ marginBottom: "var(--space-4)" }}>
          Layer 3 of the prompt hierarchy: injected into every agent in this squad, below the
          platform and organization blocks. Saving here persists both the squad&rsquo;s{" "}
          <strong>mission</strong> and its context in one action; the mission is injected on top
          of the context at compose time.
        </p>
        {squads.error ? <div className="notice error">{squads.error}</div> : null}

        {/* S-211: mission lives above the context editor and is saved with it. */}
        <label className="field" style={{ marginBottom: "var(--space-4)" }}>
          <span>Mission</span>
          <textarea
            value={mission}
            placeholder="What is this squad for?"
            onChange={(e) => setMission(e.target.value)}
            disabled={false}
          />
          <small className="field-hint">
            Short purpose statement. Prepended to the squad context in every agent&apos;s system
            prompt on its next wake.
          </small>
        </label>

        <PromptTierEditor
          scope="squad"
          label="Squad context"
          content={prompt}
          onChange={setPrompt}
          onSave={save}
          saveLabel="Save context"
          placeholder="What this squad is working toward, shared conventions, standing constraints…"
          hint="Template variables like {{agent.name}} or {{squad.roster}} are substituted at compose time. Reserved <skquad_…> delimiters are rejected."
        />

        <section style={{ marginTop: "var(--space-5)" }}>
          <div className="section-head">
            <h2>Effective prompt preview</h2>
          </div>
          <p className="field-hint" style={{ marginBottom: "var(--space-3)" }}>
            What an agent actually receives: platform → organization → squad → agent, composed by
            the control plane. Read-only.
          </p>
          {agentItems.length === 0 && !agents.loading ? (
            <EmptyState title="No agents in this squad" hint="Add an agent to preview its composed prompt." />
          ) : (
            <div className="entity-list">
              {agentItems.map((agent) => (
                <div key={agent.id} className="entity-row">
                  <div className="entity-main">
                    <span className="entity-title">{agent.name}</span>
                    <span className="entity-meta">{agent.role ? agent.role : "no role set"}</span>
                  </div>
                  <div className="entity-side">
                    <button type="button" className="btn btn-sm" onClick={() => setPreviewAgent(agent)}>
                      Preview effective prompt
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>

        <section style={{ marginTop: "var(--space-5)" }}>
          <div className="section-head">
            <h2>Revision history</h2>
          </div>
          <PromptRevisionsPanel
            scope="squad"
            scopeId={squadId}
            onRestore={(content) => {
              setPrompt(content);
            }}
          />
        </section>

        {previewAgent ? (
          <Modal title={`Effective prompt — ${previewAgent.name}`} wide onClose={() => setPreviewAgent(null)}>
            <EffectivePromptPanel agentId={previewAgent.id} />
          </Modal>
        ) : null}
      </AppShell>
    </AuthGate>
  );
}
