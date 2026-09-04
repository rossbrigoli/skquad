"use client";

import { FormEvent, useEffect, useState } from "react";
import {
  AccessGrant,
  Agent,
  AgentPermission,
  ApiState,
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
} from "../lib/api";
import {
  StateNotice,
  formatCost,
  formatRelativeTime,
  leaseState,
  messageDeliveryNote,
  messageText,
  registryTypes,
  resourceLabel,
  taskStatuses,
} from "./shared";
import { SquadCockpit } from "./SquadCockpit";

export type SquadTab = "overview" | "agents" | "tasks" | "grants";

const squadTabs: Array<{ id: SquadTab; label: string }> = [
  { id: "overview", label: "Overview" },
  { id: "agents", label: "Agents" },
  { id: "tasks", label: "Tasks" },
  { id: "grants", label: "Access Grants" },
];

export function SquadsSection({
  squads,
  selectedSquadID,
  selectedSquad,
  onSelectSquad,
  newSquadForm,
  setNewSquadForm,
  onCreateSquad,
  missionDraft,
  setMissionDraft,
  onUpdateMission,
  squadTab,
  setSquadTab,
  agents,
  selectedAgentID,
  onSelectAgent,
  agentForm,
  setAgentForm,
  onCreateAgent,
  onCreateIdentity,
  onRotateIdentity,
  chat,
  chatDraft,
  setChatDraft,
  onSendChat,
  permissions,
  permissionForm,
  setPermissionForm,
  providers,
  resources,
  onGrantPermission,
  onRevokePermission,
  board,
  taskForm,
  setTaskForm,
  onCreateTask,
  onMoveTask,
  onAssignTask,
  onDeleteTask,
  accessGrants,
  grantForm,
  setGrantForm,
  onCreateGrant,
  onRevokeGrant,
  squadMetering,
  squadAudit,
  agentCosts,
}: {
  squads: ApiState<Squad[]>;
  selectedSquadID: string;
  selectedSquad: Squad | null;
  onSelectSquad: (id: string) => void;
  newSquadForm: { name: string; mission: string };
  setNewSquadForm: (form: { name: string; mission: string }) => void;
  onCreateSquad: (event: FormEvent<HTMLFormElement>) => void;
  missionDraft: string;
  setMissionDraft: (value: string) => void;
  onUpdateMission: (event: FormEvent<HTMLFormElement>) => void;
  squadTab: SquadTab;
  setSquadTab: (tab: SquadTab) => void;
  agents: ApiState<Agent[]>;
  selectedAgentID: string;
  onSelectAgent: (id: string) => void;
  agentForm: { name: string; role: string; system_prompt: string; default_model: string; idle_timeout_sec: string };
  setAgentForm: (form: { name: string; role: string; system_prompt: string; default_model: string; idle_timeout_sec: string }) => void;
  onCreateAgent: (event: FormEvent<HTMLFormElement>) => void;
  onCreateIdentity: (id: string) => void;
  onRotateIdentity: (id: string) => void;
  chat: ApiState<Message[]>;
  chatDraft: string;
  setChatDraft: (value: string) => void;
  onSendChat: (event: FormEvent<HTMLFormElement>) => void;
  permissions: ApiState<AgentPermission[]>;
  permissionForm: { resource_type: ResourceType; resource_id: string };
  setPermissionForm: (form: { resource_type: ResourceType; resource_id: string }) => void;
  providers: ApiState<LLMProvider[]>;
  resources: ApiState<RegistryResource[]>;
  onGrantPermission: (event: FormEvent<HTMLFormElement>) => void;
  onRevokePermission: (permission: AgentPermission) => void;
  board: ApiState<BoardPayload>;
  taskForm: { title: string; description: string; assignee_agent_id: string };
  setTaskForm: (form: { title: string; description: string; assignee_agent_id: string }) => void;
  onCreateTask: (event: FormEvent<HTMLFormElement>) => void;
  onMoveTask: (taskID: string, status: TaskStatus) => void;
  onAssignTask: (taskID: string, assigneeAgentID: string) => void;
  onDeleteTask: (taskID: string) => void;
  accessGrants: ApiState<AccessGrant[]>;
  grantForm: { grantee_type: "user" | "agent"; grantee_id: string; permissions: string };
  setGrantForm: (form: { grantee_type: "user" | "agent"; grantee_id: string; permissions: string }) => void;
  onCreateGrant: (event: FormEvent<HTMLFormElement>) => void;
  onRevokeGrant: (id: string) => void;
  squadMetering: ApiState<MeteringSummary>;
  squadAudit: ApiState<AuditEntry[]>;
  agentCosts: Record<string, MeteringSummary>;
}) {
  const squadItems = squads.data || [];
  return (
    <>
      <div className="workflow-grid">
        <form className="form-panel" onSubmit={onCreateSquad}>
          <h3>Create Squad</h3>
          <label>
            Name
            <input value={newSquadForm.name} onChange={(event) => setNewSquadForm({ ...newSquadForm, name: event.target.value })} required />
          </label>
          <label>
            Mission
            <textarea value={newSquadForm.mission} onChange={(event) => setNewSquadForm({ ...newSquadForm, mission: event.target.value })} rows={4} />
          </label>
          <button type="submit">Create</button>
        </form>

        <div className="span-2">
          <StateNotice state={squads} empty="No squads yet" />
          {squadItems.length > 0 && (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Status</th>
                    <th>Namespace</th>
                    <th>Mission</th>
                  </tr>
                </thead>
                <tbody>
                  {squadItems.map((squad) => (
                    <tr
                      key={squad.id}
                      className={squad.id === selectedSquadID ? "selected-row" : ""}
                      onClick={() => onSelectSquad(squad.id)}
                    >
                      <td>
                        <strong>{squad.name}</strong>
                        <small>{squad.id}</small>
                      </td>
                      <td>{squad.status || "active"}</td>
                      <td>{squad.namespace || "-"}</td>
                      <td>{squad.mission || "-"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>

      {selectedSquad && (
        <>
          <div className="tab-row" role="tablist" aria-label="Squad detail">
            {squadTabs.map((tab) => (
              <button
                key={tab.id}
                type="button"
                role="tab"
                aria-selected={tab.id === squadTab}
                className={tab.id === squadTab ? "tab active" : "tab"}
                onClick={() => setSquadTab(tab.id)}
              >
                {tab.label}
              </button>
            ))}
          </div>

          {squadTab === "overview" && (
            <>
              <SquadCockpit agents={agents} board={board} metering={squadMetering} audit={squadAudit} />
              <form className="form-panel span-3" style={{ marginTop: 16 }} onSubmit={onUpdateMission}>
                <h3>{selectedSquad.name}</h3>
                <div className="key-grid">
                  <span>ID</span>
                  <strong>{selectedSquad.id}</strong>
                  <span>Owner</span>
                  <strong>{selectedSquad.owner_id || "-"}</strong>
                  <span>Namespace</span>
                  <strong>{selectedSquad.namespace || "-"}</strong>
                  <span>Status</span>
                  <strong>{selectedSquad.status || "active"}</strong>
                </div>
                <label>
                  Mission
                  <textarea value={missionDraft} onChange={(event) => setMissionDraft(event.target.value)} rows={3} />
                </label>
                <button type="submit">Save Mission</button>
              </form>
            </>
          )}

          {squadTab === "agents" && (
            <AgentsTab
              agents={agents}
              selectedAgentID={selectedAgentID}
              onSelectAgent={onSelectAgent}
              agentForm={agentForm}
              setAgentForm={setAgentForm}
              onCreateAgent={onCreateAgent}
              onCreateIdentity={onCreateIdentity}
              onRotateIdentity={onRotateIdentity}
              chat={chat}
              chatDraft={chatDraft}
              setChatDraft={setChatDraft}
              onSendChat={onSendChat}
              permissions={permissions}
              permissionForm={permissionForm}
              setPermissionForm={setPermissionForm}
              providers={providers}
              resources={resources}
              onGrantPermission={onGrantPermission}
              onRevokePermission={onRevokePermission}
              agentCosts={agentCosts}
            />
          )}

          {squadTab === "tasks" && (
            <TasksTab
              board={board}
              agents={agents.data || []}
              squadID={selectedSquad.id}
              taskForm={taskForm}
              setTaskForm={setTaskForm}
              onCreateTask={onCreateTask}
              onMoveTask={onMoveTask}
              onAssignTask={onAssignTask}
              onDeleteTask={onDeleteTask}
            />
          )}

          {squadTab === "grants" && (
            <div className="workflow-grid">
              <form className="form-panel" onSubmit={onCreateGrant}>
                <h3>Create Access Grant</h3>
                <label>
                  Grantee type
                  <select value={grantForm.grantee_type} onChange={(event) => setGrantForm({ ...grantForm, grantee_type: event.target.value as "user" | "agent" })}>
                    <option value="user">User</option>
                    <option value="agent">Agent</option>
                  </select>
                </label>
                <label>
                  Grantee ID
                  <input value={grantForm.grantee_id} onChange={(event) => setGrantForm({ ...grantForm, grantee_id: event.target.value })} required />
                </label>
                <label>
                  Permissions
                  <input value={grantForm.permissions} onChange={(event) => setGrantForm({ ...grantForm, permissions: event.target.value })} />
                </label>
                <button type="submit">Create Grant</button>
              </form>

              <div className="span-2">
                <h3 className="panel-title">Squad Access Grants</h3>
                <StateNotice state={accessGrants} empty="No access grants" />
                <div className="stack-list">
                  {(accessGrants.data || []).map((grant) => (
                    <article className="message-item" key={grant.id}>
                      <strong>{grant.grantee_type}: {grant.grantee_id}</strong>
                      <span>{grant.permissions || "talk"}</span>
                      <button type="button" className="secondary small" onClick={() => onRevokeGrant(grant.id)}>
                        Revoke
                      </button>
                    </article>
                  ))}
                </div>
              </div>
            </div>
          )}
        </>
      )}
    </>
  );
}

function AgentsTab({
  agents,
  selectedAgentID,
  onSelectAgent,
  agentForm,
  setAgentForm,
  onCreateAgent,
  onCreateIdentity,
  onRotateIdentity,
  chat,
  chatDraft,
  setChatDraft,
  onSendChat,
  permissions,
  permissionForm,
  setPermissionForm,
  providers,
  resources,
  onGrantPermission,
  onRevokePermission,
  agentCosts,
}: {
  agents: ApiState<Agent[]>;
  selectedAgentID: string;
  onSelectAgent: (id: string) => void;
  agentForm: { name: string; role: string; system_prompt: string; default_model: string; idle_timeout_sec: string };
  setAgentForm: (form: { name: string; role: string; system_prompt: string; default_model: string; idle_timeout_sec: string }) => void;
  onCreateAgent: (event: FormEvent<HTMLFormElement>) => void;
  onCreateIdentity: (id: string) => void;
  onRotateIdentity: (id: string) => void;
  chat: ApiState<Message[]>;
  chatDraft: string;
  setChatDraft: (value: string) => void;
  onSendChat: (event: FormEvent<HTMLFormElement>) => void;
  permissions: ApiState<AgentPermission[]>;
  permissionForm: { resource_type: ResourceType; resource_id: string };
  setPermissionForm: (form: { resource_type: ResourceType; resource_id: string }) => void;
  providers: ApiState<LLMProvider[]>;
  resources: ApiState<RegistryResource[]>;
  onGrantPermission: (event: FormEvent<HTMLFormElement>) => void;
  onRevokePermission: (permission: AgentPermission) => void;
  agentCosts: Record<string, MeteringSummary>;
}) {
  const agentItems = agents.data || [];
  const selectedAgent = agentItems.find((agent) => agent.id === selectedAgentID) || null;
  const providerItems = providers.data || [];
  const resourceItems = resources.data || [];
  const grantableResources = [
    ...providerItems.map((provider) => ({ type: "llm_provider" as ResourceType, id: provider.id, name: provider.name })),
    ...resourceItems.map((resource) => ({ type: resource.type, id: resource.id, name: resource.name })),
  ].filter((item) => item.type === permissionForm.resource_type);

  return (
    <div className="workflow-grid">
      <form className="form-panel" onSubmit={onCreateAgent}>
        <h3>Add Agent</h3>
        <label>
          Name
          <input value={agentForm.name} onChange={(event) => setAgentForm({ ...agentForm, name: event.target.value })} required />
        </label>
        <label>
          Role
          <textarea value={agentForm.role} onChange={(event) => setAgentForm({ ...agentForm, role: event.target.value })} rows={3} />
        </label>
        <label>
          System Prompt
          <textarea
            value={agentForm.system_prompt}
            onChange={(event) => setAgentForm({ ...agentForm, system_prompt: event.target.value })}
            rows={3}
            placeholder="Optional persona/instructions for this agent's chat and tasks"
          />
        </label>
        <label>
          Default model
          <input value={agentForm.default_model} onChange={(event) => setAgentForm({ ...agentForm, default_model: event.target.value })} placeholder="provider/model" />
        </label>
        <label>
          Idle timeout seconds
          <input
            type="number"
            min="1"
            value={agentForm.idle_timeout_sec}
            onChange={(event) => setAgentForm({ ...agentForm, idle_timeout_sec: event.target.value })}
          />
        </label>
        <button type="submit">Add Agent</button>
      </form>

      <div className="span-2">
        <StateNotice state={agents} empty="No agents in this squad" />
        {agentItems.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Agent</th>
                  <th>Status</th>
                  <th>Model</th>
                  <th>Cost</th>
                  <th>Identity</th>
                </tr>
              </thead>
              <tbody>
                {agentItems.map((agent) => (
                  <tr key={agent.id} className={agent.id === selectedAgentID ? "selected-row" : ""} onClick={() => onSelectAgent(agent.id)}>
                    <td>
                      <strong>{agent.name}</strong>
                      <small>{agent.role || agent.id}</small>
                    </td>
                    <td>
                      <span className={`agent-state ${agentStateClass(agent.status)}`}>{agent.status || "idle"}</span>
                    </td>
                    <td>{agent.default_model || agent.default_provider_id || "-"}</td>
                    <td className="numeric">{agentCosts[agent.id] ? formatCost(agentCosts[agent.id]) : "-"}</td>
                    <td>
                      <div className="button-row">
                        <button type="button" className="secondary small" onClick={() => onCreateIdentity(agent.id)}>
                          Create
                        </button>
                        <button type="button" className="secondary small" onClick={() => onRotateIdentity(agent.id)}>
                          Rotate
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="form-panel">
        <h3>Grant Resource Access</h3>
        {selectedAgent ? (
          <form className="nested-form" onSubmit={onGrantPermission}>
            <label>
              Agent
              <input value={selectedAgent.name} readOnly />
            </label>
            <label>
              Resource type
              <select value={permissionForm.resource_type} onChange={(event) => setPermissionForm({ resource_type: event.target.value as ResourceType, resource_id: "" })}>
                <option value="llm_provider">LLM Provider</option>
                {registryTypes.map((item) => (
                  <option key={item.type} value={item.type}>
                    {item.label}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Resource
              <select value={permissionForm.resource_id} onChange={(event) => setPermissionForm({ ...permissionForm, resource_id: event.target.value })}>
                <option value="">Select resource</option>
                {grantableResources.map((resource) => (
                  <option key={`${resource.type}:${resource.id}`} value={resource.id}>
                    {resource.name}
                  </option>
                ))}
              </select>
            </label>
            <button type="submit">Grant</button>
          </form>
        ) : (
          <div className="notice compact">Select an agent before granting resources</div>
        )}
      </div>

      <div>
        <h3 className="panel-title">Agent Permissions</h3>
        <StateNotice state={permissions} empty="No resources granted" />
        <div className="stack-list">
          {(permissions.data || []).map((permission) => (
            <article className="message-item" key={permission.id}>
              <strong>{permission.resource_type}</strong>
              <span>{resourceLabel(permission, providerItems, resourceItems)}</span>
              <button type="button" className="secondary small" onClick={() => onRevokePermission(permission)}>
                Revoke
              </button>
            </article>
          ))}
        </div>
      </div>

      <form className="form-panel" onSubmit={onSendChat}>
        <h3>Agent Chat</h3>
        {selectedAgent ? (
          <>
            <div className="message-list">
              <StateNotice state={chat} empty="No chat messages yet" />
              {(chat.data || []).map((message) => {
                const note = messageDeliveryNote(message);
                return (
                  <article key={message.id} className="message-item">
                    <strong>{message.from_type}</strong>
                    <span>{messageText(message)}</span>
                    <small>
                      <span className={`delivery ${deliveryClass(message.status)}`}>{message.status}</span> · {message.type}
                      {note && ` · ${note}`}
                    </small>
                    {message.terminal_reason && <small className="delivery-reason">{message.terminal_reason}</small>}
                  </article>
                );
              })}
            </div>
            <label>
              Message to {selectedAgent.name}
              <textarea value={chatDraft} onChange={(event) => setChatDraft(event.target.value)} rows={3} />
            </label>
            <button type="submit">Send</button>
          </>
        ) : (
          <div className="notice compact">Select an agent to view or send messages</div>
        )}
      </form>
    </div>
  );
}

const assigneeFilterKey = "skquad.board.assigneeFilter";

function TasksTab({
  board,
  agents,
  squadID,
  taskForm,
  setTaskForm,
  onCreateTask,
  onMoveTask,
  onAssignTask,
  onDeleteTask,
}: {
  board: ApiState<BoardPayload>;
  agents: Agent[];
  squadID: string;
  taskForm: { title: string; description: string; assignee_agent_id: string };
  setTaskForm: (form: { title: string; description: string; assignee_agent_id: string }) => void;
  onCreateTask: (event: FormEvent<HTMLFormElement>) => void;
  onMoveTask: (taskID: string, status: TaskStatus) => void;
  onAssignTask: (taskID: string, assigneeAgentID: string) => void;
  onDeleteTask: (taskID: string) => void;
}) {
  // "" = everyone, "__unassigned" = no agent assigned, otherwise an agent id.
  const [assigneeFilter, setAssigneeFilter] = useState("");
  const [activeOnly, setActiveOnly] = useState(false);

  useEffect(() => {
    setAssigneeFilter(window.localStorage.getItem(`${assigneeFilterKey}:${squadID}`) || "");
  }, [squadID]);

  // A filter pointing at an agent that has since been removed would hide every
  // card with no visible reason. Only second-guess it once a non-empty agent
  // list has arrived, otherwise the first render (agents still loading) would
  // wipe a perfectly valid filter.
  useEffect(() => {
    if (!assigneeFilter || assigneeFilter === "__unassigned" || agents.length === 0) {
      return;
    }
    if (agents.some((agent) => agent.id === assigneeFilter)) {
      return;
    }
    setAssigneeFilter("");
    window.localStorage.removeItem(`${assigneeFilterKey}:${squadID}`);
  }, [agents, assigneeFilter, squadID]);

  function changeAssigneeFilter(value: string) {
    setAssigneeFilter(value);
    if (value) {
      window.localStorage.setItem(`${assigneeFilterKey}:${squadID}`, value);
    } else {
      window.localStorage.removeItem(`${assigneeFilterKey}:${squadID}`);
    }
  }

  const allTasks = board.data?.tasks || [];
  const tasks = allTasks.filter((task) => {
    if (assigneeFilter === "__unassigned" && task.assignee_agent_id) {
      return false;
    }
    if (assigneeFilter && assigneeFilter !== "__unassigned" && task.assignee_agent_id !== assigneeFilter) {
      return false;
    }
    if (activeOnly && leaseState(task) === "idle") {
      return false;
    }
    return true;
  });
  const runningCount = allTasks.filter((task) => leaseState(task) !== "idle").length;

  return (
    <div className="workflow-grid">
      <form className="form-panel" onSubmit={onCreateTask}>
        <h3>Create Task</h3>
        <label>
          Title
          <input value={taskForm.title} onChange={(event) => setTaskForm({ ...taskForm, title: event.target.value })} required />
        </label>
        <label>
          Description
          <textarea value={taskForm.description} onChange={(event) => setTaskForm({ ...taskForm, description: event.target.value })} rows={4} />
        </label>
        <label>
          Assignee
          <select value={taskForm.assignee_agent_id} onChange={(event) => setTaskForm({ ...taskForm, assignee_agent_id: event.target.value })}>
            <option value="">Unassigned</option>
            {agents.map((agent) => (
              <option key={agent.id} value={agent.id}>
                {agent.name}
              </option>
            ))}
          </select>
        </label>
        <button type="submit">Create Task</button>
      </form>

      <div className="span-2">
        <div className="filter-bar">
          <label>
            Assignee
            <select value={assigneeFilter} onChange={(event) => changeAssigneeFilter(event.target.value)}>
              <option value="">Everyone</option>
              <option value="__unassigned">Unassigned</option>
              {agents.map((agent) => (
                <option key={agent.id} value={agent.id}>
                  {agent.name}
                </option>
              ))}
            </select>
          </label>
          <button
            type="button"
            className={activeOnly ? "filter-toggle active" : "filter-toggle"}
            onClick={() => setActiveOnly((current) => !current)}
            aria-pressed={activeOnly}
          >
            In flight only{runningCount > 0 ? ` · ${runningCount}` : ""}
          </button>
          <span className="filter-count">
            {tasks.length === allTasks.length
              ? `${allTasks.length} tasks`
              : `${tasks.length} of ${allTasks.length} tasks`}
          </span>
        </div>

        <StateNotice state={board} empty="No tasks yet" />
        {allTasks.length > 0 && tasks.length === 0 && (
          <div className="notice compact">No tasks match the current filter</div>
        )}
        {tasks.length > 0 && (
          <div className="board-grid">
            {taskStatuses.map((status) => (
              <section className="task-column" key={status}>
                <h3>{status}</h3>
                {tasks.filter((task) => task.status === status).map((task) => (
                  <article className="task-card" key={task.id}>
                    <TaskExecutionBadge task={task} agents={agents} />
                    <strong>{task.title}</strong>
                    <p>{task.description || "-"}</p>
                    {task.created_by_type === "agent" && (
                      <span className="provenance">Created by agent {agentName(agents, task.created_by_id)}</span>
                    )}
                    <label>
                      Status
                      <select value={task.status} onChange={(event) => onMoveTask(task.id, event.target.value as TaskStatus)}>
                        {taskStatuses.map((item) => (
                          <option key={item} value={item}>
                            {item}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      Assignee
                      <select value={task.assignee_agent_id || ""} onChange={(event) => onAssignTask(task.id, event.target.value)}>
                        <option value="">Unassigned</option>
                        {agents.map((agent) => (
                          <option key={agent.id} value={agent.id}>
                            {agent.name}
                          </option>
                        ))}
                      </select>
                    </label>
                    <button type="button" className="secondary small" onClick={() => onDeleteTask(task.id)}>
                      Delete
                    </button>
                  </article>
                ))}
              </section>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// A task holds a lease only while an agent runtime is actively working it, so
// an unexpired lease is the one reliable signal that work is happening now.
function TaskExecutionBadge({ task, agents }: { task: Task; agents: Agent[] }) {
  const state = leaseState(task);
  if (state === "idle") {
    return null;
  }
  const worker = workerLabel(agents, task.worker_id);
  const expiry = formatRelativeTime(task.lease_expires_at);
  return (
    <span className="exec-line">
      {state === "running" ? (
        <span className="exec-badge running">
          <span className="exec-dot" />
          Running
        </span>
      ) : (
        <span className="exec-badge stalled" title={`Lease expired ${expiry}`}>
          Stalled
        </span>
      )}
      {worker && <small className="exec-worker" title={worker.detail}>{worker.name}</small>}
    </span>
  );
}

// A runtime names its worker "<agent id>:<uuid>", so the prefix resolves to an
// agent while the suffix only tells two processes of the same agent apart.
// Printing the raw id on the card would be unreadable noise.
function workerLabel(agents: Agent[], workerID?: string): { name: string; detail: string } | null {
  if (!workerID) {
    return null;
  }
  const [agentID, instance] = workerID.split(":");
  const name = agentName(agents, agentID);
  return {
    name: name || "worker",
    detail: instance ? `worker ${instance}` : workerID,
  };
}

function agentName(agents: Agent[], id?: string): string {
  if (!id) {
    return "";
  }
  return agents.find((agent) => agent.id === id)?.name || id;
}

function agentStateClass(status?: string): string {
  if (status === "error" || status === "failed") {
    return "error";
  }
  if (status === "busy") {
    return "busy";
  }
  if (status === "paused") {
    return "paused";
  }
  return "idle";
}

function deliveryClass(status: string): string {
  if (status === "failed" || status === "dead_letter" || status === "expired") {
    return "bad";
  }
  if (status === "pending" || status === "retrying") {
    return "warn";
  }
  return "ok";
}
