// Package domain defines the core entities of skquad. These mirror the
// physical schema in docs/data-model.md and are the shared vocabulary across
// the storage, authz, and httpapi packages.
package domain

import (
	"encoding/json"
	"time"
)

// Role is a Layer-1 (user) RBAC role, managed by the platform admin.
type Role string

const (
	RolePlatformAdmin Role = "platform_admin"
	RoleUser          Role = "user"
)

// Valid reports whether r is a known user role.
func (r Role) Valid() bool {
	return r == RolePlatformAdmin || r == RoleUser
}

// User is a human principal authenticated via OIDC.
type User struct {
	ID            string    `json:"id"`
	OIDCIssuer    string    `json:"oidc_issuer,omitempty"`
	OIDCSubject   string    `json:"oidc_subject,omitempty"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	Name          string    `json:"name"`
	Role          Role      `json:"role"`
	CreatedAt     time.Time `json:"created_at"`
}

// SquadStatus is the lifecycle state of a squad.
type SquadStatus string

const (
	SquadActive   SquadStatus = "active"
	SquadArchived SquadStatus = "archived"
)

// Squad is a team of agents with a mission and operating model. It maps to a
// Kubernetes namespace (see docs/domain-model.md).
type Squad struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Mission        string          `json:"mission"`
	OperatingModel json.RawMessage `json:"operating_model"`
	OwnerID        string          `json:"owner_id"`
	Namespace      string          `json:"namespace"`
	Status         SquadStatus     `json:"status"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// AgentStatus is the lifecycle state of an agent.
type AgentStatus string

const (
	AgentIdle  AgentStatus = "idle"
	AgentBusy  AgentStatus = "busy"
	AgentError AgentStatus = "error"
)

// Agent is a member of a squad. It runs in its own pod and has its own
// identity, credentials, and permission set.
type Agent struct {
	ID              string `json:"id"`
	SquadID         string `json:"squad_id"`
	Name            string `json:"name"`
	Role            string `json:"role"`
	SystemPrompt    string `json:"system_prompt,omitempty"`
	IdentityID      string `json:"identity_id,omitempty"`
	DefaultProvider string `json:"default_provider_id,omitempty"`
	DefaultModel    string `json:"default_model,omitempty"`
	// AIModelID is the bound primary model (ADR-0010 D4). Nullable until the
	// WP8 backfill makes it required; must be granted to the agent's owner.
	AIModelID string `json:"ai_model_id,omitempty"`
	// FallbackAIModelID is the optional failover model (ADR-0010 D4/D6).
	// Must differ from AIModelID and be granted to the agent's owner.
	FallbackAIModelID string          `json:"fallback_ai_model_id,omitempty"`
	Permissions       json.RawMessage `json:"permissions"`
	IdleTimeoutSec    int             `json:"idle_timeout_sec"`
	Status            AgentStatus     `json:"status"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	// WorkspaceSecrets is derived from the agent's project_workspace grants at
	// CR-write time (outbox worker); it is NOT persisted on the agents table.
	// See ADR-0009 and the operator WorkspaceSecret spec.
	WorkspaceSecrets []WorkspaceSecret `json:"workspace_secrets,omitempty"`
}

// WorkspaceSecret links a granted git workspace (registry resource id) to the
// name of a Kubernetes Secret in the agent namespace holding its HTTPS git
// token under the key "token".
type WorkspaceSecret struct {
	ResourceID string `json:"resourceId"`
	SecretName string `json:"secretName"`
}

// AgentIdentity is the owner-created identity + credential reference for an
// agent (see ADR-0007 and docs/identity-security.md).
type AgentIdentity struct {
	ID             string    `json:"id"`
	AgentID        string    `json:"agent_id"`
	CredentialRef  string    `json:"credential_ref"`
	CredentialHash string    `json:"-"`
	VirtualKeyRef  string    `json:"virtual_key_ref,omitempty"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	RotatedAt      time.Time `json:"rotated_at,omitempty"`
	// GatewayKeyToken is the LiteLLM key token (sha256 hash of the virtual
	// key, not the key itself) used to update/revoke the key at the gateway.
	GatewayKeyToken string `json:"-"`
	// GatewayKeyStatus tracks the virtual-key lifecycle so permission
	// changes can revoke/rotate it instead of leaving stale keys behind.
	GatewayKeyStatus GatewayKeyStatus `json:"gateway_key_status,omitempty"`
}

// GatewayKeyStatus is the lifecycle state of an agent's LLM gateway virtual key.
type GatewayKeyStatus string

const (
	// GatewayKeyNone means no virtual key has been issued (or it was reset).
	GatewayKeyNone GatewayKeyStatus = "none"
	// GatewayKeyActive means the key is live at the gateway.
	GatewayKeyActive GatewayKeyStatus = "active"
	// GatewayKeyRevoked means the key was revoked at the gateway.
	GatewayKeyRevoked GatewayKeyStatus = "revoked"
)

// KubernetesOutboxStatus is the delivery state for a durable Kubernetes write.
type KubernetesOutboxStatus string

const (
	KubernetesOutboxPending KubernetesOutboxStatus = "pending"
	KubernetesOutboxApplied KubernetesOutboxStatus = "applied"
	KubernetesOutboxFailed  KubernetesOutboxStatus = "failed"
)

// KubernetesOutboxEvent is a durable, retryable intent to mirror domain state
// into Kubernetes after the database mutation has committed.
type KubernetesOutboxEvent struct {
	ID            string                 `json:"id"`
	AggregateType string                 `json:"aggregate_type"`
	AggregateID   string                 `json:"aggregate_id"`
	Operation     string                 `json:"operation"`
	Payload       json.RawMessage        `json:"payload"`
	Status        KubernetesOutboxStatus `json:"status"`
	Attempts      int                    `json:"attempts"`
	LastError     string                 `json:"last_error,omitempty"`
	NextAttemptAt time.Time              `json:"next_attempt_at"`
	LockedUntil   time.Time              `json:"locked_until,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

// Kubernetes outbox aggregate and operation names.
const (
	KubernetesAggregateSquad = "squad"
	KubernetesAggregateAgent = "agent"

	KubernetesOpUpsertSquad = "upsert_squad"
	KubernetesOpDeleteSquad = "delete_squad"
	KubernetesOpUpsertAgent = "upsert_agent"
	KubernetesOpDeleteAgent = "delete_agent"
)

// KubernetesOutboxPayload carries enough non-secret state for the worker to
// perform idempotent Squad/Agent CR writes and deletes.
type KubernetesOutboxPayload struct {
	Squad    *Squad         `json:"squad,omitempty"`
	Agent    *Agent         `json:"agent,omitempty"`
	Identity *AgentIdentity `json:"identity,omitempty"`
}

// TaskStatus is a Kanban column.
type TaskStatus string

const (
	TaskTodo       TaskStatus = "todo"
	TaskInProgress TaskStatus = "in-progress"
	TaskInReview   TaskStatus = "in-review"
	TaskDone       TaskStatus = "done"
	TaskBlocked    TaskStatus = "blocked"
)

// Valid reports whether s is a known task status.
func (s TaskStatus) Valid() bool {
	switch s {
	case TaskTodo, TaskInProgress, TaskInReview, TaskDone, TaskBlocked:
		return true
	}
	return false
}

// Task is the unit of work on a squad's Kanban board.
type Task struct {
	ID              string     `json:"id"`
	BoardID         string     `json:"board_id"`
	SquadID         string     `json:"squad_id"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	Status          TaskStatus `json:"status"`
	AssigneeAgentID string     `json:"assignee_agent_id,omitempty"`
	CreatedByType   string     `json:"created_by_type"` // "user" | "agent"
	CreatedByID     string     `json:"created_by_id"`
	Position        int        `json:"position"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	// OriginMessageID links a task back to the delegate/handoff message
	// that materialized it ("" for user-created tasks).
	OriginMessageID string `json:"origin_message_id,omitempty"`
	// Workspace linkage: set when the task ran against a granted git
	// workspace. WorkspaceResourceID points at the registry resource, and
	// Branch/CommitSHA record what the runtime pushed (audit trail).
	WorkspaceResourceID string    `json:"workspace_resource_id,omitempty"`
	WorkspaceBranch     string    `json:"workspace_branch,omitempty"`
	WorkspaceCommitSHA  string    `json:"workspace_commit_sha,omitempty"`
	ExecutionID         string    `json:"execution_id,omitempty"`
	WorkerID            string    `json:"worker_id,omitempty"`
	FencingToken        string    `json:"fencing_token,omitempty"`
	LeaseExpiresAt      time.Time `json:"lease_expires_at,omitempty"`
}

// TaskExecutionStatus is the lifecycle of one runtime attempt for a task.
type TaskExecutionStatus string

const (
	TaskExecutionActive    TaskExecutionStatus = "active"
	TaskExecutionCompleted TaskExecutionStatus = "completed"
	TaskExecutionBlocked   TaskExecutionStatus = "blocked"
	TaskExecutionExpired   TaskExecutionStatus = "expired"
)

// TaskExecution is a fenced, lease-backed runtime attempt. Terminal result
// fields are committed atomically with the task status transition.
type TaskExecution struct {
	ID             string              `json:"id"`
	TaskID         string              `json:"task_id"`
	AgentID        string              `json:"agent_id"`
	WorkerID       string              `json:"worker_id"`
	FencingToken   string              `json:"fencing_token"`
	Status         TaskExecutionStatus `json:"status"`
	LeaseExpiresAt time.Time           `json:"lease_expires_at"`
	ResultStatus   TaskStatus          `json:"result_status,omitempty"`
	ResultSummary  string              `json:"result_summary,omitempty"`
	StartedAt      time.Time           `json:"started_at"`
	CompletedAt    time.Time           `json:"completed_at,omitempty"`
	UpdatedAt      time.Time           `json:"updated_at"`
}

// AgentMemory is a scoped long-term memory row for an agent. Squad-scoped
// memory is opt-in by setting SquadID; otherwise the row is private to AgentID.
type AgentMemory struct {
	ID             string          `json:"id"`
	AgentID        string          `json:"agent_id"`
	SquadID        string          `json:"squad_id,omitempty"`
	Content        string          `json:"content"`
	RawContent     string          `json:"raw_content,omitempty"`
	TrustLevel     string          `json:"trust_level"`
	Provenance     string          `json:"provenance"`
	ReviewStatus   string          `json:"review_status"`
	Embedding      []float64       `json:"embedding,omitempty"`
	EmbeddingModel string          `json:"embedding_model,omitempty"`
	SourceTaskID   string          `json:"source_task_id,omitempty"`
	Metadata       json.RawMessage `json:"metadata"`
	CreatedAt      time.Time       `json:"created_at"`
}

// MessageType identifies a queued agent collaboration message.
type MessageType string

const (
	MessageConsult  MessageType = "consult"
	MessageDelegate MessageType = "delegate"
	MessageHandoff  MessageType = "handoff"
	MessagePing     MessageType = "ping"
	MessageReply    MessageType = "reply"
)

// Valid reports whether t is a known message type.
func (t MessageType) Valid() bool {
	switch t {
	case MessageConsult, MessageDelegate, MessageHandoff, MessagePing, MessageReply:
		return true
	}
	return false
}

// MessageStatus is the lifecycle state of an inbox message.
type MessageStatus string

const (
	MessagePending   MessageStatus = "pending"
	MessageDelivered MessageStatus = "delivered"
	MessageExpired   MessageStatus = "expired"
	MessageDead      MessageStatus = "dead"
)

// Message is a durable queued message for an agent inbox.
type Message struct {
	ID             string          `json:"id"`
	FromType       string          `json:"from_type"` // "user" | "agent"
	FromID         string          `json:"from_id"`
	ToAgentID      string          `json:"to_agent_id"`
	SquadID        string          `json:"squad_id"`
	Type           MessageType     `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	Status         MessageStatus   `json:"status"`
	CorrelationID  string          `json:"correlation_id,omitempty"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	NextRetryAt    time.Time       `json:"next_retry_at,omitempty"`
	ExpiresAt      time.Time       `json:"expires_at,omitempty"`
	TerminalReason string          `json:"terminal_reason,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	DeliveredAt    time.Time       `json:"delivered_at,omitempty"`
}

// InboxKind classifies owner-facing notifications.
type InboxKind string

const (
	// InboxTaskCompleted is emitted by the control plane when an agent moves a
	// task to in-review/done. Agents cannot forge it via the notify endpoint.
	InboxTaskCompleted InboxKind = "task_completed"
	// InboxActionRequired is emitted when a task blocks and agents may request
	// it explicitly (e.g. approval to proceed).
	InboxActionRequired InboxKind = "action_required"
)

// InboxMessage is a notification addressed to a squad owner, not part of the
// agent-to-agent message queue.
type InboxMessage struct {
	ID          string    `json:"id"`
	SquadID     string    `json:"squad_id"`
	UserID      string    `json:"user_id"`
	FromAgentID string    `json:"from_agent_id,omitempty"`
	TaskID      string    `json:"task_id,omitempty"`
	Kind        InboxKind `json:"kind"`
	Message     string    `json:"message"`
	ReadAt      time.Time `json:"read_at,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// IsRead reports whether the owner has acknowledged the notification.
func (m *InboxMessage) IsRead() bool { return !m.ReadAt.IsZero() }

// Board is a squad's Kanban board (one per squad).
type Board struct {
	ID        string    `json:"id"`
	SquadID   string    `json:"squad_id"`
	CreatedAt time.Time `json:"created_at"`
}

// ResourceStatus is the lifecycle state of a registry resource.
type ResourceStatus string

const (
	ResourceActive     ResourceStatus = "active"
	ResourceDeprecated ResourceStatus = "deprecated"
)

// ResourceType identifies a kind of registry resource.
type ResourceType string

const (
	ResLLMProvider      ResourceType = "llm_provider"
	ResSkill            ResourceType = "skill"
	ResTool             ResourceType = "tool"
	ResAPI              ResourceType = "api"
	ResKnowledgeBase    ResourceType = "knowledge_base"
	ResProjectWorkspace ResourceType = "project_workspace"
)

// LLMProvider is a model endpoint registered in the registry (BYOM).
type LLMProvider struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"` // openai, anthropic, ollama, ...
	BaseURL      string          `json:"base_url"`
	APIKeyRef    string          `json:"api_key_ref"`
	DefaultModel string          `json:"default_model,omitempty"`
	Models       json.RawMessage `json:"models"`
	Pricing      json.RawMessage `json:"pricing"`
	Status       ResourceStatus  `json:"status"`
	RegisteredBy string          `json:"registered_by"`
	CreatedAt    time.Time       `json:"created_at"`
}

// AIModel is an admin-registered, grantable model (ADR-0010 D1). It
// references an internal provider credential holder so N models from one
// account share one base_url + api_key_ref (D2). This — not the provider —
// is the unit shown in the UI and granted to users.
type AIModel struct {
	ID          string `json:"id"`
	ProviderID  string `json:"provider_id"`
	DisplayName string `json:"display_name"`
	ModelName   string `json:"model_name"`
	// ContextWindow is the model's maximum context in tokens (0 = unknown).
	ContextWindow int `json:"context_window"`
	// SupportsTools records tool-calling capability; fallbacks without it
	// break the agent tool loop (ADR-0010 Risk 2).
	SupportsTools bool `json:"supports_tools"`
	// Pricing holds the four per-1M rates (input_per_1m, cached_input_per_1m,
	// cache_write_per_1m, output_per_1m) per ADR-0010 D8. Cost is
	// snapshotted at metering time and never re-derived from live pricing.
	Pricing json.RawMessage `json:"pricing"`
	// LongContextThresholdTokens splits short vs long context pricing tiers.
	LongContextThresholdTokens int            `json:"long_context_threshold_tokens"`
	Status                     ResourceStatus `json:"status"`
	RegisteredBy               string         `json:"registered_by"`
	CreatedAt                  time.Time      `json:"created_at"`
	UpdatedAt                  time.Time      `json:"updated_at,omitempty"`
}

// UserModelGrant records that a user may bind an AI Model to their agents
// (ADR-0010 D3: grants follow people, the authenticated OIDC principal).
type UserModelGrant struct {
	ID            string    `json:"id"`
	GranteeUserID string    `json:"grantee_user_id"`
	AIModelID     string    `json:"ai_model_id"`
	GrantedBy     string    `json:"granted_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// RegistryResource is a generic registry entry (skill, tool, api, kb, ws).
type RegistryResource struct {
	ID           string          `json:"id"`
	Type         ResourceType    `json:"type"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Endpoint     string          `json:"endpoint,omitempty"`
	AuthRef      string          `json:"auth_ref,omitempty"`
	Manifest     json.RawMessage `json:"manifest"`
	Status       ResourceStatus  `json:"status"`
	RegisteredBy string          `json:"registered_by"`
	CreatedAt    time.Time       `json:"created_at"`
}

// AgentPermission grants an agent access to a registry resource (Layer-2 RBAC,
// squad-owner managed).
type AgentPermission struct {
	ID           string       `json:"id"`
	AgentID      string       `json:"agent_id"`
	ResourceType ResourceType `json:"resource_type"`
	ResourceID   string       `json:"resource_id"`
	GrantedBy    string       `json:"granted_by"`
	CreatedAt    time.Time    `json:"created_at"`
}

// GranteeType identifies who an access grant is issued to.
type GranteeType string

const (
	GranteeUser  GranteeType = "user"
	GranteeAgent GranteeType = "agent"
)

// AccessGrant is an owner-issued permission for a user or another squad's agent
// to talk to the squad's agents (and, for agents, to add tasks / ping).
type AccessGrant struct {
	ID          string      `json:"id"`
	SquadID     string      `json:"squad_id"`
	GranteeType GranteeType `json:"grantee_type"`
	GranteeID   string      `json:"grantee_id"`
	Permissions string      `json:"permissions"` // comma-separated: talk,add_task,ping
	GrantedBy   string      `json:"granted_by"`
	CreatedAt   time.Time   `json:"created_at"`
}

// MeteringEvent records token usage for an agent (and squad) LLM call.
type MeteringEvent struct {
	ID           string    `json:"id"`
	AgentID      string    `json:"agent_id"`
	SquadID      string    `json:"squad_id"`
	TaskID       string    `json:"task_id,omitempty"`
	ProviderID   string    `json:"provider_id"`
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Cost         float64   `json:"cost"`
	Currency     string    `json:"currency"`
	Timestamp    time.Time `json:"timestamp"`
}

// WakeLatencyEvent records one task-delivery wake path (S-87): from the
// assignment that triggered the wake (upsert_agent outbox event creation)
// through the CR write, the runtime's container start, and the claim that
// delivered the task. Segments are computed at claim time; E2EMs is the
// SLO number (target p95 < 20s).
type WakeLatencyEvent struct {
	ID                 string    `json:"id"`
	AgentID            string    `json:"agent_id"`
	SquadID            string    `json:"squad_id"`
	TaskID             string    `json:"task_id,omitempty"`
	WakeRequestedAt    time.Time `json:"wake_requested_at"`
	CRAppliedAt        time.Time `json:"cr_applied_at"`
	ContainerStartedAt time.Time `json:"container_started_at"`
	ClaimedAt          time.Time `json:"claimed_at"`
	QueueMs            float64   `json:"queue_ms"`
	ScaleupMs          float64   `json:"scaleup_ms"`
	ClaimDelayMs       float64   `json:"claim_delay_ms"`
	E2EMs              float64   `json:"e2e_ms"`
	ColdStart          bool      `json:"cold_start"`
}

// AuditEntry is an append-only record of a significant action.
type AuditEntry struct {
	ID           string          `json:"id"`
	ActorType    string          `json:"actor_type"` // "user" | "agent" | "system"
	ActorID      string          `json:"actor_id"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	SquadID      string          `json:"squad_id,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
	Timestamp    time.Time       `json:"timestamp"`
}
