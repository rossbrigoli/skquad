"use client";

import { useEffect, useState } from "react";
import { PromptRevisionsPanel } from "./PromptRevisionsPanel";
import { PromptTierEditor } from "./PromptTierEditor";
import { useAuth } from "../lib/auth";
import { useApi } from "../lib/useApi";
import { apiPut } from "../lib/api";
import { formatRelativeTime } from "../lib/format";

export type OrgPromptSettings = {
  org_name: string;
  org_prompt: string;
  tokens: number;
  soft_warn: number;
  hard_cap: number;
  warnings?: string[];
  updated_at: string;
  updated_by: string;
};

// OrganizationPromptTab — admin-only editor for the organization prompt
// tier (layer 2). Loads GET /settings/prompt, saves via
// PUT /settings/prompt; the editor surfaces the same meter +
// validate-on-edit battery as the squad/agent tiers. Restore from a
// revision prefills the editor; saving appends a new revision.
export function OrganizationPromptTab() {
  const { token } = useAuth();
  const settings = useApi<OrgPromptSettings>("/settings/prompt", 0);
  const [orgName, setOrgName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [loadedOnce, setLoadedOnce] = useState(false);

  // Prefill from the server once; don't clobber in-flight edits on refresh.
  useEffect(() => {
    if (settings.data && !loadedOnce) {
      // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot prefill from the loaded entity
      setOrgName(settings.data.org_name ?? "");
      setPrompt(settings.data.org_prompt ?? "");
      setLoadedOnce(true);
    }
  }, [settings.data, loadedOnce]);

  async function save() {
    await apiPut<OrgPromptSettings>("/settings/prompt", token, {
      org_name: orgName.trim(),
      org_prompt: prompt,
    });
    setLoadedOnce(false);
    settings.refresh();
  }

  return (
    <section>
      <div className="section-head">
        <h2>Organization prompt</h2>
      </div>
      <p className="field-hint" style={{ marginBottom: "var(--space-4)" }}>
        Layer 2 of the prompt hierarchy: shared context injected into every agent in this
        instance, below the platform block and above squad and agent prompts.
      </p>
      {settings.error ? <div className="notice error">{settings.error}</div> : null}
      {settings.data ? (
        <p className="entity-meta" style={{ marginBottom: "var(--space-3)" }}>
          Last saved {formatRelativeTime(settings.data.updated_at)} by {settings.data.updated_by || "unknown"}
        </p>
      ) : null}

      <div className="field-row">
        <label className="field">
          <span>Organization name (optional)</span>
          <input value={orgName} onChange={(e) => setOrgName(e.target.value)} placeholder="Acme Corp" />
        </label>
      </div>

      <PromptTierEditor
        scope="organization"
        label="Organization prompt"
        content={prompt}
        onChange={setPrompt}
        onSave={save}
        saveLabel="Save org prompt"
        placeholder="Context every agent in this organization should know…"
        hint="Template variables like {{agent.name}} or {{squad.name}} are substituted at compose time. Reserved <skquad_…> delimiters are rejected."
      />

      <div style={{ marginTop: "var(--space-5)" }}>
        <div className="section-head">
          <h2>Revision history</h2>
        </div>
        <PromptRevisionsPanel
          scope="organization"
          onRestore={(content) => {
            setPrompt(content);
          }}
        />
      </div>
    </section>
  );
}
