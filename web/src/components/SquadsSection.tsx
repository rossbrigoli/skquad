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
  SquadLLM,
  SquadLLMStatus,
  StateNotice,
  agentUsesSquadLLM,
  countLabel,
  formatCost,
  formatRelativeTime,
  leaseState,
  messageDeliveryNote,
  messageText,
  pickModel,
  providerModels,
  registryTypes,
  resolveSquadLLM,
  resourceLabel,
  taskStatuses,
} from "./shared";
import { ListHeader, Modal, useModalForm } from "./Modal";
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
  onDeleteSquad,
  newSquadForm,
  setNewSquadForm,
  onCreateSquad,
  missionDraft,
  setMissionDraft,
  squadLLMDraft,
  setSquadLLMDraft,
  onUpdateSettings,
  onApplySquadLLM,
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
  onDeleteAgent,
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
  onDeleteSquad: (id: string) => void;
  newSquadForm: { name: string; mission: string; provider_id: string; model: string };
  setNewSquadForm: (form: { name: string; mission: string; provider_id: string; model: string }) => void;
  onCreateSquad: () => Promise<string | null>;
  missionDraft: string;
  setMissionDraft: (value: string) => void;
  squadLLMDraft: SquadLLM;
  setSquadLLMDraft: (llm: SquadLLM) => void;
  onUpdateSettings: (event: FormEvent<HTMLFormElement>) => void;
  onApplySquadLLM: (agentID: string) => void;
  squadTab: SquadTab;
  setSquadTab: (tab: SquadTab) => void;
  agents: ApiState<Agent[]>;
  selectedAgentID: string;
  onSelectAgent: (id: string) => void;
  agentForm: { name: string; role: string; system_prompt: string; default_model: string; idle_timeout_sec: string };
  setAgentForm: (form: { name: string; role: string; system_prompt: string; default_model: string; idle_timeout_sec: string }) => void;
  onCreateAgent: () => Promise<string | null>;
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
  onGrantPermission: () => Promise<string | null>;
  onRevokePermission: (permission: AgentPermission) => void;
  board: ApiState<BoardPayload>;
  taskForm: { title: string; description: string; assignee_agent_id: string };
  setTaskForm: (form: { title: string; description: string; assignee_agent_id: string }) => void;
  onCreateTask: () => Promise<string | null>;
  onMoveTask: (taskID: string, status: TaskStatus) => void;
  onAssignTask: (taskID: string, assigneeAgentID: string) => void;
  onDeleteTask: (taskID: string) => void;
  onDeleteAgent: (id: string) => void;
  accessGrants: ApiState<AccessGrant[]>;
  grantForm: { grantee_type: "user" | "agent"; grantee_id: string; permissions: string };
  setGrantForm: (form: { grantee_type: "user" | "agent"; grantee_id: string; permissions: string }) => void;
  onCreateGrant: () => Promise<string | null>;
  onRevokeGrant: (id: string) => void;
  squadMetering: ApiState<MeteringSummary>;
  squadAudit: ApiState<AuditEntry[]>;
  agentCosts: Record<string, MeteringSummary>;
}) {
  const squadItems = squads.data || [];
  const providerItems = providers.data || [];
  const llmStatus = resolveSquadLLM(selectedSquad, providerItems);
  const squadModal = useModalForm(onCreateSquad);
  const grantModal = useModalForm(onCreateGrant);
  return (
    <>
      <div className="section-stack">
        <ListHeader title="Squads" detail={countLabel(squadItems.length, "squad")}>
          <button type="button" className="primary" onClick={squadModal.show}>
            + New squad
          </button>
        </ListHeader>
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
                  <th>Actions</th>
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
                    <td>
                      <button
                        type="button"
                        className="secondary small danger"
                        onClick={(event) => {
                          event.stopPropagation();
                          onDeleteSquad(squad.id);
                        }}
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <Modal {...squadModal.props} title="New squad" submitLabel="Create squad">
        <label>
          Name
          <input value={newSquadForm.name} onChange={(event) => setNewSquadForm({ ...newSquadForm, name: event.target.value })} required />
        </label>
        <label>
          Mission
          <textarea value={newSquadForm.mission} onChange={(event) => setNewSquadForm({ ...newSquadForm, mission: event.target.value })} rows={4} />
        </label>
        <LLMPicker
          providers={providerItems}
          loading={providers.loading}
          value={{ provider_id: newSquadForm.provider_id, model: newSquadForm.model }}
          onChange={(llm) => setNewSquadForm({ ...newSquadForm, provider_id: llm.provider_id, model: llm.model })}
          required
        />
      </Modal>

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
              <form className="form-panel span-3" style={{ marginTop: 16 }} onSubmit={onUpdateSettings}>
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
                <LLMPicker
                  providers={providerItems}
                  loading={providers.loading}
                  value={squadLLMDraft}
                  onChange={setSquadLLMDraft}
                />
                <small className="field-note">
                  Agents added from now on use this LLM. Existing agents keep theirs until you choose Apply squad LLM on the Agents tab.
                </small>
                <button type="submit">Save settings</button>
              </form>
            </>
          )}

          {squadTab === "agents" && (
            <AgentsTab
              agents={agents}
              selectedAgentID={selectedAgentID}
              onSelectAgent={onSelectAgent}
              onDeleteAgent={onDeleteAgent}
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
              llmStatus={llmStatus}
              providersLoading={providers.loading}
              onApplySquadLLM={onApplySquadLLM}
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
            <div className="section-stack">
              <ListHeader title="Access Grants" detail="Other users, or agents in other squads, allowed to talk to this squad's agents">
                <button type="button" className="primary" onClick={grantModal.show}>
                  + New grant
                </button>
              </ListHeader>
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

              <Modal {...grantModal.props} title="New access grant" submitLabel="Create grant">
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
              </Modal>
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
  onDeleteAgent,
  agentCosts,
  llmStatus,
  providersLoading,
  onApplySquadLLM,
}: {
  agents: ApiState<Agent[]>;
  selectedAgentID: string;
  onSelectAgent: (id: string) => void;
  agentForm: { name: string; role: string; system_prompt: string; default_model: string; idle_timeout_sec: string };
  setAgentForm: (form: { name: string; role: string; system_prompt: string; default_model: string; idle_timeout_sec: string }) => void;
  onCreateAgent: () => Promise<string | null>;
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
  onGrantPermission: () => Promise<string | null>;
  onRevokePermission: (permission: AgentPermission) => void;
  onDeleteAgent: (id: string) => void;
  agentCosts: Record<string, MeteringSummary>;
  llmStatus: SquadLLMStatus;
  providersLoading: boolean;
  onApplySquadLLM: (agentID: string) => void;
}) {
  const agentItems = agents.data || [];
  const selectedAgent = agentItems.find((agent) => agent.id === selectedAgentID) || null;
  const providerItems = providers.data || [];
  const resourceItems = resources.data || [];
  // LLM access comes from the squad, so providers are not offered as grants.
  const grantableResources = resourceItems
    .filter((resource) => resource.type === permissionForm.resource_type)
    .map((resource) => ({ type: resource.type, id: resource.id, name: resource.name }));
  const llmReady = llmStatus.state === "ready";
  const llmBlocked = !llmReady && providersLoading ? "Loading LLM providers" : llmBlockedMessage(llmStatus);
  const agentModal = useModalForm(onCreateAgent);
  const accessModal = useModalForm(onGrantPermission);

  return (
    <div className="section-stack">
      <ListHeader title="Agents" detail={countLabel(agentItems.length, "agent")}>
        <button type="button" className="primary" onClick={agentModal.show} disabled={!llmReady}>
          + New agent
        </button>
      </ListHeader>
      {llmBlocked && <div className="notice warn compact">{llmBlocked}</div>}
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
                <th>Actions</th>
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
                  <td>
                    <button
                      type="button"
                      className="secondary small danger"
                      onClick={(event) => {
                        event.stopPropagation();
                        onDeleteAgent(agent.id);
                      }}
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="split-grid">
        <section>
          <ListHeader
            title="Permissions"
            detail={selectedAgent ? `Resources ${selectedAgent.name} may use` : "Select an agent to manage its permissions"}
          >
            <button type="button" className="secondary" onClick={accessModal.show} disabled={!selectedAgent}>
              + Grant access
            </button>
          </ListHeader>
          {selectedAgent && llmReady && !permissions.loading && (
            <AgentLLMStatus
              agent={selectedAgent}
              permissions={permissions.data || []}
              status={llmStatus}
              onApply={() => onApplySquadLLM(selectedAgent.id)}
            />
          )}
          <StateNotice state={permissions} empty="No resources granted" />
          <div className="stack-list">
            {(permissions.data || []).map((permission) => (
              <article className="message-item" key={permission.id}>
                <strong>{permission.resource_type === "llm_provider" ? "LLM access" : permission.resource_type}</strong>
                <span>{resourceLabel(permission, providerItems, resourceItems)}</span>
                {permission.resource_type === "llm_provider" ? (
                  <small>Managed by the squad LLM</small>
                ) : (
                  <button type="button" className="secondary small" onClick={() => onRevokePermission(permission)}>
                    Revoke
                  </button>
                )}
              </article>
            ))}
          </div>
        </section>

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

      <Modal {...agentModal.props} title="New agent" submitLabel="Add agent" submitDisabled={!llmReady}>
        {llmBlocked && <div className="notice warn compact">{llmBlocked}</div>}
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
        {llmStatus.state === "ready" && (
          <label>
            Model
            <select
              value={pickModel(llmStatus.models, agentForm.default_model, llmStatus.llm.model)}
              onChange={(event) => setAgentForm({ ...agentForm, default_model: event.target.value })}
            >
              {llmStatus.models.map((model) => (
                <option key={model} value={model}>
                  {model}
                </option>
              ))}
            </select>
            <small className="field-note">Served by {llmStatus.provider.name}, the provider this squad uses.</small>
          </label>
        )}
        <label>
          Idle timeout seconds
          <input
            type="number"
            min="1"
            value={agentForm.idle_timeout_sec}
            onChange={(event) => setAgentForm({ ...agentForm, idle_timeout_sec: event.target.value })}
          />
        </label>
      </Modal>

      <Modal {...accessModal.props} title="Grant resource access" submitLabel="Grant">
        {selectedAgent ? (
          <>
            <label>
              Agent
              <input value={selectedAgent.name} readOnly />
            </label>
            <label>
              Resource type
              <select value={permissionForm.resource_type} onChange={(event) => setPermissionForm({ resource_type: event.target.value as ResourceType, resource_id: "" })}>
                {registryTypes.map((item) => (
                  <option key={item.type} value={item.type}>
                    {item.label}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Resource
              <select value={permissionForm.resource_id} onChange={(event) => setPermissionForm({ ...permissionForm, resource_id: event.target.value })} required>
                <option value="">Select resource</option>
                {grantableResources.map((resource) => (
                  <option key={`${resource.type}:${resource.id}`} value={resource.id}>
                    {resource.name}
                  </option>
                ))}
              </select>
            </label>
          </>
        ) : (
          <div className="notice compact">Select an agent before granting resources</div>
        )}
      </Modal>
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
  onCreateTask: () => Promise<string | null>;
  onMoveTask: (taskID: string, status: TaskStatus) => void;
  onAssignTask: (taskID: string, assigneeAgentID: string) => void;
  onDeleteTask: (taskID: string) => void;
}) {
  // "" = everyone, "__unassigned" = no agent assigned, otherwise an agent id.
  const [assigneeFilter, setAssigneeFilter] = useState("");
  const [activeOnly, setActiveOnly] = useState(false);
  const taskModal = useModalForm(onCreateTask);

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
    <div className="section-stack">
      <ListHeader title="Task Board">
        <button type="button" className="primary" onClick={taskModal.show}>
          + New task
        </button>
      </ListHeader>

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

      <Modal {...taskModal.props} title="New task" submitLabel="Create task">
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
      </Modal>
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

// Shown in both Create Squad and a squad's Overview, so a squad can gain or
// change its LLM after creation. Deprecated providers are hidden unless one is
// already selected, because the gateway refuses them.
function LLMPicker({
  providers,
  loading,
  value,
  onChange,
  required = false,
}: {
  providers: LLMProvider[];
  loading: boolean;
  value: SquadLLM;
  onChange: (llm: SquadLLM) => void;
  required?: boolean;
}) {
  const options = providers.filter((provider) => provider.status === "active" || provider.id === value.provider_id);
  const models = providerModels(providers.find((provider) => provider.id === value.provider_id));

  if (loading && providers.length === 0) {
    return <div className="notice compact">Loading LLM providers</div>;
  }
  if (options.length === 0) {
    return (
      <div className="notice warn compact">
        No LLM providers are registered yet. A platform admin can add one under Admin.
      </div>
    );
  }
  return (
    <>
      <label>
        LLM provider
        <select
          value={value.provider_id}
          required={required}
          onChange={(event) => {
            const next = providers.find((provider) => provider.id === event.target.value);
            onChange({ provider_id: event.target.value, model: pickModel(providerModels(next), value.model) });
          }}
        >
          {!value.provider_id && <option value="">Choose a provider</option>}
          {options.map((provider) => (
            <option key={provider.id} value={provider.id}>
              {`${provider.name}${provider.status === "active" ? "" : " (deprecated)"}`}
            </option>
          ))}
        </select>
      </label>
      <label>
        Model
        <select
          value={pickModel(models, value.model)}
          required={required}
          disabled={models.length === 0}
          onChange={(event) => onChange({ ...value, model: event.target.value })}
        >
          {models.length === 0 && <option value="">No models available</option>}
          {models.map((model) => (
            <option key={model} value={model}>
              {model}
            </option>
          ))}
        </select>
      </label>
    </>
  );
}

function AgentLLMStatus({
  agent,
  permissions,
  status,
  onApply,
}: {
  agent: Agent;
  permissions: AgentPermission[];
  status: SquadLLMStatus;
  onApply: () => void;
}) {
  if (status.state !== "ready") {
    return null;
  }
  if (agentUsesSquadLLM(agent, permissions, status)) {
    return (
      <div className="notice good compact">
        Uses the squad LLM: {status.provider.name} · {agent.default_model}
      </div>
    );
  }
  return (
    <div className="notice warn compact">
      <span>{agent.name} is not on the squad LLM ({status.provider.name}).</span>
      <button type="button" className="secondary small" onClick={onApply}>
        Apply squad LLM
      </button>
    </div>
  );
}

function llmBlockedMessage(status: SquadLLMStatus): string {
  switch (status.state) {
    case "ready":
      return "";
    case "unset":
      return "This squad has no LLM yet. Choose one on the Overview tab before adding agents.";
    case "unknown":
      return "This squad's LLM provider is no longer registered. Choose another on the Overview tab.";
    case "deprecated":
      return `${status.provider.name} has been deprecated. Choose another LLM provider on the Overview tab.`;
    case "no-models":
      return `${status.provider.name} has no models configured. Choose another provider on the Overview tab, or ask a platform admin to add models.`;
  }
}
