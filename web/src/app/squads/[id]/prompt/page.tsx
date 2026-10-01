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
import { useApi } from "../../../../lib/useApi";
import { useAuth } from "../../../../lib/auth";
import { apiPatch, type Agent, type Squad } from "../../../../lib/api";

// Squad Prompt tab (S-PROMPT WP4, plan §6.2). Layer-3 editor for the
// squad prompt. The short `mission` field stays where it already lives
// (Overview → Edit) — mission and prompt coexist by resolved decision Q4:
// mission = short summary for listings, prompt = full layer 3.
export default function SquadPromptPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id ?? "");
  const { token } = useAuth();
  const squads = useApi<Squad[]>("/squads", 0);
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 0);
  const squad = (squads.data || []).find((item) => item.id === squadId);

  const [prompt, setPrompt] = useState("");
  const [loadedOnce, setLoadedOnce] = useState(false);
  const [previewAgent, setPreviewAgent] = useState<Agent | null>(null);

  // Prefill from the server once; don't clobber in-flight edits on refresh.
  useEffect(() => {
    if (squad && !loadedOnce) {
      // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot prefill from the loaded entity
      setPrompt(squad.prompt ?? "");
      setLoadedOnce(true);
    }
  }, [squad, loadedOnce]);

  async function save() {
    await apiPatch<Squad>(`/squads/${squadId}`, token, { prompt });
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
          platform and organization blocks. The squad&rsquo;s <strong>mission</strong> stays on the
          Overview page as the short listing summary.
        </p>
        {squads.error ? <div className="notice error">{squads.error}</div> : null}

        <PromptTierEditor
          scope="squad"
          label="Squad context"
          content={prompt}
          onChange={setPrompt}
          onSave={save}
          saveLabel="Save squad prompt"
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
                    <span className="entity-meta">{agent.role || "no role set"}</span>
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
