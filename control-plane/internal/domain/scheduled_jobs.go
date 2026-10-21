package domain

// S-281: built-in CRON / scheduled tasks.
//
// A ScheduledJob is the durable record of "wake this agent with this
// prompt at a future time / on an interval / on a cron expression".
// The scheduler (httpapi/scheduled_jobs.go) claims due jobs and fires
// them through the canonical agent-run path (materialized task + busy
// mirror, exactly like delegate/handoff). This file owns the shape, the
// schedule-spec validation and the next-run computation — the two
// stores persist whatever next_run_at the domain computes, they never
// invent schedule semantics of their own.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// ScheduleKind is how a job's fire times are derived.
type ScheduleKind string

const (
	// ScheduleAt fires once at a fixed RFC3339 instant.
	ScheduleAt ScheduleKind = "at"
	// ScheduleEvery fires every interval_seconds from the previous fire.
	ScheduleEvery ScheduleKind = "every"
	// ScheduleCron fires on a standard 5-field cron expression,
	// evaluated in an IANA timezone (default UTC).
	ScheduleCron ScheduleKind = "cron"
)

// Valid reports whether the kind is one of at/every/cron.
func (k ScheduleKind) Valid() bool {
	switch k {
	case ScheduleAt, ScheduleEvery, ScheduleCron:
		return true
	}
	return false
}

const (
	// MinEverySeconds / MaxEverySeconds bound ScheduleEvery. The floor
	// keeps a runaway "every 1s" cron from hammering the scheduler and
	// the agent-run path; the ceiling keeps a "yearly" job honest to the
	// 'every' contract (use 'at' for anything longer).
	MinEverySeconds = 60
	MaxEverySeconds = 90 * 24 * 60 * 60 // 90 days

	// DefaultScheduleTimezone is used for cron jobs with no explicit tz.
	DefaultScheduleTimezone = "UTC"

	// MaxScheduledMessageChars caps the scheduled prompt text, matching
	// the inbox message cap (S-229) so a cron payload can never smuggle
	// a novel into an agent's context.
	MaxScheduledMessageChars = 10000
)

// ScheduledJob is one row of the scheduled_jobs table (migration 0057).
type ScheduledJob struct {
	ID      string `json:"id"`
	AgentID string `json:"agent_id"`
	SquadID string `json:"squad_id"`
	// Title is the optional board-task title used when the job fires.
	Title string `json:"title,omitempty"`
	// Message is the prompt delivered to the agent on each fire.
	Message string `json:"message"`
	// ScheduleKind selects the derivation of fire times.
	ScheduleKind ScheduleKind `json:"schedule_kind"`
	// ScheduleSpec pins the schedule per kind:
	//   at:    {"at": "2026-10-21T19:30:00+10:30"}
	//   every: {"interval_seconds": 3600}
	//   cron:  {"expression": "0 9 * * 1-5", "timezone": "Australia/Adelaide"}
	ScheduleSpec  json.RawMessage `json:"schedule_spec"`
	Enabled       bool            `json:"enabled"`
	NextRunAt     time.Time       `json:"next_run_at"`
	LastRunAt     time.Time       `json:"last_run_at,omitempty"`
	LastRunStatus string          `json:"last_run_status,omitempty"`
	FailCount     int             `json:"fail_count"`
	// DisabledReason is set when the scheduler auto-disabled the job
	// (consecutive failures) — surfaced in `cron list`.
	DisabledReason string `json:"disabled_reason,omitempty"`
	// CreatedByType/CreatedByID mirror the task-creation actor model
	// ("agent"/"user"); the fired task inherits them.
	CreatedByType string    `json:"created_by_type"`
	CreatedByID   string    `json:"created_by_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// AtSpec is the schedule_spec shape for kind=at.
type AtSpec struct {
	At string `json:"at"`
}

// EverySpec is the schedule_spec shape for kind=every.
type EverySpec struct {
	IntervalSeconds int `json:"interval_seconds"`
}

// CronSpec is the schedule_spec shape for kind=cron.
type CronSpec struct {
	Expression string `json:"expression"`
	Timezone   string `json:"timezone,omitempty"`
}

// BuildScheduleSpec normalizes per-kind schedule inputs into the JSON
// spec stored in ScheduleSpec. Exactly the fields for the given kind
// are emitted; anything inconsistent is rejected here rather than at
// every-fire evaluation time.
func BuildScheduleSpec(kind ScheduleKind, at string, intervalSeconds int, expression, timezone string) (json.RawMessage, error) {
	switch kind {
	case ScheduleAt:
		at = strings.TrimSpace(at)
		if at == "" {
			return nil, fmt.Errorf("schedule.at is required for kind=at")
		}
		if _, err := time.Parse(time.RFC3339, at); err != nil {
			return nil, fmt.Errorf("schedule.at must be an RFC3339 timestamp (e.g. 2026-10-21T19:30:00+09:30): %v", err)
		}
		return json.Marshal(AtSpec{At: at})
	case ScheduleEvery:
		if intervalSeconds < MinEverySeconds {
			return nil, fmt.Errorf("schedule.interval_seconds must be at least %d", MinEverySeconds)
		}
		if intervalSeconds > MaxEverySeconds {
			return nil, fmt.Errorf("schedule.interval_seconds must be at most %d (90 days)", MaxEverySeconds)
		}
		return json.Marshal(EverySpec{IntervalSeconds: intervalSeconds})
	case ScheduleCron:
		expression = strings.TrimSpace(expression)
		if expression == "" {
			return nil, fmt.Errorf("schedule.expression is required for kind=cron")
		}
		if _, err := ParseCronExpression(expression); err != nil {
			return nil, fmt.Errorf("schedule.expression: %w", err)
		}
		timezone = strings.TrimSpace(timezone)
		if timezone == "" {
			timezone = DefaultScheduleTimezone
		}
		if _, err := time.LoadLocation(timezone); err != nil {
			return nil, fmt.Errorf("schedule.timezone %q is not a valid IANA timezone: %v", timezone, err)
		}
		return json.Marshal(CronSpec{Expression: expression, Timezone: timezone})
	default:
		return nil, fmt.Errorf("unknown schedule kind %q (want at, every or cron)", kind)
	}
}

// cronParser parses the standard 5-field cron format (minute hour
// day-of-month month day-of-week). No macros, no seconds field — the
// spec surface is deliberately the POSIX subset.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ParseCronExpression validates a standard 5-field cron expression.
func ParseCronExpression(expr string) (cron.Schedule, error) {
	sched, err := cronParser.Parse(strings.TrimSpace(expr))
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid 5-field cron expression: %w", expr, err)
	}
	return sched, nil
}

// ComputeNextRun returns the first fire time strictly after `after` for
// the given kind/spec. kind=at returns the fixed instant (callers must
// disable one-shot jobs after firing; this function does not know about
// firing). Errors when the schedule is invalid or, for kind=at, when
// the instant is not after `after`.
func ComputeNextRun(kind ScheduleKind, spec json.RawMessage, after time.Time) (time.Time, error) {
	switch kind {
	case ScheduleAt:
		var s AtSpec
		if err := json.Unmarshal(spec, &s); err != nil {
			return time.Time{}, fmt.Errorf("invalid at schedule spec: %w", err)
		}
		at, err := time.Parse(time.RFC3339, s.At)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid at timestamp: %w", err)
		}
		if !at.After(after) {
			return time.Time{}, fmt.Errorf("schedule.at must be in the future")
		}
		return at.UTC(), nil
	case ScheduleEvery:
		var s EverySpec
		if err := json.Unmarshal(spec, &s); err != nil {
			return time.Time{}, fmt.Errorf("invalid every schedule spec: %w", err)
		}
		if s.IntervalSeconds < MinEverySeconds || s.IntervalSeconds > MaxEverySeconds {
			return time.Time{}, fmt.Errorf("interval_seconds %d out of range [%d, %d]", s.IntervalSeconds, MinEverySeconds, MaxEverySeconds)
		}
		return after.UTC().Add(time.Duration(s.IntervalSeconds) * time.Second), nil
	case ScheduleCron:
		var s CronSpec
		if err := json.Unmarshal(spec, &s); err != nil {
			return time.Time{}, fmt.Errorf("invalid cron schedule spec: %w", err)
		}
		sched, err := ParseCronExpression(s.Expression)
		if err != nil {
			return time.Time{}, err
		}
		tz := strings.TrimSpace(s.Timezone)
		if tz == "" {
			tz = DefaultScheduleTimezone
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid timezone %q: %w", tz, err)
		}
		// Evaluate in the job's timezone so DST transitions follow the
		// wall clock: robfig/cron's Next skips instants that do not
		// exist on a spring-forward day and lands on the next valid
		// wall-clock match.
		return sched.Next(after.In(loc)).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("unknown schedule kind %q", kind)
	}
}
