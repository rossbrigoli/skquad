"use client";

import { FormEvent, useEffect, useState } from "react";
import {
  Agent,
  AgentIdentity,
  AgentPermission,
  ApiError,
  ApiState,
  ApiUser,
  AccessGrant,
  AuditEntry,
  BoardPayload,
  LLMProvider,
  MeteringSummary,
  Message,
  RegistryResource,
  ResourceType,
  Squad,
  Task,
  TaskStatus,
  apiBaseUrl,
  apiDelete,
  apiGet,
  apiPatch,
  apiPost,
  apiPut,
} from "../lib/api";
import { TopNav } from "../components/TopNav";
import { RegistrySubsection, Section, Sidebar } from "../components/Sidebar";
import { SquadTab, SquadsSection } from "../components/SquadsSection";
import { RegistrySection } from "../components/RegistrySection";
import { AdminSection } from "../components/AdminSection";
import { registryTypes } from "../components/shared";

const emptyState = {
  data: null,
  loading: true,
  error: "",
};

const emptyResourceForm = { name: "", description: "", endpoint: "", auth_ref: "", manifest: "{}" };

export default function Home() {
  const [token, setToken] = useState("");
  const [draftToken, setDraftToken] = useState("");
  const [activeSection, setActiveSection] = useState<Section>("squads");
  const [registrySub, setRegistrySub] = useState<RegistrySubsection>("llm-providers");
  const [squadTab, setSquadTab] = useState<SquadTab>("overview");
  const [selectedSquadID, setSelectedSquadID] = useState("");
  const [selectedAgentID, setSelectedAgentID] = useState("");
  const [refreshTick, setRefreshTick] = useState(0);
  const [actionMessage, setActionMessage] = useState("");
  const [user, setUser] = useState<ApiState<ApiUser>>(emptyState);
  const [squads, setSquads] = useState<ApiState<Squad[]>>(emptyState);
  const [agents, setAgents] = useState<ApiState<Agent[]>>({ data: [], loading: false, error: "" });
  const [board, setBoard] = useState<ApiState<BoardPayload>>({ data: null, loading: false, error: "" });
  const [chat, setChat] = useState<ApiState<Message[]>>({ data: [], loading: false, error: "" });
  const [providers, setProviders] = useState<ApiState<LLMProvider[]>>({ data: [], loading: false, error: "" });
  const [resources, setResources] = useState<ApiState<RegistryResource[]>>({ data: [], loading: false, error: "" });
  const [agentPermissions, setAgentPermissions] = useState<ApiState<AgentPermission[]>>({ data: [], loading: false, error: "" });
  const [accessGrants, setAccessGrants] = useState<ApiState<AccessGrant[]>>({ data: [], loading: false, error: "" });
  const [audit, setAudit] = useState<ApiState<AuditEntry[]>>({ data: [], loading: false, error: "" });
  const [metering, setMetering] = useState<ApiState<MeteringSummary>>({ data: null, loading: false, error: "" });
  const [squadMetering, setSquadMetering] = useState<ApiState<MeteringSummary>>({ data: null, loading: false, error: "" });
  const [squadAudit, setSquadAudit] = useState<ApiState<AuditEntry[]>>({ data: [], loading: false, error: "" });
  const [agentCosts, setAgentCosts] = useState<Record<string, MeteringSummary>>({});
  const [newSquadForm, setNewSquadForm] = useState({ name: "", mission: "" });
  const [squadMissionDraft, setSquadMissionDraft] = useState("");
  const [agentForm, setAgentForm] = useState({ name: "", role: "", default_model: "", idle_timeout_sec: "300" });
  const [taskForm, setTaskForm] = useState({ title: "", description: "", assignee_agent_id: "" });
  const [chatDraft, setChatDraft] = useState("");
  const [providerForm, setProviderForm] = useState({ name: "", kind: "openai", base_url: "", api_key_ref: "", default_model: "", models: "" });
  const [resourceForm, setResourceForm] = useState(emptyResourceForm);
  const [permissionForm, setPermissionForm] = useState({ resource_type: "llm_provider" as ResourceType, resource_id: "" });
  const [grantForm, setGrantForm] = useState({ grantee_type: "user" as "user" | "agent", grantee_id: "", permissions: "talk" });

  useEffect(() => {
    const stored = window.localStorage.getItem("skquad.authToken") || "";
    setToken(stored);
    setDraftToken(stored);
  }, []);

  // Each registry subsection registers a different resource type, so values
  // typed for one type should not carry into another type's form.
  useEffect(() => {
    setResourceForm(emptyResourceForm);
  }, [registrySub]);

  useEffect(() => {
    let cancelled = false;
    setUser({ data: null, loading: true, error: "" });
    setSquads({ data: null, loading: true, error: "" });

    Promise.allSettled([
      apiGet<ApiUser>("/auth/me", token),
      apiGet<Squad[]>("/squads", token),
    ]).then(([userResult, squadResult]) => {
      if (cancelled) {
        return;
      }
      const nextSquads = stateFromResult(squadResult, []);
      setUser(stateFromResult(userResult));
      setSquads(nextSquads);
      const list = nextSquads.data || [];
      if (list.length > 0 && !list.some((squad) => squad.id === selectedSquadID)) {
        setSelectedSquadID(list[0].id);
      }
      if (list.length === 0) {
        setSelectedSquadID("");
      }
    });

    return () => {
      cancelled = true;
    };
  }, [token, refreshTick, selectedSquadID]);

  useEffect(() => {
    if (!selectedSquadID) {
      setAgents({ data: [], loading: false, error: "" });
      setBoard({ data: null, loading: false, error: "" });
      setSelectedAgentID("");
      return;
    }

    let cancelled = false;
    setAgents({ data: null, loading: true, error: "" });
    setBoard({ data: null, loading: true, error: "" });
    Promise.allSettled([
      apiGet<Agent[]>(`/squads/${selectedSquadID}/agents`, token),
      apiGet<BoardPayload>(`/squads/${selectedSquadID}/board`, token),
    ]).then(([agentResult, boardResult]) => {
      if (cancelled) {
        return;
      }
      const nextAgents = stateFromResult(agentResult, []);
      setAgents(nextAgents);
      setBoard(stateFromResult(boardResult));
      const list = nextAgents.data || [];
      if (list.length > 0 && !list.some((agent) => agent.id === selectedAgentID)) {
        setSelectedAgentID(list[0].id);
      }
      if (list.length === 0) {
        setSelectedAgentID("");
      }
    });

    return () => {
      cancelled = true;
    };
  }, [selectedSquadID, token, refreshTick, selectedAgentID]);

  useEffect(() => {
    const selected = (squads.data || []).find((squad) => squad.id === selectedSquadID);
    if (selected) {
      setSquadMissionDraft(selected.mission || "");
    }
  }, [selectedSquadID, squads.data]);

  useEffect(() => {
    if (!selectedAgentID) {
      setChat({ data: [], loading: false, error: "" });
      return;
    }
    let cancelled = false;
    setChat({ data: null, loading: true, error: "" });
    apiGet<Message[]>(`/agents/${selectedAgentID}/chat`, token)
      .then((messages) => {
        if (!cancelled) {
          setChat({ data: messages, loading: false, error: "" });
        }
      })
      .catch((error) => {
        if (!cancelled) {
          setChat(errorState(error, []));
        }
      });
    return () => {
      cancelled = true;
    };
  }, [selectedAgentID, token, refreshTick]);

  useEffect(() => {
    if (activeSection !== "registry" && activeSection !== "squads") {
      return;
    }
    let cancelled = false;
    setProviders({ data: null, loading: true, error: "" });
    setResources({ data: null, loading: true, error: "" });
    Promise.allSettled([
      apiGet<LLMProvider[]>("/registry/llm-providers", token),
      ...registryTypes.map((item) => apiGet<RegistryResource[]>(`/registry/${item.path}`, token)),
    ]).then(([providerResult, ...resourceResults]) => {
      if (cancelled) {
        return;
      }
      setProviders(stateFromResult(providerResult, []));
      const combined: RegistryResource[] = [];
      let error = "";
      for (const result of resourceResults) {
        const state = stateFromResult(result, []);
        if (state.error && !error) {
          error = state.error;
        }
        combined.push(...(state.data || []));
      }
      setResources({ data: combined, loading: false, error });
    });
    return () => {
      cancelled = true;
    };
  }, [activeSection, token, refreshTick]);

  useEffect(() => {
    if (!selectedAgentID) {
      setAgentPermissions({ data: [], loading: false, error: "" });
      return;
    }
    let cancelled = false;
    setAgentPermissions({ data: null, loading: true, error: "" });
    apiGet<AgentPermission[]>(`/agents/${selectedAgentID}/permissions`, token)
      .then((items) => {
        if (!cancelled) {
          setAgentPermissions({ data: items, loading: false, error: "" });
        }
      })
      .catch((error) => {
        if (!cancelled) {
          setAgentPermissions(errorState(error, []));
        }
      });
    return () => {
      cancelled = true;
    };
  }, [selectedAgentID, token, refreshTick]);

  useEffect(() => {
    if (!selectedSquadID) {
      setAccessGrants({ data: [], loading: false, error: "" });
      return;
    }
    let cancelled = false;
    setAccessGrants({ data: null, loading: true, error: "" });
    apiGet<AccessGrant[]>(`/squads/${selectedSquadID}/access-grants`, token)
      .then((items) => {
        if (!cancelled) {
          setAccessGrants({ data: items, loading: false, error: "" });
        }
      })
      .catch((error) => {
        if (!cancelled) {
          setAccessGrants(errorState(error, []));
        }
      });
    return () => {
      cancelled = true;
    };
  }, [selectedSquadID, token, refreshTick]);

  // Cockpit data is only rendered on the squad Overview tab, so fetch it only
  // while that tab is open instead of on every squad selection and refresh tick.
  useEffect(() => {
    if (!selectedSquadID || activeSection !== "squads" || squadTab !== "overview") {
      setSquadMetering({ data: null, loading: false, error: "" });
      setSquadAudit({ data: [], loading: false, error: "" });
      return;
    }
    let cancelled = false;
    setSquadMetering({ data: null, loading: true, error: "" });
    setSquadAudit({ data: null, loading: true, error: "" });
    Promise.allSettled([
      apiGet<MeteringSummary>(`/squads/${selectedSquadID}/metering`, token),
      apiGet<AuditEntry[]>(`/squads/${selectedSquadID}/audit`, token),
    ]).then(([meteringResult, auditResult]) => {
      if (cancelled) {
        return;
      }
      setSquadMetering(stateFromResult(meteringResult));
      setSquadAudit(stateFromResult(auditResult, []));
    });
    return () => {
      cancelled = true;
    };
  }, [selectedSquadID, activeSection, squadTab, token, refreshTick]);

  // Cost is one request per agent, so it runs only while the Agents tab is
  // open. A squad-level aggregate endpoint would remove the fan-out entirely.
  useEffect(() => {
    if (activeSection !== "squads" || squadTab !== "agents") {
      return;
    }
    const agentItems = agents.data || [];
    if (agentItems.length === 0) {
      setAgentCosts({});
      return;
    }
    let cancelled = false;
    Promise.allSettled(
      agentItems.map((agent) => apiGet<MeteringSummary>(`/agents/${agent.id}/metering`, token)),
    ).then((results) => {
      if (cancelled) {
        return;
      }
      const costs: Record<string, MeteringSummary> = {};
      results.forEach((result, index) => {
        if (result.status === "fulfilled" && result.value) {
          costs[agentItems[index].id] = result.value;
        }
      });
      setAgentCosts(costs);
    });
    return () => {
      cancelled = true;
    };
  }, [agents.data, activeSection, squadTab, token, refreshTick]);

  useEffect(() => {
    if (activeSection !== "admin") {
      return;
    }
    let cancelled = false;
    setAudit({ data: null, loading: true, error: "" });
    setMetering({ data: null, loading: true, error: "" });
    Promise.allSettled([
      apiGet<AuditEntry[]>("/audit", token),
      apiGet<MeteringSummary>("/metering/summary", token),
    ]).then(([auditResult, meteringResult]) => {
      if (cancelled) {
        return;
      }
      setAudit(stateFromResult(auditResult, []));
      setMetering(stateFromResult(meteringResult));
    });
    return () => {
      cancelled = true;
    };
  }, [activeSection, token, refreshTick]);

  const connected = user.data !== null && user.error === "";
  const isAdmin = user.data?.role === "platform_admin";
  const selectedSquad = (squads.data || []).find((squad) => squad.id === selectedSquadID) || null;
  const selectedAgent = (agents.data || []).find((agent) => agent.id === selectedAgentID) || null;
  const openTasks = selectedSquad && board.data
    ? (board.data.tasks || []).filter((task) => task.status !== "done").length
    : null;

  useEffect(() => {
    if (activeSection === "admin" && user.data !== null && !isAdmin) {
      setActiveSection("squads");
    }
  }, [activeSection, isAdmin, user.data]);

  function refresh(message = "") {
    setActionMessage(message);
    setRefreshTick((current) => current + 1);
  }

  function saveToken(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const next = draftToken.trim();
    if (next) {
      window.localStorage.setItem("skquad.authToken", next);
    } else {
      window.localStorage.removeItem("skquad.authToken");
    }
    setToken(next);
  }

  async function submitSquad(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    await runAction("Squad created", async () => {
      const created = await apiPost<Squad>("/squads", token, {
        name: newSquadForm.name,
        mission: newSquadForm.mission,
        operating_model: {},
      });
      setNewSquadForm({ name: "", mission: "" });
      setSelectedSquadID(created.id);
    });
  }

  async function updateSquadMission(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selectedSquad) {
      return;
    }
    await runAction("Squad updated", async () => {
      await apiPatch<Squad>(`/squads/${selectedSquad.id}`, token, {
        name: selectedSquad.name,
        mission: squadMissionDraft,
      });
    });
  }

  async function submitAgent(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selectedSquadID) {
      return;
    }
    await runAction("Agent created", async () => {
      const created = await apiPost<Agent>(`/squads/${selectedSquadID}/agents`, token, {
        name: agentForm.name,
        role: agentForm.role,
        default_model: agentForm.default_model,
        permissions: [],
        idle_timeout_sec: Number(agentForm.idle_timeout_sec) || 300,
      });
      setAgentForm({ name: "", role: "", default_model: "", idle_timeout_sec: "300" });
      setSelectedAgentID(created.id);
    });
  }

  async function createIdentity(agentID: string) {
    await runAction("Identity created", async () => {
      await apiPost<AgentIdentity>(`/agents/${agentID}/identity`, token, {});
    });
  }

  async function rotateIdentity(agentID: string) {
    await runAction("Identity rotated", async () => {
      await apiPost<AgentIdentity>(`/agents/${agentID}/identity/rotate`, token, {});
    });
  }

  async function submitTask(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selectedSquadID) {
      return;
    }
    await runAction("Task created", async () => {
      await apiPost<Task>(`/squads/${selectedSquadID}/board/tasks`, token, taskForm);
      setTaskForm({ title: "", description: "", assignee_agent_id: "" });
    });
  }

  async function moveTask(taskID: string, status: TaskStatus) {
    await runAction("Task moved", async () => {
      await apiPost<Task>(`/tasks/${taskID}/move`, token, { status });
    });
  }

  async function assignTask(taskID: string, assigneeAgentID: string) {
    await runAction("Task assignment updated", async () => {
      await apiPatch<Task>(`/tasks/${taskID}`, token, { assignee_agent_id: assigneeAgentID });
    });
  }

  async function deleteTask(taskID: string) {
    if (!window.confirm("Delete this task?")) {
      return;
    }
    await runAction("Task deleted", async () => {
      await apiDelete(`/tasks/${taskID}`, token);
    });
  }

  async function sendChat(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selectedAgentID || chatDraft.trim() === "") {
      return;
    }
    await runAction("Message queued", async () => {
      await apiPost<Message>(`/agents/${selectedAgentID}/chat`, token, {
        type: "consult",
        message: chatDraft,
      });
      setChatDraft("");
    });
  }

  async function submitProvider(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    await runAction("Provider registered", async () => {
      await apiPost<LLMProvider>("/registry/llm-providers", token, {
        name: providerForm.name,
        kind: providerForm.kind,
        base_url: providerForm.base_url,
        api_key_ref: providerForm.api_key_ref,
        default_model: providerForm.default_model,
        models: parseJSONList(providerForm.models),
        pricing: {},
      });
      setProviderForm({ name: "", kind: "openai", base_url: "", api_key_ref: "", default_model: "", models: "" });
    });
  }

  async function submitResource(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (registrySub === "llm-providers") {
      return;
    }
    // The visible subsection is the only source of truth for the resource type,
    // so the posted route always matches the form the user is looking at.
    const route = registrySub;
    await runAction("Resource registered", async () => {
      await apiPost<RegistryResource>(`/registry/${route}`, token, {
        name: resourceForm.name,
        description: resourceForm.description,
        endpoint: resourceForm.endpoint,
        auth_ref: resourceForm.auth_ref,
        manifest: parseJSONObject(resourceForm.manifest),
      });
      setResourceForm(emptyResourceForm);
    });
  }

  async function deprecateProvider(providerID: string) {
    await runAction("Provider deprecated", async () => {
      await apiPost<void>(`/registry/llm-providers/${providerID}/deprecate`, token, {});
    });
  }

  async function deprecateResource(resource: RegistryResource) {
    const route = registryTypes.find((item) => item.type === resource.type)?.path;
    if (!route) {
      return;
    }
    await runAction("Resource deprecated", async () => {
      await apiPost<void>(`/registry/${route}/${resource.id}/deprecate`, token, {});
    });
  }

  async function grantAgentPermission(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selectedAgentID || permissionForm.resource_id === "") {
      return;
    }
    await runAction("Agent permission updated", async () => {
      const current = agentPermissions.data || [];
      const next = [
        ...current.map((item) => ({ resource_type: item.resource_type, resource_id: item.resource_id })),
        { resource_type: permissionForm.resource_type, resource_id: permissionForm.resource_id },
      ];
      await apiPut<AgentPermission[]>(`/agents/${selectedAgentID}/permissions`, token, uniquePermissions(next));
      setPermissionForm({ ...permissionForm, resource_id: "" });
    });
  }

  async function revokeAgentPermission(permission: AgentPermission) {
    if (!selectedAgentID) {
      return;
    }
    await runAction("Agent permission revoked", async () => {
      const next = (agentPermissions.data || [])
        .filter((item) => item.id !== permission.id)
        .map((item) => ({ resource_type: item.resource_type, resource_id: item.resource_id }));
      await apiPut<AgentPermission[]>(`/agents/${selectedAgentID}/permissions`, token, next);
    });
  }

  async function submitAccessGrant(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selectedSquadID) {
      return;
    }
    await runAction("Access grant created", async () => {
      await apiPost<AccessGrant>(`/squads/${selectedSquadID}/access-grants`, token, grantForm);
      setGrantForm({ ...grantForm, grantee_id: "" });
    });
  }

  async function revokeAccessGrant(grantID: string) {
    await runAction("Access grant revoked", async () => {
      await apiDelete(`/access-grants/${grantID}`, token);
    });
  }

  async function runAction(success: string, action: () => Promise<void>) {
    setActionMessage("");
    try {
      await action();
      refresh(success);
    } catch (error) {
      const state = errorState(error);
      setActionMessage(state.error || "Request failed");
    }
  }

  return (
    <main className="app-shell">
      <TopNav
        user={user}
        connected={connected}
        mode={token ? "Bearer" : "Dev"}
        openTasks={openTasks}
        draftToken={draftToken}
        setDraftToken={setDraftToken}
        onSaveToken={saveToken}
      />

      <div className="app-body">
        <Sidebar
          activeSection={activeSection}
          onSelectSection={setActiveSection}
          registrySub={registrySub}
          onSelectRegistrySub={setRegistrySub}
          showAdmin={isAdmin}
        />

        <section className="workspace">
          <div className="section-head">
            <div>
              <p className="eyebrow">{apiBaseUrl()}</p>
              <h1>{sectionTitle(activeSection)}</h1>
            </div>
            <button className="secondary" type="button" onClick={() => refresh()}>
              Refresh
            </button>
          </div>

          <section className="content-band">
            {actionMessage && <div className={actionMessage.includes(":") ? "notice error compact" : "notice good compact"}>{actionMessage}</div>}

            {activeSection === "squads" && (
              <SquadsSection
                squads={squads}
                selectedSquadID={selectedSquadID}
                selectedSquad={selectedSquad}
                onSelectSquad={setSelectedSquadID}
                newSquadForm={newSquadForm}
                setNewSquadForm={setNewSquadForm}
                onCreateSquad={submitSquad}
                missionDraft={squadMissionDraft}
                setMissionDraft={setSquadMissionDraft}
                onUpdateMission={updateSquadMission}
                squadTab={squadTab}
                setSquadTab={setSquadTab}
                agents={agents}
                selectedAgentID={selectedAgentID}
                onSelectAgent={setSelectedAgentID}
                agentForm={agentForm}
                setAgentForm={setAgentForm}
                onCreateAgent={submitAgent}
                onCreateIdentity={createIdentity}
                onRotateIdentity={rotateIdentity}
                chat={chat}
                chatDraft={chatDraft}
                setChatDraft={setChatDraft}
                onSendChat={sendChat}
                permissions={agentPermissions}
                permissionForm={permissionForm}
                setPermissionForm={setPermissionForm}
                providers={providers}
                resources={resources}
                onGrantPermission={grantAgentPermission}
                onRevokePermission={revokeAgentPermission}
                board={board}
                taskForm={taskForm}
                setTaskForm={setTaskForm}
                onCreateTask={submitTask}
                onMoveTask={moveTask}
                onAssignTask={assignTask}
                onDeleteTask={deleteTask}
                accessGrants={accessGrants}
                grantForm={grantForm}
                setGrantForm={setGrantForm}
                onCreateGrant={submitAccessGrant}
                onRevokeGrant={revokeAccessGrant}
                squadMetering={squadMetering}
                squadAudit={squadAudit}
                agentCosts={agentCosts}
              />
            )}

            {activeSection === "registry" && (
              <RegistrySection
                registrySub={registrySub}
                providers={providers}
                resources={resources}
                providerForm={providerForm}
                setProviderForm={setProviderForm}
                resourceForm={resourceForm}
                setResourceForm={setResourceForm}
                onCreateProvider={submitProvider}
                onCreateResource={submitResource}
                onDeprecateProvider={deprecateProvider}
                onDeprecateResource={deprecateResource}
              />
            )}

            {activeSection === "admin" && isAdmin && (
              <AdminSection
                user={user}
                selectedSquad={selectedSquad}
                selectedAgent={selectedAgent}
                metering={metering}
                audit={audit}
              />
            )}
          </section>
        </section>
      </div>
    </main>
  );
}

function stateFromResult<T>(result: PromiseSettledResult<T>, fallback: T | null = null): ApiState<T> {
  if (result.status === "fulfilled") {
    return { data: result.value, loading: false, error: "" };
  }
  return errorState(result.reason, fallback);
}

function errorState<T>(reason: unknown, fallback: T | null = null): ApiState<T> {
  if (reason instanceof ApiError) {
    return { data: fallback, loading: false, error: `${reason.status}: ${reason.message}` };
  }
  return { data: fallback, loading: false, error: reason instanceof Error ? reason.message : "Request failed" };
}

function sectionTitle(section: Section) {
  return {
    squads: "Squads",
    registry: "Registry",
    admin: "Admin",
  }[section];
}

function parseJSONList(value: string): unknown[] {
  const trimmed = value.trim();
  if (!trimmed) {
    return [];
  }
  const parsed = JSON.parse(trimmed);
  return Array.isArray(parsed) ? parsed : [];
}

function parseJSONObject(value: string): Record<string, unknown> {
  const trimmed = value.trim();
  if (!trimmed) {
    return {};
  }
  const parsed = JSON.parse(trimmed);
  return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed as Record<string, unknown> : {};
}

function uniquePermissions(items: Array<{ resource_type: ResourceType; resource_id: string }>) {
  const seen = new Set<string>();
  return items.filter((item) => {
    const key = `${item.resource_type}:${item.resource_id}`;
    if (seen.has(key)) {
      return false;
    }
    seen.add(key);
    return true;
  });
}
