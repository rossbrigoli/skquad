"use client";

import { FormEvent } from "react";
import { ApiState, LLMProvider, RegistryResource, ResourceType } from "../lib/api";
import { RegistrySubsection, registrySubsections } from "./Sidebar";
import { StateNotice } from "./shared";

const subsectionToType: Record<Exclude<RegistrySubsection, "llm-providers">, Exclude<ResourceType, "llm_provider">> = {
  skills: "skill",
  tools: "tool",
  apis: "api",
  "knowledge-bases": "knowledge_base",
  "project-workspaces": "project_workspace",
};

export function RegistrySection({
  registrySub,
  providers,
  resources,
  providerForm,
  setProviderForm,
  resourceForm,
  setResourceForm,
  onCreateProvider,
  onCreateResource,
  onDeprecateProvider,
  onDeprecateResource,
  isAdmin,
}: {
  registrySub: RegistrySubsection;
  providers: ApiState<LLMProvider[]>;
  resources: ApiState<RegistryResource[]>;
  providerForm: { name: string; kind: string; base_url: string; api_key_ref: string; default_model: string; models: string };
  setProviderForm: (form: { name: string; kind: string; base_url: string; api_key_ref: string; default_model: string; models: string }) => void;
  resourceForm: { name: string; description: string; endpoint: string; auth_ref: string; manifest: string };
  setResourceForm: (form: { name: string; description: string; endpoint: string; auth_ref: string; manifest: string }) => void;
  onCreateProvider: (event: FormEvent<HTMLFormElement>) => void;
  onCreateResource: (event: FormEvent<HTMLFormElement>) => void;
  onDeprecateProvider: (id: string) => void;
  onDeprecateResource: (resource: RegistryResource) => void;
  isAdmin: boolean;
}) {
  const label = registrySubsections.find((item) => item.id === registrySub)?.label || "";

  if (registrySub === "llm-providers") {
    const providerItems = providers.data || [];
    return (
      <div className="workflow-grid">
        {isAdmin && (
        <form className="form-panel" onSubmit={onCreateProvider}>
          <h3>Register LLM Provider</h3>
          <label>
            Name
            <input value={providerForm.name} onChange={(event) => setProviderForm({ ...providerForm, name: event.target.value })} required />
          </label>
          <label>
            Kind
            <input value={providerForm.kind} onChange={(event) => setProviderForm({ ...providerForm, kind: event.target.value })} required />
          </label>
          <label>
            Base URL
            <input value={providerForm.base_url} onChange={(event) => setProviderForm({ ...providerForm, base_url: event.target.value })} required />
          </label>
          <label>
            API key ref
            <input value={providerForm.api_key_ref} onChange={(event) => setProviderForm({ ...providerForm, api_key_ref: event.target.value })} />
          </label>
          <label>
            Default model
            <input value={providerForm.default_model} onChange={(event) => setProviderForm({ ...providerForm, default_model: event.target.value })} />
          </label>
          <label>
            Models JSON array
            <textarea value={providerForm.models} onChange={(event) => setProviderForm({ ...providerForm, models: event.target.value })} rows={3} placeholder='["gpt-4.1-mini"]' />
          </label>
          <button type="submit">Register Provider</button>
        </form>
        )}

        <div className="span-2">
          <h3 className="panel-title">Provider Catalog{isAdmin ? "" : " (read-only)"}</h3>
          <StateNotice state={providers} empty="No providers registered" />
          {providerItems.length > 0 && (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Kind</th>
                    <th>Model</th>
                    <th>Status</th>
                    {isAdmin && <th />}
                  </tr>
                </thead>
                <tbody>
                  {providerItems.map((provider) => (
                    <tr key={provider.id}>
                      <td>
                        <strong>{provider.name}</strong>
                        <small>{provider.id}</small>
                      </td>
                      <td>{provider.kind}</td>
                      <td>{provider.default_model || "-"}</td>
                      <td>{provider.status}</td>
                      {isAdmin && (
                        <td>
                          <button type="button" className="secondary small" onClick={() => onDeprecateProvider(provider.id)}>
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
