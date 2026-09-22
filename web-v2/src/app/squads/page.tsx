"use client";

import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { EmptyState } from "../../components/EmptyState";
import { EntityRow } from "../../components/EntityRow";
import { useApi } from "../../lib/useApi";
import type { Squad } from "../../lib/api";

export default function SquadsPage() {
  const squads = useApi<Squad[]>("/squads");
  const items = squads.data || [];

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Squads</h1>
        {squads.error ? <div className="notice error">{squads.error}</div> : null}
        {!squads.loading && items.length === 0 ? (
          <EmptyState
            title="No squads yet"
            hint="Create a squad from the v1 UI or the API, then come back to drive it here."
          />
        ) : (
          <div className="entity-list">
            {items.map((squad) => (
              <EntityRow
                key={squad.id}
                href={`/squads/${squad.id}`}
                title={squad.name}
                meta={squad.mission || "no mission set"}
                side={squad.namespace ? <span className="mono">{squad.namespace}</span> : undefined}
              />
            ))}
          </div>
        )}
      </AppShell>
    </AuthGate>
  );
}
