// Package storage defines the persistence interfaces for the control plane and
// a Postgres implementation. The interfaces keep the httpapi and authz layers
// testable with fakes.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("storage: not found")

// ErrConflict is returned on a uniqueness violation (e.g. duplicate name).
var ErrConflict = errors.New("storage: conflict")

// ErrInvalidInput is returned when a caller violates a method's input contract
// (e.g. counting a correlation chain with an empty correlation ID).
var ErrInvalidInput = errors.New("storage: invalid input")

// Store is the aggregate persistence interface used by the API server.
type Store interface {
	UserStore
	SquadStore
	AgentStore
	KubernetesOutboxStore
	BoardStore
	TaskStore
	AgentMemoryStore
	MessageStore
	RegistryStore
	AIModelStore
	PermissionStore
	GrantStore
	MeteringStore
	WakeLatencyStore
	AuditStore
	WorkNotificationStore
	PromptTierStore
	PromptTemplateStore
	BuiltinToolStore
}

// PromptTierStore persists the stored prompt tiers (organization via
// instance_settings; squad and agent through their entity updates) and
// the append-only prompt revision history (S-PROMPT WP2, ADR-0011 D5).
// Revision rows are never updated or deleted — retention is forever.
type PromptTierStore interface {
	GetInstanceSettings(ctx context.Context) (*domain.InstanceSettings, error)
	// UpdateInstanceSettings writes the org tier. When the context carries
	// a PromptRevisionIntent and org_prompt actually changed, the revision
	// row is appended in the same transaction as the settings update.
	UpdateInstanceSettings(ctx context.Context, settings *domain.InstanceSettings) (*domain.InstanceSettings, error)
	// ListPromptRevisions returns revisions newest-first for one tier.
	ListPromptRevisions(ctx context.Context, scope, scopeID string, limit int) ([]*domain.PromptRevision, error)
}

// PromptTemplateStore persists admin-managed prompt templates (S-158).
// Templates pre-populate squad/agent prompts at creation; they are copies,
// not references.
type PromptTemplateStore interface {
	// CreatePromptTemplate inserts a template; duplicate name returns ErrConflict.
	CreatePromptTemplate(ctx context.Context, t *domain.PromptTemplate) (*domain.PromptTemplate, error)
	// ListPromptTemplates returns all templates ordered by name.
	ListPromptTemplates(ctx context.Context) ([]*domain.PromptTemplate, error)
	// GetPromptTemplate fetches one template by id.
	GetPromptTemplate(ctx context.Context, id string) (*domain.PromptTemplate, error)
	// UpdatePromptTemplate writes name/description/content/applies_to;
	// duplicate name (other row) returns ErrConflict.
	UpdatePromptTemplate(ctx context.Context, t *domain.PromptTemplate) (*domain.PromptTemplate, error)
	// DeletePromptTemplate removes a template.
	DeletePromptTemplate(ctx context.Context, id string) error
}

// KubernetesOutboxStore persists durable Kubernetes reconciliation intents.
type KubernetesOutboxStore interface {
	LeaseKubernetesOutbox(ctx context.Context, limit int, leaseFor time.Duration) ([]*domain.KubernetesOutboxEvent, error)
	MarkKubernetesOutboxApplied(ctx context.Context, id string) error
	MarkKubernetesOutboxFailed(ctx context.Context, id string, lastError string, retryAfter time.Duration) error
	ListKubernetesOutbox(ctx context.Context, status domain.KubernetesOutboxStatus, limit int) ([]*domain.KubernetesOutboxEvent, error)
	// LatestAppliedAgentUpsert returns the most recently applied
	// upsert_agent outbox event for an agent — the timestamp pair
	// (created = wake requested, updated = CR written) used for wake
	// latency attribution. ErrNotFound when none exists.
	LatestAppliedAgentUpsert(ctx context.Context, agentID string) (*domain.KubernetesOutboxEvent, error)
}

// UserStore persists human users.
type UserStore interface {
	GetUser(ctx context.Context, id string) (*domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	UpsertUser(ctx context.Context, u *domain.User) (*domain.User, error)
	SetUserRole(ctx context.Context, id string, role domain.Role) error
	ListUsers(ctx context.Context) ([]*domain.User, error)
}

// SquadStore persists squads.
type SquadStore interface {
	CreateSquad(ctx context.Context, s *domain.Squad) (*domain.Squad, error)
	GetSquad(ctx context.Context, id string) (*domain.Squad, error)
	GetSquadByName(ctx context.Context, ownerID, name string) (*domain.Squad, error)
	// SquadNamespaceExists reports whether ANY user already owns the given
	// namespace (S-156: two users with the same display name creating the
	// same squad name must not collide).
	SquadNamespaceExists(ctx context.Context, namespace string) (bool, error)
	UpdateSquad(ctx context.Context, s *domain.Squad) (*domain.Squad, error)
	DeleteSquad(ctx context.Context, id string) error
	ListSquads(ctx context.Context, ownerID string) ([]*domain.Squad, error) // ownerID "" = all
}

// AgentStore persists agents and their identities.
type AgentStore interface {
	CreateAgent(ctx context.Context, a *domain.Agent) (*domain.Agent, error)
	GetAgent(ctx context.Context, id string) (*domain.Agent, error)
	// GetAgentByNameForOwner returns an agent with the given name owned by
	// the given user (via its squad), any squad. S-156: agent names are
	// unique per user, not just per squad.
	GetAgentByNameForOwner(ctx context.Context, ownerID, name string) (*domain.Agent, error)
	UpdateAgent(ctx context.Context, a *domain.Agent) (*domain.Agent, error)
	DeleteAgent(ctx context.Context, id string) error
	ListAgents(ctx context.Context, squadID string) ([]*domain.Agent, error)
	SetAgentStatus(ctx context.Context, id string, status domain.AgentStatus) error

	CreateAgentIdentity(ctx context.Context, i *domain.AgentIdentity) (*domain.AgentIdentity, error)
	GetAgentIdentity(ctx context.Context, agentID string) (*domain.AgentIdentity, error)
	RotateAgentIdentity(ctx context.Context, agentID string, credentialRef string, credentialHash string, virtualKeyRef string) (*domain.AgentIdentity, error)
	// SetAgentIdentityGatewayKey records the gateway virtual-key token and
	// lifecycle state for an agent's identity.
	SetAgentIdentityGatewayKey(ctx context.Context, agentID string, token string, status domain.GatewayKeyStatus) (*domain.AgentIdentity, error)
	// ListAllAgents returns every agent across all squads (reconcile paths).
	ListAllAgents(ctx context.Context) ([]*domain.Agent, error)
}

// BoardStore persists Kanban boards.
type BoardStore interface {
	GetBoard(ctx context.Context, squadID string) (*domain.Board, error)
}

// TaskStore persists tasks.
type TaskStore interface {
	CreateTask(ctx context.Context, t *domain.Task) (*domain.Task, error)
	GetTask(ctx context.Context, id string) (*domain.Task, error)
	UpdateTask(ctx context.Context, t *domain.Task) (*domain.Task, error)
	// SetTaskWorkspace records the git workspace branch/commit a runtime
	// pushed for this task (audit linkage).
	SetTaskWorkspace(ctx context.Context, taskID string, resourceID string, branch string, commitSHA string) (*domain.Task, error)
	DeleteTask(ctx context.Context, id string) error
	ListTasks(ctx context.Context, boardID string, status domain.TaskStatus) ([]*domain.Task, error) // status "" = all
	ListAgentTasks(ctx context.Context, agentID string) ([]*domain.Task, error)
	ClaimNextTask(ctx context.Context, agentID string, workerID string, leaseFor time.Duration) (*domain.Task, error)
	// ListBoardTaskExecutions returns execution attempts still marked active for
	// every task on a board. Attempts whose lease has already lapsed are
	// included: an expired lease is the only signal that a worker stopped
	// heartbeating without completing, so callers need it to tell "running"
	// apart from "stalled".
	ListBoardTaskExecutions(ctx context.Context, boardID string) ([]*domain.TaskExecution, error)
	HeartbeatTaskExecution(ctx context.Context, agentID string, executionID string, fencingToken string, leaseFor time.Duration) (*domain.TaskExecution, error)
	// SetTaskExecutionPromptSHA records the composed-prompt sha on the
	// agent's active execution for a task (S-PROMPT WP5 run-audit).
	// ErrNotFound when no active execution exists for that agent+task.
	SetTaskExecutionPromptSHA(ctx context.Context, agentID string, taskID string, promptSHA string) (*domain.TaskExecution, error)
	// GetLatestTaskExecution returns the most recent execution row for a
	// task regardless of status (task-detail run-audit). ErrNotFound
	// when the task has never executed.
	GetLatestTaskExecution(ctx context.Context, taskID string) (*domain.TaskExecution, error)
	CompleteTaskExecution(ctx context.Context, agentID string, taskID string, executionID string, fencingToken string, status domain.TaskStatus, summary string) (*domain.Task, error)
	// ReapExpiredTaskExecutions marks active executions whose lease expired
	// before cutoff as expired and re-queues their tasks (in-progress → todo)
	// when no other live execution remains. Returns the number of executions
	// reaped. Safe to run concurrently: the updates are conditional, so a
	// heartbeat or complete that lands after cutoff wins and the row is left
	// untouched.
	ReapExpiredTaskExecutions(ctx context.Context, cutoff time.Time) (int, error)
}

// AgentMemoryStore persists bounded agent long-term memory.
type AgentMemoryStore interface {
	CreateAgentMemory(ctx context.Context, memory *domain.AgentMemory) (*domain.AgentMemory, error)
	ListAgentMemory(ctx context.Context, agentID string, squadID string, queryEmbedding []float64, limit int) ([]*domain.AgentMemory, error)
}

// MessageStore persists queued agent collaboration messages.
type MessageStore interface {
	CreateMessage(ctx context.Context, m *domain.Message) (*domain.Message, error)
	ListPendingMessages(ctx context.Context, agentID string) ([]*domain.Message, error)
	HasPendingMessages(ctx context.Context, agentID string) (bool, error)
	ListAgentMessageHistory(ctx context.Context, agentID string) ([]*domain.Message, error)
	// CountMessagesByCorrelation (S-164) returns the number of messages in a
	// correlation chain. The agent send path uses it to cap reply-loop depth.
	// An empty correlationID is a programming error for this caller and returns
	// ErrInvalidInput rather than counting every uncorrelated message.
	CountMessagesByCorrelation(ctx context.Context, correlationID string) (int, error)
	// ResetAgentChat (S-162) archives the current chat transcript into
	// agent memory and moves the agent's chat_reset_at boundary to now.
	// Returns the number of messages archived and the boundary time.
	ResetAgentChat(ctx context.Context, agentID, squadID, transcript string, metadata json.RawMessage) (int, time.Time, error)
	AckMessage(ctx context.Context, agentID string, messageID string) (*domain.Message, error)
	FailMessage(ctx context.Context, agentID string, messageID string, reason string) (*domain.Message, error)
	// CancelChatTurn (S-175) marks the newest still-live user chat message
	// for an agent (pending or delivered, after the chat-reset boundary,
	// with no agent reply yet) as cancelled. The runtime polls the message
	// status and abandons the turn without replying. Returns ErrNotFound
	// when there is no live turn to cancel.
	CancelChatTurn(ctx context.Context, agentID string) (*domain.Message, error)
	// GetMessage fetches a single message by id (no agent scoping — the
	// HTTP handler enforces it). Returns ErrNotFound when absent.
	GetMessage(ctx context.Context, messageID string) (*domain.Message, error)
	// ListAgentInbox (S-174) returns the agent's queue snapshot: pending
	// (never attempted), retrying (attempted but not terminal), recently
	// delivered, and dead letters, with total counts per section.
	ListAgentInbox(ctx context.Context, agentID string, limit int) (*domain.AgentInboxSnapshot, error)
	// ReplayDeadMessage (S-174) returns a dead message to the delivery
	// queue: status dead -> pending, attempts reset to 0, expiry refreshed
	// by the default TTL. Returns ErrNotFound when absent and ErrConflict
	// when the message is not dead. The correlation chain is untouched:
	// the replay is an operator action and does not consume send-path budget.
	ReplayDeadMessage(ctx context.Context, messageID string) (*domain.Message, error)
	// ListDeadLetters (S-174) lists dead messages across all squads for
	// the admin screen, newest first.
	ListDeadLetters(ctx context.Context, filter domain.DeadLetterFilter) ([]*domain.Message, error)
	// DeleteMessage hard-deletes one message row (admin prune, S-174).
	// Returns ErrNotFound when absent.
	DeleteMessage(ctx context.Context, messageID string) error
	// SweepConsultTimeouts (S-173) posts synthetic consult_timeout replies
	// for agent consults whose reply deadline passed with no correlated
	// reply, and marks them notified so each consult times out exactly once.
	// Returns the number of synthetic notifications posted.
	SweepConsultTimeouts(ctx context.Context, now time.Time) (int, error)
	// UpdateMessagePayload replaces a message's payload and status (used to
	// link a materialized delegated task back to its trigger message).
	UpdateMessagePayload(ctx context.Context, messageID string, payload json.RawMessage, status domain.MessageStatus) (*domain.Message, error)
}

// InboxStore persists owner-facing notifications (task completions, action
// requests). Visibility is scoped to the addressed user.
type InboxStore interface {
	CreateInboxMessage(ctx context.Context, msg *domain.InboxMessage) (*domain.InboxMessage, error)
	ListInboxMessages(ctx context.Context, userID string, unreadOnly bool, limit int) ([]*domain.InboxMessage, error)
	MarkInboxMessageRead(ctx context.Context, userID string, id string) (*domain.InboxMessage, error)
}

// WorkNotificationStore lets runtimes wait for assigned task or inbox changes
// without polling the control plane on a fixed interval.
type WorkNotificationStore interface {
	WaitForAgentWork(ctx context.Context, agentID string, timeout time.Duration) (bool, error)
}

// RegistryStore persists registry resources (LLM providers + generic resources).
type RegistryStore interface {
	CreateLLMProvider(ctx context.Context, p *domain.LLMProvider) (*domain.LLMProvider, error)
	GetLLMProvider(ctx context.Context, id string) (*domain.LLMProvider, error)
	UpdateLLMProvider(ctx context.Context, p *domain.LLMProvider) (*domain.LLMProvider, error)
	DeprecateLLMProvider(ctx context.Context, id string) error
	DeleteLLMProvider(ctx context.Context, id string) error
	ListLLMProviders(ctx context.Context) ([]*domain.LLMProvider, error)

	CreateResource(ctx context.Context, r *domain.RegistryResource) (*domain.RegistryResource, error)
	GetResource(ctx context.Context, typ domain.ResourceType, id string) (*domain.RegistryResource, error)
	UpdateResource(ctx context.Context, r *domain.RegistryResource) (*domain.RegistryResource, error)
	DeprecateResource(ctx context.Context, typ domain.ResourceType, id string) error
	// DeleteResource hard-deletes a registry resource and revokes every agent
	// grant that references it (S-103). Callers must surface usage to the
	// operator before forcing deletion.
	DeleteResource(ctx context.Context, typ domain.ResourceType, id string) error
	ListResources(ctx context.Context, typ domain.ResourceType) ([]*domain.RegistryResource, error)
}

// AIModelStore persists AI models (the grantable unit, ADR-0010 D1) and the
// user → model grants that gate which models a user may bind to agents (D3).
type AIModelStore interface {
	CreateAIModel(ctx context.Context, m *domain.AIModel) (*domain.AIModel, error)
	GetAIModel(ctx context.Context, id string) (*domain.AIModel, error)
	// ListAIModels returns models filtered by status; status "" = all.
	ListAIModels(ctx context.Context, status domain.ResourceStatus) ([]*domain.AIModel, error)
	UpdateAIModel(ctx context.Context, m *domain.AIModel) (*domain.AIModel, error)
	DeprecateAIModel(ctx context.Context, id string) error
	// DeleteAIModel hard-deletes a model and its user grants. It is
	// RESTRICTed while any agent is bound to the model (primary or fallback).
	DeleteAIModel(ctx context.Context, id string) error

	// GrantModelToUser grants the user the right to bind the model. The
	// (user, model) pair is unique; granting twice returns ErrConflict.
	GrantModelToUser(ctx context.Context, g *domain.UserModelGrant) (*domain.UserModelGrant, error)
	// RevokeModelFromUser removes one (user, model) grant. ErrNotFound when absent.
	RevokeModelFromUser(ctx context.Context, userID string, aiModelID string) error
	// ListUserModelGrants returns every model grant held by one user.
	ListUserModelGrants(ctx context.Context, userID string) ([]*domain.UserModelGrant, error)
	// ListUsersGrantedModel returns every grant of one model (who holds it).
	ListUsersGrantedModel(ctx context.Context, aiModelID string) ([]*domain.UserModelGrant, error)
}

// PermissionStore persists agent → resource grants (Layer-2 RBAC).
type PermissionStore interface {
	GrantAgentPermission(ctx context.Context, p *domain.AgentPermission) error
	RevokeAgentPermission(ctx context.Context, agentID string, typ domain.ResourceType, resourceID string) error
	ListAgentPermissions(ctx context.Context, agentID string) ([]*domain.AgentPermission, error)
	SetAgentPermissions(ctx context.Context, agentID string, perms []domain.AgentPermission) error
	AgentHasPermission(ctx context.Context, agentID string, typ domain.ResourceType, resourceID string) (bool, error)
	// ListPermissionsByResource returns every agent grant pointing at one
	// resource — the usage check behind delete warnings (S-103).
	ListPermissionsByResource(ctx context.Context, typ domain.ResourceType, resourceID string) ([]*domain.AgentPermission, error)
}

// GrantStore persists owner-issued access grants.
type GrantStore interface {
	CreateGrant(ctx context.Context, g *domain.AccessGrant) (*domain.AccessGrant, error)
	GetGrant(ctx context.Context, id string) (*domain.AccessGrant, error)
	RevokeGrant(ctx context.Context, id string) error
	ListGrants(ctx context.Context, squadID string) ([]*domain.AccessGrant, error)
	// UserMayAccessSquad reports whether user id may access squad for the requested action.
	UserMayAccessSquad(ctx context.Context, userID, squadID string, action string) (bool, error)
	// AgentMayMessageSquad reports whether agent id may message squad for the requested action.
	AgentMayMessageSquad(ctx context.Context, agentID, squadID string, action string) (bool, error)
}

// MeteringStore persists token-usage events.
type MeteringStore interface {
	RecordMetering(ctx context.Context, m *domain.MeteringEvent) error
	SumMetering(ctx context.Context, squadID, agentID string, since time.Time) (*domain.MeteringEvent, error) // aggregated; zero since = all time (S-169 month-to-date)
}

// WakeLatencyStore persists wake-path latency events (S-87). Record is
// idempotent per (agent, container start): a duplicate returns false
// without error. List returns events newest-first for one squad (empty
// squadID = all squads) since the given time.
type WakeLatencyStore interface {
	RecordWakeLatency(ctx context.Context, e *domain.WakeLatencyEvent) (bool, error)
	ListWakeLatency(ctx context.Context, squadID string, since time.Time, limit int) ([]*domain.WakeLatencyEvent, error)
}

// AuditStore persists the append-only audit log.
type AuditStore interface {
	RecordAudit(ctx context.Context, a *domain.AuditEntry) error
	ListAudit(ctx context.Context, squadID string, limit int) ([]*domain.AuditEntry, error)
}
