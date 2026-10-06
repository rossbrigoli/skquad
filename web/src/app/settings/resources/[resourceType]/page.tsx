"use client";

// S-204 follow-up: generic resource category page.
//
// Real route for the classic registry resource types
// (/settings/resources/skills, /apis, /knowledge-bases,
// /project-workspaces) so every breadcrumb level under Settings >
// Resources resolves. "tools" is served by the static route
// app/settings/resources/tools (tile grid) — Next.js prefers it over
// this dynamic segment, so it never renders here. Unknown types render
// a not-found state instead of hitting an arbitrary registry endpoint.

import { notFound, useParams } from "next/navigation";
import { AppShell } from "../../../../components/AppShell";
import { AuthGate } from "../../../../components/AuthGate";
import { ResourceRegistryPanel } from "../../../../components/ResourceRegistryPanel";
import { RestResourcePanel } from "../../../../components/RestResourcePanel";
import { GitResourcePanel } from "../../../../components/GitResourcePanel";
import { useAuth } from "../../../../lib/auth";
import { isPlatformAdmin } from "../../../../lib/aimodels";
import { findResourceCategory } from "../../../../lib/resourceCategories";

export default function ResourceCategoryPage() {
  const params = useParams<{ resourceType: string }>();
  const raw = Array.isArray(params?.resourceType) ? params.resourceType[0] : params?.resourceType ?? "";
  const { user } = useAuth();
  const isAdmin = isPlatformAdmin(user?.role);
  const category = findResourceCategory(raw);
  if (!category) {
    notFound();
  }

  // TG-4: the "apis" category is the BYO REST surface — a purpose-built
  // registration form (write-only credentials + policy ceiling) instead
  // of the generic registry panel.
  if (category.key === "apis") {
    return (
      <AuthGate>
        <AppShell>
          <RestResourcePanel isAdmin={isAdmin} />
        </AppShell>
      </AuthGate>
    );
  }

  // TG-4b: the "git" category is the BYO git surface — a purpose-built
  // registration form (write-only bearer PAT + repo ceiling) instead of
  // the generic registry panel.
  if (category.key === "git") {
    return (
      <AuthGate>
        <AppShell>
          <GitResourcePanel isAdmin={isAdmin} />
        </AppShell>
      </AuthGate>
    );
  }

  return (
    <AuthGate>
      <AppShell>
        <ResourceRegistryPanel resourceType={category.key} label={category.label} isAdmin={isAdmin} />
      </AppShell>
    </AuthGate>
  );
}
