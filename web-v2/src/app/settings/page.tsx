"use client";

import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { EmptyState } from "../../components/EmptyState";

export default function SettingsPage() {
  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Settings</h1>
        <EmptyState
          title="Platform settings move here"
          hint="Providers, resource registry, grants and audit migrate from the v1 sidebar into this contextual area (UIv2-3+)."
        />
      </AppShell>
    </AuthGate>
  );
}
