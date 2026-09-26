"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import { AgentFormModal } from "../../../../components/AgentForm";
import { AuthGate } from "../../../../components/AuthGate";
import { AppShell } from "../../../../components/AppShell";
import { EmptyState } from "../../../../components/EmptyState";
import { EntityRow } from "../../../../components/EntityRow";
import { SquadRail } from "../../../../components/SquadRail";
import { StatusChip } from "../../../../components/StatusChip";
import { useApi } from "../../../../lib/useApi";
import { useAuth } from "../../../../lib/auth";
import { apiPost } from "../../../../lib/api";
import { agentStatus } from "../../../../lib/status";
import { findModelById, modelLabel } from "../../../../lib/agentLlm";
import type { AIModel } from "../../../../lib/aimodels";
import type { Agent } from "../../../../lib/api";

// boundModelName renders the agent's bound primary model for the list row.
// WP8 step-4: the legacy default_model text is no longer shown anywhere.
function boundModelName(models: AIModel[] | null | undefined, aiModelId: string | undefined): string {
  const m = findModelById(models || [], aiModelId);
  return m ? modelLabel(m) : "no model bound — set in LLM tab";
}

export default function SquadAgentsPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id ?? "");
  const { token } = useAuth();
  const agents = useApi<Agent[]>(`/squads/${squadId}/agents`, 15000);
  const myModels = useApi<AIModel[]>("/models/me", 60000);
  const [creating, setCreating] = useState(false);
  const items = agents.data || [];

  return (
    <AuthGate>
      <AppShell secondary={<SquadRail squadId={squadId} squadName="Squad" />}>
        <div className="section-head">
          <h1 className="page-title" style={{ margin: 0 }}>
            Agents
          </h1>
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            + New agent
          </button>
        </div>
        {agents.error ? <div className="notice error">{agents.error}</div> : null}
        {!agents.loading && items.length === 0 ? (
          <EmptyState
            title="No agents in this squad"
            hint="Agents pick up assigned tasks, scale to zero when idle, and can be talked to directly."
            action={
              <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
                + Add your first agent
              </button>
            }
          />
        ) : (
          <div className="entity-list">
            {items.map((agent) => (
              <EntityRow
                key={agent.id}
                href={`/squads/${squadId}/agents/${agent.id}`}
                title={agent.name}
                meta={`${agent.role ?? "no role"} · ${boundModelName(myModels.data, agent.ai_model_id)}`}
                side={<StatusChip status={agentStatus(agent)} />}
              />
            ))}
          </div>
        )}
        {creating ? (
          <AgentFormModal
            title="New agent"
            submitLabel="Create agent"
            onClose={() => setCreating(false)}
            onSubmit={async (values) => {
              await apiPost<Agent>(`/squads/${squadId}/agents`, token, values);
              setCreating(false);
              agents.refresh();
            }}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}
