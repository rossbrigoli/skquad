"use client";

import { useParams } from "next/navigation";
import { AuthGate } from "../../../../components/AuthGate";
import { AppShell } from "../../../../components/AppShell";
import { EmptyState } from "../../../../components/EmptyState";
import { SquadRail } from "../../../../components/SquadRail";

export default function SquadBoardPage() {
  const params = useParams<{ id: string }>();
  const squadId = String(params?.id || "");
  return (
    <AuthGate>
      <AppShell secondary={<SquadRail squadId={squadId} squadName="Squad" />}>
        <h1 className="page-title">Board</h1>
        <EmptyState
          title="Kanban board lands in UIv2-3"
          hint="The cockpit currently shows live and stalled work; the full board view is the next card."
        />
      </AppShell>
    </AuthGate>
  );
}
