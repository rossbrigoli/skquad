package httpapi

// S-281: built-in CRON / scheduled tasks.
//
// The scheduler is a background goroutine in the API server (same
// shape as the execution reaper / stuck-task scanner). Every tick it
// atomically claims due jobs (SKIP LOCKED on Postgres so N replicas
// never double-fire) and fires each one through the CANONICAL agent-run
// path — the exact mechanism delegate/handoff uses: materialize a task
// on the agent's board assigned to the agent, then mirror the agent to
// busy so the outbox/operator starts the pod and the runtime claims the
// task. The scheduled prompt is the task description; the agent turn
// happens exactly like any assigned-task turn.
//
// Firing semantics:
//   - one-shot ('at') jobs are disabled after a successful fire;
//   - 'every'/'cron' jobs get their next_run_at recomputed from the
//     fire instant;
//   - a fire that errors increments fail_count; after maxJobFailures
//     consecutive failures the job auto-disables with the reason
//     recorded (surfaced in `cron list`);
//   - the claim itself bumps next_run_at by a safety net, so a worker
//     crash mid-fire retries later instead of losing the job
//     (at-least-once; the task materialization is the idempotency
//     boundary an operator can see on the board).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	// DefaultScheduledJobTick is the scheduler poll interval (task
	// spec: 30s).
	DefaultScheduledJobTick = 30 * time.Second
	// scheduledJobClaimSafetyNet: how long a claimed-but-unfinished job
	// waits before it may be re-claimed after a worker crash.
	scheduledJobClaimSafetyNet = 5 * time.Minute
	// maxJobFailures: consecutive fire failures before auto-disable.
	maxJobFailures = 5
	// scheduledJobBatch caps jobs claimed per tick so one flood cannot
	// starve the loop.
	scheduledJobBatch = 50
	// defaultMaxActiveJobs is the cron policy cap when the admin policy
	// omits maxActiveJobs.
	defaultMaxActiveJobs = 10
	// maxScheduledJobTitle caps the optional task title.
	maxScheduledJobTitle = 200
)

// RunScheduledJobScheduler loops until ctx is cancelled, sweeping due
// scheduled jobs every interval. All failures are logged and never
// fatal — the loop must survive a bad sweep (same posture as the
// stuck-task scanner). Safe on every replica: the claim is atomic.
func RunScheduledJobScheduler(ctx context.Context, store Store, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultScheduledJobTick
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fired, err := TickScheduledJobsOnce(ctx, store, time.Now().UTC())
			if err != nil {
				slog.Warn("scheduled job sweep failed", "error", err)
				continue
			}
			if fired > 0 {
				slog.Info("fired scheduled jobs", "count", fired)
			}
		}
	}
}

// TickScheduledJobsOnce claims and fires everything due at `now`.
// Exported for deterministic tests; the loop above is just the clock.
// Returns the number of jobs fired successfully.
func TickScheduledJobsOnce(ctx context.Context, store Store, now time.Time) (int, error) {
	jobs, err := store.ClaimDueScheduledJobs(ctx, now, scheduledJobBatch, scheduledJobClaimSafetyNet)
	if err != nil {
		return 0, err
	}
	fired := 0
	for _, job := range jobs {
		if fireScheduledJob(ctx, store, job, now) {
			fired++
		}
	}
	return fired, nil
}

// fireScheduledJob fires one already-claimed job and records the
// outcome. Returns true on success. Never returns an error to the
// loop: each job's failure is recorded on the job row instead.
func fireScheduledJob(ctx context.Context, store Store, job *domain.ScheduledJob, firedAt time.Time) bool {
	if err := deliverScheduledJob(ctx, store, job, firedAt); err != nil {
		slog.Warn("scheduled job fire failed", "job", job.ID, "agent", job.AgentID, "error", err)
		reason := fmt.Sprintf("auto-disabled after %d consecutive failures: %v", maxJobFailures, err)
		if ferr := store.FailScheduledJobRun(ctx, job.ID, firedAt, maxJobFailures, reason); ferr != nil {
			slog.Error("recording scheduled job failure", "job", job.ID, "error", ferr)
		}
		return false
	}
	enabled := true
	nextRun := job.NextRunAt
	if job.ScheduleKind == domain.ScheduleAt {
		enabled = false // one-shot: never fires again
	} else {
		next, err := domain.ComputeNextRun(job.ScheduleKind, job.ScheduleSpec, firedAt)
		if err != nil {
			// Validated at creation; a failure here means the row was
			// corrupted out from under us. Treat as a fire failure.
			slog.Warn("scheduled job next-run recompute failed", "job", job.ID, "error", err)
			reason := fmt.Sprintf("auto-disabled after %d consecutive failures: %v", maxJobFailures, err)
			if ferr := store.FailScheduledJobRun(ctx, job.ID, firedAt, maxJobFailures, reason); ferr != nil {
				slog.Error("recording scheduled job failure", "job", job.ID, "error", ferr)
			}
			return false
		}
		nextRun = next
	}
	if err := store.FinishScheduledJobRun(ctx, job.ID, firedAt, nextRun, enabled); err != nil {
		slog.Error("recording scheduled job success", "job", job.ID, "error", err)
		return false
	}
	return true
}

// deliverScheduledJob runs the canonical wake sequence for one fire:
// budget guard → materialize the task → mirror the agent busy. Kept
// separate from the bookkeeping so tests can exercise the exact path.
func deliverScheduledJob(ctx context.Context, store Store, job *domain.ScheduledJob, firedAt time.Time) error {
	// S-203 posture: a budget-blocked owner's scheduled work must not
	// start. Read the block state directly from the store (the same
	// check setAgentStatusAndMirror applies to live wake paths).
	if blocked, err := store.IsAgentBudgetBlocked(ctx, job.AgentID, currentBudgetPeriod(firedAt)); err == nil && blocked {
		return errors.New(budgetStopNotice)
	}
	agent, err := store.GetAgent(ctx, job.AgentID)
	if err != nil {
		return fmt.Errorf("agent %s unavailable: %w", job.AgentID, err)
	}
	board, err := store.GetBoard(ctx, job.SquadID)
	if err != nil {
		return fmt.Errorf("board for squad %s unavailable: %w", job.SquadID, err)
	}
	task := &domain.Task{
		BoardID:         board.ID,
		SquadID:         job.SquadID,
		Title:           scheduledTaskTitle(job),
		Description:     scheduledTaskDescription(job, firedAt),
		Status:          domain.TaskTodo,
		AssigneeAgentID: job.AgentID,
		CreatedByType:   job.CreatedByType,
		CreatedByID:     job.CreatedByID,
	}
	if _, err := store.CreateTask(ctx, task); err != nil {
		return fmt.Errorf("materialize scheduled task: %w", err)
	}
	// The busy mirror is what actually wakes the agent (outbox → operator
	// → pod start → runtime claim), identical to syncAgentStatusFromPendingWork's
	// busy branch. Pending work now exists, so busy is the honest state.
	if err := store.SetAgentStatus(ctx, agent.ID, domain.AgentBusy); err != nil {
		return fmt.Errorf("mirror agent busy: %w", err)
	}
	return nil
}

func scheduledTaskTitle(job *domain.ScheduledJob) string {
	title := strings.TrimSpace(job.Title)
	if title == "" {
		title = strings.TrimSpace(strings.SplitN(strings.TrimSpace(job.Message), "\n", 2)[0])
	}
	if title == "" {
		title = "Scheduled task"
	}
	return trimRunes(title, maxScheduledJobTitle)
}

func scheduledTaskDescription(job *domain.ScheduledJob, firedAt time.Time) string {
	body := strings.TrimSpace(job.Message)
	if body == "" {
		body = "(no prompt provided)"
	}
	origin := fmt.Sprintf("\n\n---\nScheduled job %s (kind=%s) fired at %s",
		job.ID, job.ScheduleKind, firedAt.UTC().Format(time.RFC3339))
	return trimRunes(body+origin, maxAgentMemoryContentChars)
}

// --- REST API (agent-scoped, authenticateAgent) --------------------------
//
// Mounted under /api/v1/agents/me/scheduled-jobs: every operation is
// implicitly scoped to the calling agent, which is exactly the S-281
// authz rule (an agent can only see and manage its own jobs). The
// runtime `cron` builtin is a thin client over these routes with the
// same bearer-credential auth as send_inbox/notify_owner.

type scheduledJobScheduleRequest struct {
	Kind            string `json:"kind"`
	At              string `json:"at,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	Expression      string `json:"expression,omitempty"`
	Timezone        string `json:"timezone,omitempty"`
}

type createScheduledJobRequest struct {
	Title    string                      `json:"title,omitempty"`
	Message  string                      `json:"message"`
	Schedule scheduledJobScheduleRequest `json:"schedule"`
}

func (s *Server) createScheduledJob(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	var req createScheduledJobRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	if len(message) > domain.MaxScheduledMessageChars {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("message exceeds %d characters", domain.MaxScheduledMessageChars))
		return
	}
	if !s.cronToolEnabled(w, r) {
		return
	}
	kind := domain.ScheduleKind(strings.TrimSpace(req.Schedule.Kind))
	spec, err := domain.BuildScheduleSpec(kind, req.Schedule.At, req.Schedule.IntervalSeconds, req.Schedule.Expression, req.Schedule.Timezone)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}
	now := time.Now().UTC()
	nextRun, err := domain.ComputeNextRun(kind, spec, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}
	if err := s.enforceCronJobCap(r, principal.Agent.ID); err != nil {
		writeError(w, http.StatusConflict, "too_many_active_jobs", err.Error())
		return
	}
	created, err := s.store.CreateScheduledJob(s.pendingAgentAuditCtx(r, principal.Agent.ID, "scheduled_job.create", "scheduled_job", "", principal.Agent.SquadID, nil), &domain.ScheduledJob{
		AgentID:       principal.Agent.ID,
		SquadID:       principal.Agent.SquadID,
		Title:         trimRunes(strings.TrimSpace(req.Title), maxScheduledJobTitle),
		Message:       message,
		ScheduleKind:  kind,
		ScheduleSpec:  spec,
		Enabled:       true,
		NextRunAt:     nextRun,
		CreatedByType: "agent",
		CreatedByID:   principal.Agent.ID,
	})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listScheduledJobs(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	jobs, err := s.store.ListScheduledJobs(r.Context(), principal.Agent.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// loadOwnScheduledJob resolves {jobID} and enforces the S-281 authz
// rule: the job must belong to the calling agent.
func (s *Server) loadOwnScheduledJob(w http.ResponseWriter, r *http.Request) (*domain.ScheduledJob, bool) {
	principal := currentAgent(r.Context())
	job, err := s.store.GetScheduledJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		writeStorageError(w, err)
		return nil, false
	}
	if job.AgentID != principal.Agent.ID {
		writeError(w, http.StatusForbidden, "forbidden", "scheduled jobs are scoped to the owning agent")
		return nil, false
	}
	return job, true
}

func (s *Server) getScheduledJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnScheduledJob(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) deleteScheduledJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnScheduledJob(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteScheduledJob(s.pendingAgentAuditCtx(r, currentAgent(r.Context()).Agent.ID, "scheduled_job.delete", "scheduled_job", job.ID, job.SquadID, nil), job.ID); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pauseScheduledJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnScheduledJob(w, r)
	if !ok {
		return
	}
	updated, err := s.store.SetScheduledJobEnabled(r.Context(), job.ID, false, time.Time{})
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) resumeScheduledJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnScheduledJob(w, r)
	if !ok {
		return
	}
	if err := s.enforceCronJobCap(r, job.AgentID); err != nil {
		writeError(w, http.StatusConflict, "too_many_active_jobs", err.Error())
		return
	}
	// Recompute the next run from NOW: a paused 'every' job must not
	// replay the interval it spent paused, and a 'cron' job resumes on
	// the next expression match.
	nextRun, err := domain.ComputeNextRun(job.ScheduleKind, job.ScheduleSpec, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}
	updated, err := s.store.SetScheduledJobEnabled(s.pendingAgentAuditCtx(r, currentAgent(r.Context()).Agent.ID, "scheduled_job.resume", "scheduled_job", job.ID, job.SquadID, nil), job.ID, true, nextRun)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// cronToolEnabled rejects job creation/resume when the platform admin
// has disabled the cron builtin outright.
func (s *Server) cronToolEnabled(w http.ResponseWriter, r *http.Request) bool {
	cfg, err := s.store.GetBuiltinTool(r.Context(), domain.BuiltinToolCron)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Not seeded: treat as disabled — the seed is part of the
			// feature's migration, an absent row means no cron here.
			writeError(w, http.StatusForbidden, "cron_disabled", "the cron built-in tool is not configured")
			return false
		}
		writeStorageError(w, err)
		return false
	}
	if !cfg.Enabled {
		writeError(w, http.StatusForbidden, "cron_disabled", "the cron built-in tool is disabled by platform policy")
		return false
	}
	return true
}

// enforceCronJobCap enforces the cron policy maxActiveJobs per agent
// (default 10 when the policy omits the key).
func (s *Server) enforceCronJobCap(r *http.Request, agentID string) error {
	maxActive := defaultMaxActiveJobs
	if cfg, err := s.store.GetBuiltinTool(r.Context(), domain.BuiltinToolCron); err == nil && len(cfg.Policy) > 0 {
		var policy struct {
			MaxActiveJobs *int `json:"maxActiveJobs"`
		}
		if err := json.Unmarshal(cfg.Policy, &policy); err == nil && policy.MaxActiveJobs != nil && *policy.MaxActiveJobs > 0 {
			maxActive = *policy.MaxActiveJobs
		}
	}
	jobs, err := s.store.ListScheduledJobs(r.Context(), agentID)
	if err != nil {
		return err
	}
	active := 0
	for _, job := range jobs {
		if job.Enabled {
			active++
		}
	}
	if active >= maxActive {
		return fmt.Errorf("agent already has %d active scheduled jobs (maxActiveJobs=%d); cancel or pause one first", active, maxActive)
	}
	return nil
}
