"use client";

// S-204 follow-up: Resources index page.
//
// The breadcrumb trail "Settings / Resources / Tools / …" links every
// level, so /settings/resources must be a real page. It lists the
// resource categories; Tools links to the unified tile grid, the other
// categories to the classic registry list at /settings/resources/<type>.

import Link from "next/link";
import { AppShell } from "../../../components/AppShell";
import { AuthGate } from "../../../components/AuthGate";
import { RESOURCE_CATEGORIES, resourceCategoryHref } from "../../../lib/resourceCategories";

export default function ResourcesIndexPage() {
  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Resources</h1>
        <p className="page-subtitle">
          Everything agents can be granted access to. Pick a category to browse and manage it.
        </p>
        <div className="resource-index">
          {RESOURCE_CATEGORIES.map((c) => (
            <Link key={c.key} href={resourceCategoryHref(c.key)} className="resource-index-card">
              <span className="resource-index-label">{c.label}</span>
              <span className="resource-index-desc">{c.description}</span>
            </Link>
          ))}
        </div>
      </AppShell>
    </AuthGate>
  );
}
