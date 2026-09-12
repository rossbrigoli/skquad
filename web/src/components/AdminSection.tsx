"use client";

import { FormEvent } from "react";
import { Agent, ApiState, ApiUser, AuditEntry, LLMProvider, MeteringSummary, Squad } from "../lib/api";
import { StateNotice, formatCost, providerModels } from "./shared";

type ProviderForm = { name: string; kind: string; base_url: string; api_key_ref: string; default_model: string; models: string };

export function AdminSection({
  user,
  selectedSquad,
  selectedAgent,
  metering,
  audit,
  providers,
  providerForm,
  setProviderForm,
  onCreateProvider,
  onDeprecateProvider,
}: {
  user: ApiState<ApiUser>;
  selectedSquad: Squad | null;
  selectedAgent: Agent | null;
  metering: ApiState<MeteringSummary>;
  audit: ApiState<AuditEntry[]>;
  providers: ApiState<LLMProvider[]>;
  providerForm: ProviderForm;
  setProviderForm: (form: ProviderForm) => void;
  onCreateProvider: (event: FormEvent<HTMLFormElement>) => void;
  onDeprecateProvider: (id: string) => void;
}) {
  const entries = audit.data || [];
  const providerItems = providers.data || [];
  return (
    <div className="admin-stack">
      <div className="admin-grid">
        <article>
          <span>Role</span>
          <strong>{user.data?.role || "-"}</strong>
        </article>
        <article>
          <span>User ID</span>
          <strong>{user.data?.id || "-"}</strong>
        </article>
        <article>
          <span>Email</span>
          <strong>{user.data?.email || "-"}</strong>
        </article>
        <article>
          <span>Selected Squad</span>
          <strong>{selectedSquad?.name || "-"}</strong>
        </article>
        <article>
          <span>Selected Agent</span>
          <strong>{selectedAgent?.name || "-"}</strong>
        </article>
        <article>
          <span>Platform Cost</span>
          <strong>{formatCost(metering.data)}</strong>
        </article>
      </div>

      <section>
        <h3 className="panel-title">LLM Providers</h3>
        <p className="field-note">
          Squads choose one of these providers when they are created, and their agents pick from its models.
        </p>
        <div className="workflow-grid">
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

          <div className="span-2">
            <StateNotice state={providers} empty="No providers registered" />
            {providerItems.length > 0 && (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>Kind</th>
                      <th>Models</th>
                      <th>Status</th>
                      <th />
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
                        <td>{providerModels(provider).join(", ") || "-"}</td>
                        <td>{provider.status}</td>
                        <td>
                          {provider.status === "active" && (
                            <button type="button" className="secondary small" onClick={() => onDeprecateProvider(provider.id)}>
                              Deprecate
                            </button>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </div>
      </section>

      <section>
        <h3 className="panel-title">Platform Audit</h3>
        <StateNotice state={audit} empty="No audit entries" />
        {entries.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Action</th>
                  <th>Actor</th>
                  <th>Resource</th>
                  <th>Squad</th>
                  <th>Time</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((entry) => (
                  <tr key={entry.id}>
                    <td>{entry.action}</td>
                    <td>
                      <strong>{entry.actor_type}</strong>
                      <small>{entry.actor_id}</small>
                    </td>
                    <td>
                      <strong>{entry.resource_type}</strong>
                      <small>{entry.resource_id}</small>
                    </td>
                    <td>{entry.squad_id || "-"}</td>
                    <td>{entry.timestamp ? new Date(entry.timestamp).toLocaleString() : "-"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
