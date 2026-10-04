"use client";

// S-204 follow-up: canonical Tools tiles page.
//
// /settings/resources/tools is now a real route (it used to exist only
// as a tab inside /settings, so the breadcrumb link 404'd). Renders the
// unified tile grid (built-ins + registered tools) with the register
// affordance in the header. Tile clicks go to /settings/resources/tools/
// {toolId}; that page carries a "Back to Tools" link to this route.

import { useState } from "react";
import { AppShell } from "../../../../components/AppShell";
import { AuthGate } from "../../../../components/AuthGate";
import { ResourceModal, resourceLabel } from "../../../../components/ResourceRegistryPanel";
import { ToolsPanel } from "../../../../components/ToolsPanel";
import { useAuth } from "../../../../lib/auth";
import { isPlatformAdmin } from "../../../../lib/aimodels";

export default function ToolsPage() {
  const { user } = useAuth();
  const isAdmin = isPlatformAdmin(user?.role);
  const [creating, setCreating] = useState(false);

  return (
    <AuthGate>
      <AppShell>
        <div className="section-head">
          <h1 className="page-title">Tools</h1>
          {isAdmin ? (
            <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
              + Register {resourceLabel("tools")}
            </button>
          ) : null}
        </div>
        {!isAdmin ? (
          <div className="notice" style={{ marginBottom: "var(--space-4)" }}>
            You are signed in as <strong>{user?.role ?? "user"}</strong>. Registering or changing tools requires the{" "}
            <strong>platform_admin</strong> role.
          </div>
        ) : null}
        <ToolsPanel isAdmin={isAdmin} />
        {creating ? (
          <ResourceModal
            resourceType="tools"
            resource={null}
            onClose={() => setCreating(false)}
            onSaved={() => setCreating(false)}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}
