"use client";

import { FormEvent } from "react";
import { ApiState, RegistryResource, ResourceType } from "../lib/api";
import { RegistrySubsection, registrySubsections } from "./Sidebar";
import { StateNotice } from "./shared";

// LLM providers are managed under Admin and chosen per squad, so Resources
// covers only the resource types that agents are granted individually.
const subsectionToType: Record<RegistrySubsection, Exclude<ResourceType, "llm_provider">> = {
  skills: "skill",
  tools: "tool",
  apis: "api",
  "knowledge-bases": "knowledge_base",
  "project-workspaces": "project_workspace",
};

export function RegistrySection({
  registrySub,
  resources,
  resourceForm,
  setResourceForm,
  onCreateResource,
  onDeprecateResource,
  isAdmin,
}: {
  registrySub: RegistrySubsection;
  resources: ApiState<RegistryResource[]>;
  resourceForm: { name: string; description: string; endpoint: string; auth_ref: string; manifest: string };
  setResourceForm: (form: { name: string; description: string; endpoint: string; auth_ref: string; manifest: string }) => void;
  onCreateResource: (event: FormEvent<HTMLFormElement>) => void;
  onDeprecateResource: (resource: RegistryResource) => void;
  isAdmin: boolean;
}) {
  const label = registrySubsections.find((item) => item.id === registrySub)?.label || "";
  const activeType = subsectionToType[registrySub];
  const typeItems = (resources.data || []).filter((resource) => resource.type === activeType);
  const filteredState: ApiState<RegistryResource[]> = { ...resources, data: resources.data === null ? null : typeItems };

  return (
    <div className="workflow-grid">
      {isAdmin && (
      <form className="form-panel" onSubmit={onCreateResource}>
        <h3>Register {singular(label)}</h3>
        <label>
          Name
          <input value={resourceForm.name} onChange={(event) => setResourceForm({ ...resourceForm,name: event.target.value })} required />
        </label>
        <label>
          Description
          <textarea value={resourceForm.description} onChange={(event) => setResourceForm({ ...resourceForm,description: event.target.value })} rows={3} />
        </label>
        <label>
          Endpoint
          <input value={resourceForm.endpoint} onChange={(event) => setResourceForm({ ...resourceForm,endpoint: event.target.value })} />
        </label>
        <label>
          Auth ref
          <input value={resourceForm.auth_ref} onChange={(event) => setResourceForm({ ...resourceForm,auth_ref: event.target.value })} />
        </label>
        <label>
          Manifest JSON
          <textarea value={resourceForm.manifest} onChange={(event) => setResourceForm({ ...resourceForm,manifest: event.target.value })} rows={3} />
        </label>
        <button type="submit">Register</button>
      </form>
      )}

      <div className="span-2">
        <h3 className="panel-title">{label} Catalog{isAdmin ? "" : " (read-only)"}</h3>
        <StateNotice state={filteredState} empty={`No ${label.toLowerCase()} registered`} />
        {typeItems.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Endpoint</th>
                  <th>Status</th>
                  {isAdmin && <th />}
                </tr>
              </thead>
              <tbody>
                {typeItems.map((resource) => (
                  <tr key={resource.id}>
                    <td>
                      <strong>{resource.name}</strong>
                      <small>{resource.id}</small>
                    </td>
                    <td>{resource.endpoint || "-"}</td>
                    <td>{resource.status}</td>
                    {isAdmin && (
                      <td>
                        <button type="button" className="secondary small" onClick={() => onDeprecateResource(resource)}>
                          Deprecate
                        </button>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

function singular(label: string): string {
  if (label === "APIs") {
    return "API";
  }
  return label.endsWith("s") ? label.slice(0, -1) : label;
}
