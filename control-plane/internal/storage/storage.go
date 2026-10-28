// Package storage defines the persistence interfaces for the control plane and
// a Postgres implementation. The interfaces keep the httpapi and authz layers
// testable with fakes.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ErrDependencyCycle (S-278) rejects a depends-on edge that would close
// a cycle in the dependency graph (checked transitively at insert time).
var ErrDependencyCycle = fmt.Errorf("%w: dependency cycle", ErrConflict)

// ErrDependencyExists (S-278) rejects a duplicate depends-on edge.
var ErrDependencyExists = fmt.Errorf("%w: dependency already exists", ErrConflict)

// ErrReopenCapExceeded (S-290, ADR-0016 D3) is returned by
// ReopenTaskExecution when the task already carries maxReopens
// agent-reopen attempts in the rolling 24h window. Maps to HTTP 429.
var ErrReopenCapExceeded = fmt.Errorf("%w: reopen cap exceeded", ErrConflict)

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
	GrantRequestStore
	ConfirmationStore
	MeteringStore
	WakeLatencyStore
	AuditStore
	WorkNotificationStore
	PromptTierStore
	PromptTemplateStore
	BuiltinToolStore
	ScheduledJobStore
	UploadStore
	BudgetStore
	DriftStore
}

// DriftStore persists artifact drift-check reports (TG-11 §6.7,
// migration 0052 drift_reports). Rows are an append-only history: every
// check run inserts one row. The daily owner-digest dedup reads through
// CountDriftReports rather than a row-level uniqueness constraint.
type DriftStore interface {
	CreateDriftReport(ctx context.Context, r *domain.DriftReport) (*domain.DriftReport, error)
	// CountDriftReports counts reports for one resource checked at or
	// after `since`. When driftedOnly is true only in_sync=false rows
	// count — the batch-window probe used before filing a digest.
	CountDriftReports(ctx context.Context, resourceID string, since time.Time, driftedOnly bool) (int, error)
}

// ScheduledJobStore persists S-281 cron scheduled jobs. Claiming is
// atomic by contract: ClaimDueScheduledJobs must hand each due job to
// exactly one claimant (Postgres FOR UPDATE SKIP LOCKED; the memory
// store under its mutex) and bump next_run_at to a safety-net value so
// a crashed worker's job is retried rather than lost (at-least-once
// firing; the post-run update pins the real next run).
type ScheduledJobStore interface {
	CreateScheduledJob(ctx context.Context, job *domain.ScheduledJob) (*domain.ScheduledJob, error)
	GetScheduledJob(ctx context.Context, id string) (*domain.ScheduledJob, error)
	// ListScheduledJobs returns the agent's jobs (enabled and disabled)
	// in creation order.
	ListScheduledJobs(ctx context.Context, agentID string) ([]*domain.ScheduledJob, error)
	DeleteScheduledJob(ctx context.Context, id string) error
	// ClaimDueScheduledJobs atomically claims up to `limit` enabled jobs
	// with next_run_at <= now, marking them last_run_at=now /
	// last_run_status='running' and pushing next_run_at to
	// now+claimSafetyNet until FinishScheduledJobRun / FailScheduledJobRun
	// pins the real outcome.
	ClaimDueScheduledJobs(ctx context.Context, now time.Time, limit int, claimSafetyNet time.Duration) ([]*domain.ScheduledJob, error)
	// FinishScheduledJobRun records a successful fire: last_run_at,
	// last_run_status, fail_count reset to 0, the computed next_run_at,
	// and enabled (false for one-shot 'at' jobs).
	FinishScheduledJobRun(ctx context.Context, id string, firedAt time.Time, nextRunAt time.Time, enabled bool) error
	// FailScheduledJobRun records a failed fire: fail_count++ and, once
	// fail_count reaches maxFailures, enabled=false with disabled_reason.
	FailScheduledJobRun(ctx context.Context, id string, firedAt time.Time, maxFailures int, reason string) error
	// SetScheduledJobEnabled pauses (false) or resumes (true) a job.
	// A non-zero nextRunAt overwrites next_run_at (resume recomputes so
	// missed intervals are not replayed); zero keeps the stored value.
	SetScheduledJobEnabled(ctx context.Context, id string, enabled bool, nextRunAt time.Time) (*domain.ScheduledJob, error)
}

// UploadStore persists image uploads (S-194). Uploads are squad-scoped:
// the HTTP layer authorizes reads/writes against the upload's squad.
type UploadStore interface {
	CreateUpload(ctx context.Context, u *domain.Upload) (*domain.Upload, error)
	GetUpload(ctx context.Context, uploadID string) (*domain.Upload, error)
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
	// BulkDeletePromptTemplates (S-214) removes many templates in one
	// atomic operation and returns how many rows actually existed and were
	// deleted. Unknown ids are skipped silently; an empty id list returns
	// ErrInvalidInput.
	BulkDeletePromptTemplates(ctx context.Context, ids []string) (int, error)
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
	// GetBoardPolicy returns the raw squad board-policy document
	// (ADR-0016 D8). Effective values are resolved in the domain layer.
	GetBoardPolicy(ctx context.Context, squadID string) (domain.BoardPolicy, error)
	// UpdateBoardPolicy replaces the whole board-policy document
	// (S-289). Pending same-transaction audits are drained with the update.
	UpdateBoardPolicy(ctx context.Context, squadID string, policy domain.BoardPolicy) (*domain.Squad, error)
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
	// ReorderTasks (S-304) assigns sequential positions (1..n) to the
	// given task ids within one board column. Every id must exist, belong
	// to boardID, and currently carry the given status; the whole column
	// is renumbered atomically so drag-and-drop order becomes the pickup
	// priority (ClaimNextTask orders todos by position).
	ReorderTasks(ctx context.Context, boardID string, status domain.TaskStatus, orderedIDs []string) error
	ListTasks(ctx context.Context, boardID string, status domain.TaskStatus) ([]*domain.Task, error) // status "" = all
	ListAgentTasks(ctx context.Context, agentID string) ([]*domain.Task, error)
	ClaimNextTask(ctx context.Context, agentID string, workerID string, leaseFor time.Duration) (*domain.Task, error)
	// StartTaskExecution mints a new active, fenced execution attempt for
	// (agent, task) — the board.claim path (S-292, ADR-0016 D1) where no
	// prior claim created the execution. Transition legality (pickup
	// column, assignee) is validated by the caller; this only creates the
	// fence. ErrConflict when the agent already holds a live execution on
	// a different task (one live attempt per agent, matching the claim
	// posture) or the task does not exist (ErrNotFound).
	StartTaskExecution(ctx context.Context, agentID string, taskID string, workerID string, leaseFor time.Duration) (*domain.TaskExecution, error)
	// AddTaskDependency (S-278) records that taskID depends on
	// dependsOnID. Both tasks must exist, belong to the same squad, must
	// not be the same task, and the edge must not create a cycle.
	AddTaskDependency(ctx context.Context, taskID string, dependsOnID string, createdByType string, createdByID string) (*domain.TaskDependencyRef, error)
	// RemoveTaskDependency drops one depends-on edge.
	RemoveTaskDependency(ctx context.Context, taskID string, dependsOnID string) error
	// ListTaskDependencies returns the depends-on + dependents views.
	ListTaskDependencies(ctx context.Context, taskID string) (*domain.TaskDependencies, error)
	// UnsatisfiedDependencies returns the depends-on tasks that are not
	// done — the blocking set for agent pickup (S-278).
	UnsatisfiedDependencies(ctx context.Context, taskID string) ([]domain.TaskDependencyRef, error)
	// BlockedCountsByBoard (S-278) returns per-task counts of unfinished
	// depends-on tasks for one board — one query for the board badge.
	BlockedCountsByBoard(ctx context.Context, boardID string) (map[string]int, error)
	// SearchSquadTasks (S-278) finds tasks in a squad for the
	// dependency picker: q matches title (case-insensitive substring)
	// or the "T-<n>"/"<n>" task-number reference. Empty q returns the
	// most recently updated tasks.
	SearchSquadTasks(ctx context.Context, squadID string, q string, limit int) ([]*domain.Task, error)
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
	// SaveSubagentThreadSnapshot replaces the captured task-subagent thread
	// on the task's active execution row (S-301). Returns ErrNotFound when
	// no active execution exists (the run already ended).
	SaveSubagentThreadSnapshot(ctx context.Context, taskID string, snapshot []byte) error
	// GetSubagentThread returns the snapshot carried by the task's latest
	// execution row, whether that execution is still active, and when it was
	// last written (S-301 chat task-card Details panel).
	GetSubagentThread(ctx context.Context, taskID string) (snapshot []byte, updatedAt time.Time, running bool, err error)
	// ListTaskExecutions returns every execution attempt for a task,
	// oldest first (S-290: reopen retains prior attempts; the read path
	// backs the visible attempt count).
	ListTaskExecutions(ctx context.Context, taskID string) ([]*domain.TaskExecution, error)
	CompleteTaskExecution(ctx context.Context, agentID string, taskID string, executionID string, fencingToken string, status domain.TaskStatus, summary string) (*domain.Task, error)
	// ReopenTaskExecution implements the agent reopen (ADR-0016 D3, S-290):
	// the assignee moves its own in-review task back to in-progress with a
	// NEW active execution attempt (new fencing token, lease), retaining the
	// prior completed attempt. reviewerFeedback is persisted on the new
	// attempt and surfaced on the task payload. maxReopens is the resolved
	// rolling-24h cap on reopen-created attempts for this task; exceeding
	// it returns ErrReopenCapExceeded and applies nothing. ErrConflict when
	// the task is not in-review, the agent already holds a live execution,
	// or the assignee does not match.
	ReopenTaskExecution(ctx context.Context, agentID string, taskID string, workerID string, reviewerFeedback string, leaseFor time.Duration, maxReopens int) (*domain.Task, error)
	// ReapExpiredTaskExecutions marks active executions whose lease expired
	// before cutoff as expired and re-queues their tasks (in-progress → TO DO)
	// when no other live execution remains. Returns the reaped executions
	// (S-193: the reaper notifies squad owners about dead attempts) so the
	// caller can identify what died without a second sweep. Safe to run
	// concurrently: the updates are conditional, so a heartbeat or complete
	// that lands after cutoff wins and the row is left untouched.
	ReapExpiredTaskExecutions(ctx context.Context, cutoff time.Time) ([]domain.ReapedExecution, error)
	// ListStaleInProgressTasks (S-197) returns in-progress tasks with no
	// sign of life since cutoff: the task row itself untouched, no task
	// thread message (messages.payload->>'task_id') arrived, and no
	// execution heartbeat (task_executions.updated_at) landed. The stuck
	// scanner alerts squad owners about exactly these tasks.
	ListStaleInProgressTasks(ctx context.Context, cutoff time.Time) ([]*domain.Task, error)
}

// AgentMemoryStore persists bounded agent long-term memory.
type AgentMemoryStore interface {
	CreateAgentMemory(ctx context.Context, memory *domain.AgentMemory) (*domain.AgentMemory, error)
	ListAgentMemory(ctx context.Context, agentID string, squadID string, queryEmbedding []float64, limit int) ([]*domain.AgentMemory, error)
	// SearchAgentMemory (S-212) ranks the agent's own memories by cosine
	// similarity to queryEmbedding. Trust gating: rows with
	// review_status='rejected' and rows without an embedding are excluded;
	// pending_review/approved rows across all trust levels
	// (raw_model_output/distilled/verified) are recallable. Score is
	// cosine similarity in [-1, 1] (1 = identical direction).
	SearchAgentMemory(ctx context.Context, agentID string, squadID string, queryEmbedding []float64, limit int) ([]MemorySearchHit, error)
}

// MemorySearchHit is one ranked result of SearchAgentMemory.
type MemorySearchHit struct {
	Memory *domain.AgentMemory
	Score  float64
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
	// S-212: the transcript is embedded at write time — embedding +
	// embeddingModel carry the generated vector and the model that
	// produced it (empty embedding = embeddings disabled/failed).
	ResetAgentChat(ctx context.Context, agentID, squadID, transcript string, metadata json.RawMessage, embedding []float64, embeddingModel string) (int, time.Time, error)
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
	// ListInboxPage returns one page (newest first) of the user's inbox
	// plus the TOTAL number of messages matching the filter, so paged
	// clients render a pager without a second round-trip (S-239).
	ListInboxPage(ctx context.Context, userID string, unreadOnly bool, limit, offset int) ([]*domain.InboxMessage, int, error)
	MarkInboxMessageRead(ctx context.Context, userID string, id string) (*domain.InboxMessage, error)
	// GetInboxMessage fetches one message by id (S-193: delete
	// authorization needs the recipient before removal).
	GetInboxMessage(ctx context.Context, id string) (*domain.InboxMessage, error)
	// DeleteInboxMessage removes a message permanently (S-193: inbox
	// messages are never auto-removed — only explicit user delete). When
	// userID is empty the user scoping is skipped: the handler has already
	// enforced platform-admin for that path.
	DeleteInboxMessage(ctx context.Context, id string, userID string) error
	// CreateInboxAttachment stores one S-216 attachment (bytes +
	// server-computed metadata) against an existing inbox message.
	CreateInboxAttachment(ctx context.Context, a *domain.InboxAttachment) (*domain.InboxAttachment, error)
	// GetInboxAttachment fetches one attachment including its bytes
	// (download path).
	GetInboxAttachment(ctx context.Context, id string) (*domain.InboxAttachment, error)
	// ListInboxAttachmentMeta batch-fetches attachment metadata (no
	// bytes) for a set of messages, grouped by message id — keeps the
	// inbox list free of N+1 queries.
	ListInboxAttachmentMeta(ctx context.Context, messageIDs []string) (map[string][]domain.InboxAttachment, error)
}

// NotificationStore persists recipient-scoped "something went wrong"
// alerts for the top-bar bell (S-193). Separate from the inbox by design.
type NotificationStore interface {
	CreateNotification(ctx context.Context, n *domain.Notification) (*domain.Notification, error)
	ListNotifications(ctx context.Context, userID string, unreadOnly bool, limit int) ([]*domain.Notification, error)
	MarkNotificationRead(ctx context.Context, userID string, id string) (*domain.Notification, error)
	MarkAllNotificationsRead(ctx context.Context, userID string) (int, error)
	// GetNotificationPreferences returns the user's mute list (S-199).
	// No stored row ⇒ empty MutedTypes (all types enabled by default).
	GetNotificationPreferences(ctx context.Context, userID string) (*domain.NotificationPreferences, error)
	// SetNotificationPreferences upserts the user's mute list (S-199).
	SetNotificationPreferences(ctx context.Context, userID string, muted []domain.NotificationType) (*domain.NotificationPreferences, error)
	// HasRecentNotificationForTask (S-197) reports whether a notification
	// of the given type already exists for the task with created_at at or
	// after since. The stuck scanner uses it to cap alerts at one per
	// task per window without a new column on tasks.
	HasRecentNotificationForTask(ctx context.Context, taskID string, typ domain.NotificationType, since time.Time) (bool, error)
	// DeleteReadNotificationsBefore (S-198) permanently removes READ
	// notifications created before cutoff and returns the number purged.
	// Unread notifications are never touched, and inbox messages are
	// never auto-removed (S-193: explicit user delete only).
	DeleteReadNotificationsBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// WorkNotificationStore lets runtimes wait for assigned task or inbox changes
// without polling the control plane on a fixed interval.
type WorkNotificationStore interface {
	WaitForAgentWork(ctx context.Context, agentID string, timeout time.Duration) (bool, error)
}

// RegistryStore persists registry resources (AI providers + generic resources).
type RegistryStore interface {
	CreateAIProvider(ctx context.Context, p *domain.AIProvider) (*domain.AIProvider, error)
	GetAIProvider(ctx context.Context, id string) (*domain.AIProvider, error)
	UpdateAIProvider(ctx context.Context, p *domain.AIProvider) (*domain.AIProvider, error)
	DeprecateAIProvider(ctx context.Context, id string) error
	DeleteAIProvider(ctx context.Context, id string) error
	ListAIProviders(ctx context.Context) ([]*domain.AIProvider, error)

	CreateResource(ctx context.Context, r *domain.RegistryResource) (*domain.RegistryResource, error)
	GetResource(ctx context.Context, typ domain.ResourceType, id string) (*domain.RegistryResource, error)
	// GetResourceByID resolves a registry resource by id alone (TG-4: the
	// gateway credentials API addresses resources by id, not type+id).
	GetResourceByID(ctx context.Context, id string) (*domain.RegistryResource, error)
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
	// SetAIModelGatewayKeyFingerprint persists the fingerprint of the
	// provider key last pushed to the model's gateway deployment, so
	// reconcile can detect provider-key drift (S-303).
	SetAIModelGatewayKeyFingerprint(ctx context.Context, id, fingerprint string) error
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

// GrantRequestStore persists the TG-8 grant-request workflow (spec §B).
// Requests are workflow records; approval materializes the effective
// grant via PermissionStore — requests never become the effective artifact.
type GrantRequestStore interface {
	CreateGrantRequest(ctx context.Context, r *domain.GrantRequest) (*domain.GrantRequest, error)
	GetGrantRequest(ctx context.Context, id string) (*domain.GrantRequest, error)
	// ListGrantRequests filters by any combination of state, requester, and
	// resource owner (OwnerUserID matches registry_resources.owner_user_id).
	// Empty filter fields mean "no constraint on that axis".
	ListGrantRequests(ctx context.Context, f GrantRequestFilter) ([]*domain.GrantRequest, error)
	// UpdateGrantRequestState applies a workflow transition guarded by the
	// expected current state: if the row moved under the caller (concurrent
	// decision), it returns ErrConflict and nothing is written.
	UpdateGrantRequestState(ctx context.Context, id string, expectedFrom domain.GrantRequestState, up GrantRequestUpdate) (*domain.GrantRequest, error)
}

// GrantRequestFilter narrows ListGrantRequests. Zero value = all requests.
type GrantRequestFilter struct {
	State           string
	RequesterUserID string
	OwnerUserID     string
}

// GrantRequestUpdate carries the mutable fields of one transition.
// Timestamps are applied only when non-nil; DeniedReason is written as
// given (only the deny transition carries one).
type GrantRequestUpdate struct {
	State             domain.GrantRequestState
	ApprovedByOwnerAt *time.Time
	ApprovedByAdminAt *time.Time
	DeniedReason      string
}

// ConfirmationStore persists the TG-8 slice C confirmation gates and
// standing grants (docs/tg8-grant-approvals-spec.md §C). Same
// guarded-transition contract as GrantRequestStore: expectedFrom
// mismatch ⇒ ErrConflict, nothing written (race-safe double-decision).
type ConfirmationStore interface {
	CreatePendingConfirmation(ctx context.Context, c *domain.PendingConfirmation) (*domain.PendingConfirmation, error)
	GetPendingConfirmation(ctx context.Context, id string) (*domain.PendingConfirmation, error)
	// ListPendingConfirmations filters by any combination of state,
	// requesting owner, and resource. Empty filter fields mean "no
	// constraint on that axis".
	ListPendingConfirmations(ctx context.Context, f ConfirmationFilter) ([]*domain.PendingConfirmation, error)
	// UpdateConfirmationState applies a state transition guarded by the
	// expected current state; concurrent decision ⇒ ErrConflict.
	UpdateConfirmationState(ctx context.Context, id string, expectedFrom domain.ConfirmationState, up ConfirmationUpdate) (*domain.PendingConfirmation, error)

	// FindLiveStandingGrant returns the live (not revoked, not expired
	// as of `now`) standing grant for exactly (resource, agent, tool),
	// or ErrNotFound. v1 matches the agent id exactly — the '*' reserved
	// literal never matches.
	FindLiveStandingGrant(ctx context.Context, resourceID, agentID, tool string, now time.Time) (*domain.StandingGrant, error)
	// UpsertStandingGrant creates the live grant or, when one already
	// exists for (resource, agent, tool), refreshes expires_at and
	// created_by in place (the live-row uniqueness invariant).
	UpsertStandingGrant(ctx context.Context, g *domain.StandingGrant) (*domain.StandingGrant, error)
	// ListStandingGrants filters by resource owner (OwnerUserID matches
	// registry_resources.owner_user_id). Revoked grants are excluded
	// unless IncludeRevoked is set.
	ListStandingGrants(ctx context.Context, f StandingGrantFilter) ([]*domain.StandingGrant, error)
	// GetStandingGrant resolves a grant by id alone (authz before revoke).
	GetStandingGrant(ctx context.Context, id string) (*domain.StandingGrant, error)
	// RevokeStandingGrant soft-deletes the live row (sets revoked_at).
	// Missing or already-revoked ⇒ ErrNotFound.
	RevokeStandingGrant(ctx context.Context, id string, at time.Time) (*domain.StandingGrant, error)
}

// ConfirmationFilter narrows ListPendingConfirmations. Zero value = all.
type ConfirmationFilter struct {
	State       string
	RequestedBy string
	ResourceID  string
}

// ConfirmationUpdate carries the mutable fields of one confirmation
// transition. Timestamps/links apply only when non-nil; DeniedReason is
// written only when non-empty (deny transition).
type ConfirmationUpdate struct {
	State          domain.ConfirmationState
	InboxMessageID *string
	ApprovedAt     *time.Time
	ConsumedAt     *time.Time
	DeniedReason   string
}

// StandingGrantFilter narrows ListStandingGrants. Zero value = all live
// grants.
type StandingGrantFilter struct {
	OwnerUserID    string
	IncludeRevoked bool
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
	// SumMeteringDaily buckets metering into per-day aggregates per
	// (squad, agent, provider, model) for the dashboard histograms (S-190).
	// Events before `since` are excluded (zero since = all time). squadIDs
	// restricts the aggregation to an explicit allowlist; nil or empty
	// means "all squads" — auth-scoped callers must pass their visible
	// squad IDs and short-circuit when that list is empty.
	// Rows are sorted by day, then squad name, agent name, provider name,
	// model.
	SumMeteringDaily(ctx context.Context, since time.Time, squadIDs []string) ([]domain.MeteringDailyRow, error)
	// SumMeteringByResource aggregates per-resource usage for the
	// S-269 per-resource metering API. Only events carrying a resource
	// attribution are counted; resourceID / resourceType are optional
	// filters (empty = no filter). Usage (Calls) covers metered and
	// non-metered resources alike — cost is only charged when the
	// resource is metered, so a non-metered row's zero Cost is an
	// explicit billed zero, not missing data. Like SumMeteringDaily
	// this is a read-time rollup over the retained raw events
	// (S-203 retention alignment: no separate rollup table).
	SumMeteringByResource(ctx context.Context, since time.Time, resourceID, resourceType string) ([]domain.ResourceMeteringRow, error)
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
	// DeleteAuditByActionBefore (S-198) purges audit rows with the given
	// action recorded before cutoff and returns the number purged. It
	// exists so the retention sweep can clean up the audit rows it writes
	// itself (action='inbox.deleted') without touching the rest of the
	// append-only log.
	DeleteAuditByActionBefore(ctx context.Context, action string, cutoff time.Time) (int, error)
}
