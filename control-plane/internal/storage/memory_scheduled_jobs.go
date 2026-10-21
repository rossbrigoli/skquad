package storage

// S-281: in-memory ScheduledJobStore implementation. Mirrors the
// Postgres semantics in postgres_scheduled_jobs.go, including the
// atomic claim: the write lock is held across select-and-mark, so two
// concurrent ClaimDueScheduledJobs calls can never hand the same job
// to both claimants (the memory analogue of FOR UPDATE SKIP LOCKED).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func cloneScheduledJob(j *domain.ScheduledJob) *domain.ScheduledJob {
	if j == nil {
		return nil
	}
	out := *j
	if j.ScheduleSpec != nil {
		out.ScheduleSpec = append(json.RawMessage(nil), j.ScheduleSpec...)
	}
	return &out
}

func (m *MemoryStore) CreateScheduledJob(_ context.Context, job *domain.ScheduledJob) (*domain.ScheduledJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	created := cloneScheduledJob(job)
	if created.ID == "" {
		created.ID = uuid.NewString()
	}
	if created.CreatedAt.IsZero() {
		created.CreatedAt = time.Now().UTC()
	}
	if created.CreatedByType == "" {
		created.CreatedByType = "agent"
	}
	m.scheduledJobs[created.ID] = created
	m.scheduledJobOrder = append(m.scheduledJobOrder, created.ID)
	return cloneScheduledJob(created), nil
}

func (m *MemoryStore) GetScheduledJob(_ context.Context, id string) (*domain.ScheduledJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.scheduledJobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneScheduledJob(job), nil
}

func (m *MemoryStore) ListScheduledJobs(_ context.Context, agentID string) ([]*domain.ScheduledJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := make([]*domain.ScheduledJob, 0, len(m.scheduledJobOrder))
	for _, id := range m.scheduledJobOrder {
		if job, ok := m.scheduledJobs[id]; ok && job.AgentID == agentID {
			jobs = append(jobs, cloneScheduledJob(job))
		}
	}
	return jobs, nil
}

func (m *MemoryStore) DeleteScheduledJob(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.scheduledJobs[id]; !ok {
		return ErrNotFound
	}
	delete(m.scheduledJobs, id)
	for i, existing := range m.scheduledJobOrder {
		if existing == id {
			m.scheduledJobOrder = append(m.scheduledJobOrder[:i], m.scheduledJobOrder[i+1:]...)
			break
		}
	}
	return nil
}

func (m *MemoryStore) ClaimDueScheduledJobs(_ context.Context, now time.Time, limit int, claimSafetyNet time.Duration) ([]*domain.ScheduledJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Collect due ids first (no mutation while ranging), then claim.
	type due struct {
		id string
		at time.Time
	}
	dueJobs := make([]due, 0)
	for _, id := range m.scheduledJobOrder {
		job, ok := m.scheduledJobs[id]
		if !ok || !job.Enabled || job.NextRunAt.After(now) {
			continue
		}
		dueJobs = append(dueJobs, due{id: id, at: job.NextRunAt})
	}
	// Deterministic claim order: earliest due first (matches Postgres).
	for i := 1; i < len(dueJobs); i++ {
		for j := i; j > 0 && dueJobs[j].at.Before(dueJobs[j-1].at); j-- {
			dueJobs[j], dueJobs[j-1] = dueJobs[j-1], dueJobs[j]
		}
	}
	claimed := make([]*domain.ScheduledJob, 0, limit)
	for _, d := range dueJobs {
		if len(claimed) >= limit {
			break
		}
		job := m.scheduledJobs[d.id]
		job.LastRunAt = now.UTC()
		job.LastRunStatus = "running"
		// Safety net: if the worker dies before Finish/Fail, the job is
		// re-claimable after the safety window instead of being stuck.
		job.NextRunAt = now.Add(claimSafetyNet).UTC()
		claimed = append(claimed, cloneScheduledJob(job))
	}
	return claimed, nil
}

func (m *MemoryStore) FinishScheduledJobRun(_ context.Context, id string, firedAt time.Time, nextRunAt time.Time, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.scheduledJobs[id]
	if !ok {
		return ErrNotFound
	}
	job.LastRunAt = firedAt.UTC()
	job.LastRunStatus = "fired"
	job.FailCount = 0
	job.NextRunAt = nextRunAt.UTC()
	job.Enabled = enabled
	if !enabled {
		job.DisabledReason = "one-shot job completed"
	}
	return nil
}

func (m *MemoryStore) FailScheduledJobRun(_ context.Context, id string, firedAt time.Time, maxFailures int, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.scheduledJobs[id]
	if !ok {
		return ErrNotFound
	}
	job.LastRunAt = firedAt.UTC()
	job.LastRunStatus = "failed"
	job.FailCount++
	if maxFailures > 0 && job.FailCount >= maxFailures {
		job.Enabled = false
		job.DisabledReason = reason
	}
	return nil
}

func (m *MemoryStore) SetScheduledJobEnabled(_ context.Context, id string, enabled bool, nextRunAt time.Time) (*domain.ScheduledJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.scheduledJobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	job.Enabled = enabled
	if !nextRunAt.IsZero() {
		job.NextRunAt = nextRunAt.UTC()
	}
	if enabled {
		job.FailCount = 0
		job.DisabledReason = ""
	}
	return cloneScheduledJob(job), nil
}
