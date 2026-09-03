// Package storage defines the persistence interfaces for the control plane and
// a Postgres implementation. The interfaces keep the httpapi and authz layers
// testable with fakes.
package storage

import (
	"context"
	"errors"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("storage: not found")

// ErrConflict is returned on a uniqueness violation (e.g. duplicate name).
var ErrConflict = errors.New("storage: conflict")

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
	PermissionStore
	GrantStore
	MeteringStore
	AuditStore
	WorkNotificationStore
}

// KubernetesOutboxStore persists durable Kubernetes reconciliation intents.
type KubernetesOutboxStore interface {
	LeaseKubernetesOutbox(ctx context.Context, limit int, leaseFor time.Duration) ([]*domain.KubernetesOutboxEvent, error)
	MarkKubernetesOutboxApplied(ctx context.Context, id string) error
	MarkKubernetesOutboxFailed(ctx context.Context, id string, lastError string, retryAfter time.Duration) error
	ListKubernetesOutbox(ctx context.Context, status domain.KubernetesOutboxStatus, limit int) ([]*domain.KubernetesOutboxEvent, error)
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
	UpdateSquad(ctx context.Context, s *domain.Squad) (*domain.Squad, error)
	DeleteSquad(ctx context.Context, id string) error
	ListSquads(ctx context.Context, ownerID string) ([]*domain.Squad, error) // ownerID "" = all
}

// AgentStore persists agents and their identities.
type AgentStore interface {
	CreateAgent(ctx context.Context, a *domain.Agent) (*domain.Agent, error)
	GetAgent(ctx context.Context, id string) (*domain.Agent, error)
	UpdateAgent(ctx context.Context, a *domain.Agent) (*domain.Agent, error)
	DeleteAgent(ctx context.Context, id string) error
	ListAgents(ctx context.Context, squadID string) ([]*domain.Agent, error)
	SetAgentStatus(ctx context.Context, id string, status domain.AgentStatus) error

	CreateAgentIdentity(ctx context.Context, i *domain.AgentIdentity) (*domain.AgentIdentity, error)
	GetAgentIdentity(ctx context.Context, agentID string) (*domain.AgentIdentity, error)
	RotateAgentIdentity(ctx context.Context, agentID string, credentialRef string, credentialHash string, virtualKeyRef string) (*domain.AgentIdentity, error)
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
	AckMessage(ctx context.Context, agentID string, messageID string) (*domain.Message, error)
	FailMessage(ctx context.Context, agentID string, messageID string, reason string) (*domain.Message, error)
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
	ListLLMProviders(ctx context.Context) ([]*domain.LLMProvider, error)

	CreateResource(ctx context.Context, r *domain.RegistryResource) (*domain.RegistryResource, error)
	GetResource(ctx context.Context, typ domain.ResourceType, id string) (*domain.RegistryResource, error)
	UpdateResource(ctx context.Context, r *domain.RegistryResource) (*domain.RegistryResource, error)
	DeprecateResource(ctx context.Context, typ domain.ResourceType, id string) error
	ListResources(ctx context.Context, typ domain.ResourceType) ([]*domain.RegistryResource, error)
}

// PermissionStore persists agent → resource grants (Layer-2 RBAC).
type PermissionStore interface {
	GrantAgentPermission(ctx context.Context, p *domain.AgentPermission) error
	RevokeAgentPermission(ctx context.Context, agentID string, typ domain.ResourceType, resourceID string) error
	ListAgentPermissions(ctx context.Context, agentID string) ([]*domain.AgentPermission, error)
	SetAgentPermissions(ctx context.Context, agentID string, perms []domain.AgentPermission) error
	AgentHasPermission(ctx context.Context, agentID string, typ domain.ResourceType, resourceID string) (bool, error)
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
	SumMetering(ctx context.Context, squadID, agentID string) (*domain.MeteringEvent, error) // aggregated
}

// AuditStore persists the append-only audit log.
type AuditStore interface {
	RecordAudit(ctx context.Context, a *domain.AuditEntry) error
	ListAudit(ctx context.Context, squadID string, limit int) ([]*domain.AuditEntry, error)
}
