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
	ID      string `json:"id"`
	Name    string `json:"name"`
	Mission string `json:"mission"`
	// Prompt is the layer-3 squad prompt (ADR-0011). Mission stays the
	// short listing summary; Prompt is the full tier text composed between
	// the organization and agent blocks.
	Prompt         string          `json:"prompt,omitempty"`
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
	ID           string `json:"id"`
	SquadID      string `json:"squad_id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	IdentityID   string `json:"identity_id,omitempty"`
	// AIModelID is the bound primary model (ADR-0010 D4). Nullable until the
	// WP8 backfill makes it required; must be granted to the agent's owner.
	AIModelID string `json:"ai_model_id,omitempty"`
	// FallbackAIModelID is the optional failover model (ADR-0010 D4/D6).
	// Must differ from AIModelID and be granted to the agent's owner.
	FallbackAIModelID string          `json:"fallback_ai_model_id,omitempty"`
	Permissions       json.RawMessage `json:"permissions"`
	IdleTimeoutSec    int             `json:"idle_timeout_sec"`
	// ThinkingLevel (S-178) is the per-agent reasoning effort knob:
	// "low" | "medium" | "high" ("" = unset, provider default). It flows
	// through the Agent CR (spec.thinkingLevel) into the runtime env
	// (SKQUAD_THINKING_LEVEL) and lands on LLM requests as litellm's
	// reasoning_effort parameter. The UI defaults the selector to medium.
	ThinkingLevel string `json:"thinking_level,omitempty"`
	// StorageEnabled and StorageSize configure the agent's durable workspace
	// PVC (S-138). They flow into the Agent CR's spec.storage via the
	// outbox writer. StorageClass is deliberately NOT part of this surface:
	// tenant-selectable storage classes are a portability/cost footgun, so
	// only the platform (Helm/env at the control plane) may set it.
	StorageEnabled bool   `json:"storage_enabled"`
	StorageSize    string `json:"storage_size,omitempty"`
	// DeploymentName (S-156) is the deterministic Kubernetes Deployment
	// name for the agent: skquad-<owner>-agent-<agent-name>. Computed once
	// at creation (names are immutable) and persisted; the operator uses it
	// instead of the CR name when present.
	DeploymentName string `json:"deployment_name,omitempty"`
	// ChatResetAt (S-162) marks the instant the user reset the chat
	// thread. Messages created at or before it are excluded from the
	// chat history and the runtime context; the transcript is archived
	// to agent_memory first. NULL/zero = never reset.
	ChatResetAt time.Time   `json:"chat_reset_at,omitempty"`
	Status      AgentStatus `json:"status"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	// WorkspaceSecrets is derived from the agent's project_workspace grants at
	// CR-write time (outbox worker); it is NOT persisted on the agents table.
	// See ADR-0009 and the operator WorkspaceSecret spec.
	WorkspaceSecrets []WorkspaceSecret `json:"workspace_secrets,omitempty"`
	// AIModelName / FallbackAIModelName are derived from the bound AI Model
	// rows at CR-apply time (outbox worker), like WorkspaceSecrets — they are
	// NOT persisted on the agents table. They carry the gateway-routable
	// model_name so the Agent CR's defaultModel (and the runtime's
	// SKQUAD_DEFAULT_MODEL) reflects the bound AI Model rather than the
	// legacy free-text column (WP5, ADR-0010 D4).
	AIModelName         string `json:"ai_model_name,omitempty"`
	FallbackAIModelName string `json:"fallback_ai_model_name,omitempty"`
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
	// Generation is the credential epoch (TG-2, design §5.2
	// generation-bound tokens). It starts at 1 and increments on every
	// rotation; the policy endpoint surfaces it so the gateway can bind
	// sessions to a generation and material from an old generation dies
	// with the rotation that replaced it.
	Generation int `json:"generation"`
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
	// TaskBacklog (S-213) is the parking column for tasks that are NOT yet
	// ready to be started. It sits left of the TO DO column on the board and is
	// deliberately excluded from every agent-facing pickup/listing path:
	// a human moving a card out of Backlog is the instruction to start it.
	TaskBacklog    TaskStatus = "backlog"
	TaskTodo       TaskStatus = "todo"
	TaskInProgress TaskStatus = "in-progress"
	TaskInReview   TaskStatus = "in-review"
	TaskDone       TaskStatus = "done"
	TaskBlocked    TaskStatus = "blocked"
)

// Valid reports whether s is a known task status.
func (s TaskStatus) Valid() bool {
	switch s {
	case TaskBacklog, TaskTodo, TaskInProgress, TaskInReview, TaskDone, TaskBlocked:
		return true
	}
	return false
}

// AgentPickupStatuses lists the statuses an agent may claim/start work from
// (S-213). Backlog is intentionally absent: agents must never pick up
// backlog tasks unless a human moves them out (the move IS the instruction).
// Agent-facing queries must use this as an allowlist, never a denylist.
// Only TO DO column tasks are ready to start (S-228); in-progress is the
// sole exception so a crashed run can resume its own claimed task.
func AgentPickupStatuses() []TaskStatus {
	return []TaskStatus{TaskTodo, TaskInProgress}
}

// IsAgentPickupStatus reports whether an agent may claim/start work from s
// (S-228). Everything outside AgentPickupStatuses — backlog included — is
// not claimable and must be rejected server-side, not merely hidden.
func (s TaskStatus) IsAgentPickupStatus() bool {
	for _, allowed := range AgentPickupStatuses() {
		if s == allowed {
			return true
		}
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
	// TaskNumber is the per-squad sequential display reference (S-184),
	// rendered "T-<n>" on board cards and the task screen. Immutable after
	// creation (backfilled by migration 0027 for pre-existing tasks).
	TaskNumber int       `json:"task_number"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// OriginMessageID links a task back to the delegate/handoff message
	// that materialized it ("" for user-created tasks).
	OriginMessageID string `json:"origin_message_id,omitempty"`
	// Workspace linkage: set when the task ran against a granted git
	// workspace. WorkspaceResourceID points at the registry resource, and
	// Branch/CommitSHA record what the runtime pushed (audit trail).
	WorkspaceResourceID string `json:"workspace_resource_id,omitempty"`
	WorkspaceBranch     string `json:"workspace_branch,omitempty"`
	WorkspaceCommitSHA  string `json:"workspace_commit_sha,omitempty"`
	// PromptSHA is the composed-prompt sha of the task's latest execution
	// attempt (S-PROMPT WP5 run-audit; "env_legacy" for the fallback
	// path, "" when unknown/older runtime). Attached from the execution
	// row by read paths — never stored on the task itself.
	PromptSHA      string    `json:"prompt_sha,omitempty"`
	ExecutionID    string    `json:"execution_id,omitempty"`
	WorkerID       string    `json:"worker_id,omitempty"`
	FencingToken   string    `json:"fencing_token,omitempty"`
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitempty"`
	// Result carries the final outcome text of the latest terminal
	// transition (S-181): the completion summary for done tasks, the
	// blocked reason for blocked tasks. ResultStatus records which
	// status produced the text so the UI can label it correctly even
	// after a manual move; ResultAt is when it was recorded.
	Result       string    `json:"result,omitempty"`
	ResultStatus string    `json:"result_status,omitempty"`
	ResultAt     time.Time `json:"result_at,omitempty"`
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
	// PromptSHA is the sha256 of the composed effective prompt the
	// runtime used for this attempt, reported at task start
	// (S-PROMPT WP5, ADR-0011 D5). 'env_legacy' marks the fallback
	// path; '' means the runtime predates run-audit reporting.
	PromptSHA   string    `json:"prompt_sha,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
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
	// MessageCancelled (S-175) marks a chat turn the user stopped from the
	// chat UI. The runtime polls for it and abandons the in-flight turn
	// without posting a reply.
	MessageCancelled MessageStatus = "cancelled"
)

// Message is a durable queued message for an agent inbox.
type Message struct {
	ID       string `json:"id"`
	FromType string `json:"from_type"` // "user" | "agent"
	FromID   string `json:"from_id"`
	// FromDisplay (S-235) is the sender's display name resolved at read
	// time: agents show their current name (renames after the event still
	// show the new name); users show their first name with the email
	// local-part as fallback. Additive wire field — clients that ignore
	// it keep working off from_type/from_id.
	FromDisplay   string          `json:"from_display,omitempty"`
	ToAgentID     string          `json:"to_agent_id"`
	SquadID       string          `json:"squad_id"`
	Type          MessageType     `json:"type"`
	Payload       json.RawMessage `json:"payload"`
	Status        MessageStatus   `json:"status"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Attempts      int             `json:"attempts"`
	MaxAttempts   int             `json:"max_attempts"`
	NextRetryAt   time.Time       `json:"next_retry_at,omitempty"`
	ExpiresAt     time.Time       `json:"expires_at,omitempty"`
	// TimeoutAt (S-173) is the reply deadline for an agent-sent consult.
	// Zero means no consult timeout applies (chat, ping, delegate, or
	// sends that explicitly opted out).
	TimeoutAt time.Time `json:"timeout_at,omitempty"`
	// TimeoutNotifiedAt (S-173) is the sweeper's idempotency marker:
	// once set, the consult has produced its consult_timeout notice and
	// will never time out again. Internal bookkeeping, not API surface.
	TimeoutNotifiedAt time.Time `json:"-"`
	TerminalReason    string    `json:"terminal_reason,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	DeliveredAt       time.Time `json:"delivered_at,omitempty"`
}

// AgentInboxSnapshot (S-174) is the read-only observability view of one
// agent's delivery queue: what is waiting, what is retrying, what landed
// recently, and what died and why. Counts are total; the message lists are
// capped at the requested limit so a flooded queue never blows the payload.
type AgentInboxSnapshot struct {
	PendingCount    int        `json:"pending_count"`
	OldestPendingAt *time.Time `json:"oldest_pending_at,omitempty"`
	RetryingCount   int        `json:"retrying_count"`
	DeliveredCount  int        `json:"delivered_count"`
	DeadCount       int        `json:"dead_count"`
	Pending         []*Message `json:"pending"`
	Retrying        []*Message `json:"retrying"`
	Delivered       []*Message `json:"delivered"`
	Dead            []*Message `json:"dead"`
}

// DeadLetterFilter (S-174) narrows the admin dead-letter listing. Empty
// string / nil fields are not filtered. Reason is a case-insensitive
// substring of terminal_reason.
type DeadLetterFilter struct {
	SquadID string
	AgentID string
	Type    string
	Reason  string
	Since   *time.Time
	Until   *time.Time
	Limit   int
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
	// InboxAgentMessage (S-193) is content a human explicitly asked an
	// agent to deliver to their inbox via the send_inbox tool. Unlike the
	// system-emitted kinds it carries agent-authored content, so consumers
	// must treat its body as untrusted agent output.
	InboxAgentMessage InboxKind = "agent_message"
	// InboxBudgetWarning (S-203 WP3) warns a user that their monthly
	// budget (or the platform-wide limit) reached a warning threshold.
	// System-emitted, user-level (no squad context).
	InboxBudgetWarning InboxKind = "budget_warning"
	// InboxBudgetStopped (S-203 WP3) tells a user their budget/limit is
	// exhausted and their agents were stopped at end of turn.
	InboxBudgetStopped InboxKind = "budget_stopped"
)

// NotificationType classifies what went wrong (S-193). Notifications are
// transient "something needs a human" alerts, deliberately separate from
// inbox messages: an inbox message never creates a notification row.
type NotificationType string

const (
	// NotificationTaskFailed marks a task attempt that failed (agent
	// reported an error while executing it).
	NotificationTaskFailed NotificationType = "task_failed"
	// NotificationTaskStuck marks work that has made no progress while
	// claimed. Reserved: the dedicated stuck-scanner ships as a follow-up.
	NotificationTaskStuck NotificationType = "task_stuck"
	// NotificationAgentDied marks an execution whose lease expired — the
	// agent died mid-task and the task was re-queued.
	NotificationAgentDied NotificationType = "agent_died"
	// NotificationTaskBlocked marks a task blocked awaiting user input
	// ("requires attention": a decision or answer).
	NotificationTaskBlocked NotificationType = "task_blocked"
	// NotificationBudgetWarning marks a monthly budget (or the
	// platform-wide limit) crossing a warning threshold (S-203 WP4).
	// User-scoped: no squad/task context.
	NotificationBudgetWarning NotificationType = "budget_warning"
	// NotificationBudgetStopped marks a budget/limit exhausted and the
	// user's agents stopped at end of turn (S-203 WP4).
	NotificationBudgetStopped NotificationType = "budget_stopped"
)

// AllNotificationTypes enumerates the mailable notification types (S-199).
// Used for validating user preference payloads.
var AllNotificationTypes = []NotificationType{
	NotificationTaskFailed,
	NotificationTaskStuck,
	NotificationAgentDied,
	NotificationTaskBlocked,
	NotificationBudgetWarning,
	NotificationBudgetStopped,
}

// IsKnownNotificationType reports whether t is one of the known
// notification types (S-199 preference validation).
func IsKnownNotificationType(t NotificationType) bool {
	for _, known := range AllNotificationTypes {
		if known == t {
			return true
		}
	}
	return false
}

// NotificationPreferences (S-199) is a user's mute list for the bell.
// MutedTypes holds the types the user does NOT want delivered; no row /
// empty list means every type is enabled (default).
type NotificationPreferences struct {
	MutedTypes []NotificationType `json:"muted_types"`
}

// IsMuted reports whether the given type is muted in these preferences.
func (p *NotificationPreferences) IsMuted(t NotificationType) bool {
	if p == nil {
		return false
	}
	for _, m := range p.MutedTypes {
		if m == t {
			return true
		}
	}
	return false
}

// NotificationSeverity orders how loud a notification is.
type NotificationSeverity string

const (
	NotificationInfo    NotificationSeverity = "info"
	NotificationWarning NotificationSeverity = "warning"
	NotificationError   NotificationSeverity = "error"
)

// Notification is a recipient-scoped alert surfaced by the top-bar bell.
// Read state is tracked; removal is not required (retention policy is a
// follow-up concern).
type Notification struct {
	ID       string               `json:"id"`
	UserID   string               `json:"user_id"`
	SquadID  string               `json:"squad_id"`
	TaskID   string               `json:"task_id,omitempty"`
	AgentID  string               `json:"agent_id,omitempty"`
	Type     NotificationType     `json:"type"`
	Severity NotificationSeverity `json:"severity"`
	Message  string               `json:"message"`
	// ReadAt is a pointer so that unread rows serialize without the
	// read_at key at all: time.Time + omitempty still emits
	// "0001-01-01T00:00:00Z" for unset values, which the frontend
	// treats as read (S-209 retest bug).
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// IsRead reports whether the recipient has acknowledged the alert.
func (n *Notification) IsRead() bool { return n.ReadAt != nil }

// ReapedExecution (S-193) is the identity of one execution the reaper
// expired, so the caller can notify the squad owner about the dead
// attempt without a second sweep.
type ReapedExecution struct {
	ExecutionID string `json:"execution_id"`
	TaskID      string `json:"task_id"`
	AgentID     string `json:"agent_id"`
}

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
	// Subject and Body (S-181) make owner notifications read like an
	// email: a one-line subject plus a body with the key context and a
	// link to the task screen. Both are optional — older messages and
	// plain notifications leave them empty and consumers fall back to
	// Message.
	Subject   string     `json:"subject,omitempty"`
	Body      string     `json:"body,omitempty"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	// Attachments (S-216) carries the file attachments delivered with
	// this message via send_inbox. It is populated by the HTTP layer
	// (list/detail enrichment) — storage scans leave it empty. Bytes
	// never ride on this struct; see InboxAttachment.Data.
	Attachments []InboxAttachment `json:"attachments,omitempty"`
}

// IsRead reports whether the owner has acknowledged the notification.
func (m *InboxMessage) IsRead() bool { return m.ReadAt != nil }

// InboxAttachment (S-216) is one file attached to an inbox message.
// Metadata (filename, sniffed content type, size, sha256) is always
// safe to serialize; Data carries the raw bytes only on the download
// path and is excluded from JSON.
type InboxAttachment struct {
	ID          string    `json:"id"`
	MessageID   string    `json:"message_id"`
	SquadID     string    `json:"squad_id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	SHA256      string    `json:"sha256"`
	URL         string    `json:"url"`
	CreatedAt   time.Time `json:"created_at"`
	Data        []byte    `json:"-"`
}

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
	ResAIProvider ResourceType = "ai_provider"
	ResSkill      ResourceType = "skill"
	ResTool       ResourceType = "tool"
	// ResAPI is the legacy type name; TG-2 canonicalizes it to ResRest.
	ResAPI  ResourceType = "api"
	ResWeb  ResourceType = "web"
	ResRest ResourceType = "rest"
	ResMCP  ResourceType = "mcp"
	ResGit  ResourceType = "git"
	// ResSSH is the TG-10 Terminal-as-a-Service type (docs/tool-gateway.md
	// §6.6): governed SSH diagnostics via the terminal-service. Credentials
	// (CA-signed ephemeral certs or BYO static keys) live ONLY in the
	// quarantine namespace; agents hold none.
	ResSSH              ResourceType = "ssh"
	ResKnowledgeBase    ResourceType = "knowledge_base"
	ResProjectWorkspace ResourceType = "project_workspace"
)

// CanonicalResourceType maps the legacy 'api' type to its TG-2 canonical
// form 'rest' (design §7 type widening). All storage paths run types
// through this so old consumers asking for 'api' transparently operate on
// 'rest' rows.
func CanonicalResourceType(t ResourceType) ResourceType {
	if t == ResAPI {
		return ResRest
	}
	return t
}

// AIProvider is a model endpoint registered in the registry (BYOM).
type AIProvider struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // openai, anthropic, ollama, ...
	BaseURL   string `json:"base_url"`
	APIKeyRef string `json:"api_key_ref"`
	// APIKeyMask is the display-safe tail of the key (S-155): computed at
	// write time so list/get responses never resolve the Secret. Never
	// serialized — the HTTP layer exposes it as api_key_masked only.
	APIKeyMask string `json:"-"`
	// WP8 (0014): legacy default_model / models / pricing fields removed.
	// Model configuration lives exclusively on ai_models rows bound via
	// agents.ai_model_id (ADR-0010).
	Status       ResourceStatus `json:"status"`
	RegisteredBy string         `json:"registered_by"`
	CreatedAt    time.Time      `json:"created_at"`
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
	// SupportsVision (S-200) records image-input capability. The agent
	// runtime only embeds attached images as base64 content parts when the
	// bound model is vision-capable; non-vision models degrade to the text
	// reference. Provisioned into the gateway deployment model_info so the
	// gateway can enforce it as defense-in-depth.
	SupportsVision bool `json:"supports_vision"`
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
	// TG-2 governed egress fields (design §7). EndpointConfig never
	// contains secret material — credentials live behind AuthRef (K8s
	// Secret) and are resolved only at call time by the gateway.
	EndpointConfig json.RawMessage `json:"endpoint_config,omitempty"`
	PolicyCeiling  json.RawMessage `json:"policy_ceiling,omitempty"`
	RiskTier       string          `json:"risk_tier,omitempty"`
	EgressClass    string          `json:"egress_class,omitempty"`
	OwnerUserID    string          `json:"owner_user_id,omitempty"`
	// TG-5 slice B2a (S-244-series): MCP tool snapshot (docs/tool-gateway.md §6.3).
	// Registration enumerates the live tool set through the gateway
	// (POST /internal/mcp/enumerate); the CP persists the enumerated tools
	// (name + description + inputSchema), the gateway-computed canonical hash
	// VERBATIM (the CP never recomputes it) and the enumeration timestamp.
	// Drift detection (slice B2b) re-enumerates and compares against
	// ToolsHash. Meaningful only on mcp resources; empty elsewhere.
	ToolsSnapshot     json.RawMessage `json:"tools_snapshot,omitempty"`
	ToolsHash         string          `json:"tools_hash,omitempty"`
	ToolsEnumeratedAt *time.Time      `json:"tools_enumerated_at,omitempty"`
	// TG-5 slice B2b (S-244-series): MCP upstream drift state.
	// MCPDriftPending is the raw mcp_drift_pending column: a JSON array
	// of tool names newly-added upstream that WOULD match the resource's
	// current tools_allow (only wildcards can newly match). Such tools
	// are denied until an admin re-approves: on drift the CP expands the
	// newly-matching wildcard into the concrete old-snapshot names, so
	// the gateway's effective allowlist excludes the pending tool, and
	// the pending set persists as the review marker until
	// approve-tools replaces the allowlist and clears it.
	// MCPDriftCheckedAt is the last drift check (on-demand or scan).
	// Drift is the computed view surfaced on GET/list (never persisted).
	MCPDriftPending   json.RawMessage `json:"-"`
	MCPDriftCheckedAt *time.Time      `json:"-"`
	Drift             *MCPDriftState  `json:"drift,omitempty"`
}

// MCPDriftState is the computed drift view surfaced on mcp resources
// (TG-5 slice B2b): pending = tool names denied until re-approved,
// last_checked_at = last drift check, drifted = pending is non-empty.
type MCPDriftState struct {
	Pending       []string   `json:"pending"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	Drifted       bool       `json:"drifted"`
}

// AgentPermission grants an agent access to a registry resource (Layer-2 RBAC,
// squad-owner managed). Constraints (TG-2) narrow the resource's
// policy_ceiling for this agent; they must satisfy the no-escalation
// invariant (egresspolicy.ValidateGrant) before being written.
type AgentPermission struct {
	ID           string          `json:"id"`
	AgentID      string          `json:"agent_id"`
	ResourceType ResourceType    `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	GrantedBy    string          `json:"granted_by"`
	CreatedAt    time.Time       `json:"created_at"`
	Constraints  json.RawMessage `json:"constraints,omitempty"`
}

// ── TG-8 slice B: grant-request workflow (docs/tg8-grant-approvals-spec.md §B) ──

// GrantRequestState is the workflow state of a grant request. Legal
// transitions: pending_owner → pending_admin → approved | denied, plus
// pending_owner → approved (non-high tiers) and * → denied from any
// pending state. approved/denied are terminal.
type GrantRequestState string

const (
	GrantRequestPendingOwner GrantRequestState = "pending_owner"
	GrantRequestPendingAdmin GrantRequestState = "pending_admin"
	GrantRequestApproved     GrantRequestState = "approved"
	GrantRequestDenied       GrantRequestState = "denied"
)

// GrantLintFinding mirrors grantlint.Finding (§A pinned interface). Kept
// in domain so storage, httpapi and the Inbox UI share one shape without
// importing the linter package.
type GrantLintFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // "block" | "warn"
	Detail   string `json:"detail"`
}

// FindingsHaveBlock reports whether any finding is block-severity — the
// signal that disables auto-approval (spec §B: block ALWAYS forces review).
func FindingsHaveBlock(findings []GrantLintFinding) bool {
	for _, f := range findings {
		if f.Severity == "block" {
			return true
		}
	}
	return false
}

// GrantRequest is a workflow record for a proposed agent→resource grant.
// Requests are workflow only; approval materializes the effective grant
// row (agent_permissions) — invariant 2 of the gateway design.
type GrantRequest struct {
	ID                string             `json:"id"`
	ResourceID        string             `json:"resource_id"`
	AgentID           string             `json:"agent_id,omitempty"`
	RequesterUserID   string             `json:"requester_user_id"`
	Tier              string             `json:"tier"`
	State             GrantRequestState  `json:"state"`
	RequestedScope    json.RawMessage    `json:"requested_scope,omitempty"`
	Findings          []GrantLintFinding `json:"findings"`
	ApprovedByOwnerAt *time.Time         `json:"approved_by_owner_at,omitempty"`
	ApprovedByAdminAt *time.Time         `json:"approved_by_admin_at,omitempty"`
	DeniedReason      string             `json:"denied_reason,omitempty"`
	Expiry            *time.Time         `json:"expiry,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at,omitempty"`
}

// ── TG-8 slice C: confirmation gates + standing grants (docs/tg8-grant-approvals-spec.md §C) ──

// ConfirmationState is the state of a gated-call confirmation. Legal
// transitions: pending → approved_once | approved_standing | denied,
// plus approved_once → expired at consume time when the 15-minute TTL
// has passed. denied/expired are terminal.
type ConfirmationState string

const (
	ConfirmationPending          ConfirmationState = "pending"
	ConfirmationApprovedOnce     ConfirmationState = "approved_once"
	ConfirmationApprovedStanding ConfirmationState = "approved_standing"
	ConfirmationDenied           ConfirmationState = "denied"
	ConfirmationExpired          ConfirmationState = "expired"
)

// PendingConfirmation is one gated tool call awaiting (or having received)
// the resource owner's decision. The row is bound to the exact call via
// args_hash; approvals never cover a different argument set.
type PendingConfirmation struct {
	ID         string            `json:"id"`
	ResourceID string            `json:"resource_id"`
	AgentID    string            `json:"agent_id"`
	Tool       string            `json:"tool"`
	ArgsHash   string            `json:"args_hash"`
	State      ConfirmationState `json:"state"`
	// InboxMessageID links the owner's action_required inbox message.
	InboxMessageID string `json:"inbox_message_id,omitempty"`
	// RequestedBy is the resource owner's user id resolved at request
	// time (the authority for this confirmation).
	RequestedBy  string     `json:"requested_by"`
	DeniedReason string     `json:"denied_reason,omitempty"`
	ApprovedAt   *time.Time `json:"approved_at,omitempty"`
	ConsumedAt   *time.Time `json:"consumed_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at,omitempty"`
}

// StandingGrant is a persistent "approve this and future" decision: the
// resource owner pre-authorizes (agent, tool) calls until expires_at.
// Revoke is soft (revoked_at set) so the row survives as audit trail;
// the live uniqueness index guarantees at most one live row per
// (resource, agent, tool) — invariant 2: revocation is immediately
// effective because the live row is gone.
type StandingGrant struct {
	ID         string     `json:"id"`
	ResourceID string     `json:"resource_id"`
	AgentID    string     `json:"agent_id"` // specific agent for v1; '*' literal reserved for future all-agent grants
	Tool       string     `json:"tool"`
	ExpiresAt  time.Time  `json:"expires_at"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
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

// MeteringDailyRow is a per-day aggregate of metering events for one
// (squad, agent, provider, model) tuple (S-190 dashboard histograms).
// Day is the UTC calendar day in YYYY-MM-DD form. Names are joined in at
// query time so the dashboard can label series without extra round-trips.
type MeteringDailyRow struct {
	Day          string  `json:"day"`
	SquadID      string  `json:"squad_id"`
	SquadName    string  `json:"squad_name"`
	AgentID      string  `json:"agent_id"`
	AgentName    string  `json:"agent_name"`
	ProviderID   string  `json:"provider_id"`
	ProviderName string  `json:"provider_name"`
	Model        string  `json:"model"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
	Currency     string  `json:"currency"`
}

// MeteringEvent records token usage for an agent (and squad) LLM call.
//
// WP5 (ADR-0010 D8 + Risk 3): ModelUsed names the model that ACTUALLY
// served the call (which may differ from the requested Model when the
// gateway fell back), and the Rate* fields snapshot the per-1M pricing
// used at event time together with the cost computed from that snapshot.
// Historical cost must never be re-derived by joining to live pricing —
// vendors change prices and the history would silently rewrite itself.
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
	// ModelUsed is the model that actually served the turn. Empty means the
	// reporter could not tell us; consumers fall back to Model.
	ModelUsed string `json:"model_used,omitempty"`
	// Rate snapshot (per 1M tokens), captured from the serving AI Model's
	// pricing at event time. Nil rates mean no snapshot was resolvable and
	// Cost is the reporter-supplied value.
	RateInputPer1M       *float64 `json:"rate_input_per_1m,omitempty"`
	RateCachedInputPer1M *float64 `json:"rate_cached_input_per_1m,omitempty"`
	RateCacheWritePer1M  *float64 `json:"rate_cache_write_per_1m,omitempty"`
	RateOutputPer1M      *float64 `json:"rate_output_per_1m,omitempty"`
	// RateSnapshot is true when the rates above were resolved from the AI
	// Model registry and Cost was computed from them.
	RateSnapshot bool `json:"rate_snapshot,omitempty"`
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
	ID        string `json:"id"`
	ActorType string `json:"actor_type"` // "user" | "agent" | "system"
	ActorID   string `json:"actor_id"`
	// ActorDisplay (S-235) is the actor's display name resolved at read
	// time (current agent name; user first name with email local-part
	// fallback). Empty for system actors and unresolvable ids. Additive
	// wire field — see Message.FromDisplay.
	ActorDisplay string          `json:"actor_display,omitempty"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	SquadID      string          `json:"squad_id,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
	Timestamp    time.Time       `json:"timestamp"`
}

// Prompt tier scopes (S-PROMPT WP2, ADR-0011). The platform tier is not
// stored here: it is embedded in the binary / operator-file overridden at
// deploy time, so it never appears in prompt_revisions.
const (
	PromptScopeOrganization = "organization"
	PromptScopeSquad        = "squad"
	PromptScopeAgent        = "agent"
)

// ValidPromptScope reports whether scope is a stored prompt tier.
func ValidPromptScope(scope string) bool {
	switch scope {
	case PromptScopeOrganization, PromptScopeSquad, PromptScopeAgent:
		return true
	}
	return false
}

// PromptTemplate is an admin-managed reusable starting point for squad or
// agent system prompts (S-158). Selecting a template COPIES its content
// into the new squad/agent prompt field — templates are authoring aids,
// not live references, so editing a template never rewrites existing
// squads or agents.
type PromptTemplate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	// AppliesTo is one of PromptTemplateAppliesSquad / ...Agent / ...Both.
	AppliesTo string    `json:"applies_to"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	PromptTemplateAppliesSquad = "squad"
	PromptTemplateAppliesAgent = "agent"
	PromptTemplateAppliesBoth  = "both"
)

// ValidPromptTemplateAppliesTo reports whether appliesTo is a known value.
func ValidPromptTemplateAppliesTo(appliesTo string) bool {
	switch appliesTo {
	case PromptTemplateAppliesSquad, PromptTemplateAppliesAgent, PromptTemplateAppliesBoth:
		return true
	}
	return false
}

// PlatformSettingIdleScaleToZeroSeconds is the platform_settings key
// (S-183) holding how many seconds an agent must stay idle — no task
// running, no chat turn in progress, no pending inbox work — before the
// operator scales its Deployment to zero. Seeded at 900 (15 minutes)
// by migration 0026; platform admins change it from the Settings screen.
const PlatformSettingIdleScaleToZeroSeconds = "idle_scale_to_zero_seconds"

// PlatformSettingEmbedderRuntime is the platform_settings key (S-212,
// ADR-0013 §4) holding the platform admin's embedder runtime choice:
// "auto" | "cuda" | "vulkan" | "cpu". The Settings screen writes it
// here AND mirrors it into the ConfigMap the operator reads
// (kube.EmbedderConfigStore); unset means "auto".
const PlatformSettingEmbedderRuntime = "embedder_runtime"

// InstanceSettings is the single-row organization-level configuration
// (layer 2 of the prompt hierarchy). Seeded by migration 0016.
type InstanceSettings struct {
	OrgName   string    `json:"org_name"`
	OrgPrompt string    `json:"org_prompt"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// Upload (S-194) is an image a user attached in the web UI — chat
// composer or task thread. Bytes live in Postgres (bytea) for now; the
// long-term home is object storage (see the S-194 report). The payload is
// deliberately excluded from JSON (`json:"-"`): API responses carry the
// metadata + URL, never the raw bytes.
type Upload struct {
	ID          string    `json:"id"`
	SquadID     string    `json:"squad_id"`
	UploaderID  string    `json:"uploader_id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	Data        []byte    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

// PromptRevision is one append-only entry in the prompt revision history.
// Retention is forever (ADR-0011 D5): rows are never updated or deleted.
type PromptRevision struct {
	ID      string    `json:"id"`
	Scope   string    `json:"scope"`
	ScopeID string    `json:"scope_id"`
	Content string    `json:"content"`
	Tokens  int       `json:"tokens"`
	SHA256  string    `json:"sha256"`
	SavedBy string    `json:"saved_by"`
	SavedAt time.Time `json:"saved_at"`
}
