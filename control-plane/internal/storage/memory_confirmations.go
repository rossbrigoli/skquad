package storage

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// TG-8 slice C: confirmation gates + standing grants, in-memory mirror
// of the Postgres implementation in postgres_confirmations.go. Same
// filter semantics and the same guarded-transition contract
// (expectedFrom mismatch ⇒ ErrConflict).

func clonePendingConfirmation(c *domain.PendingConfirmation) *domain.PendingConfirmation {
	out := *c
	return &out
}

func cloneStandingGrant(g *domain.StandingGrant) *domain.StandingGrant {
	out := *g
	return &out
}

func (m *MemoryStore) CreatePendingConfirmation(_ context.Context, c *domain.PendingConfirmation) (*domain.PendingConfirmation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	created := clonePendingConfirmation(c)
	created.ID = uuid.NewString()
	if created.State == "" {
		created.State = domain.ConfirmationPending
	}
	now := time.Now().UTC()
	created.CreatedAt = now
	created.UpdatedAt = now
	m.pendingConfirms[created.ID] = created
	return clonePendingConfirmation(created), nil
}

func (m *MemoryStore) GetPendingConfirmation(_ context.Context, id string) (*domain.PendingConfirmation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.pendingConfirms[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clonePendingConfirmation(c), nil
}

func (m *MemoryStore) ListPendingConfirmations(_ context.Context, f ConfirmationFilter) ([]*domain.PendingConfirmation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.PendingConfirmation, 0, len(m.pendingConfirms))
	for _, c := range m.pendingConfirms {
		if f.State != "" && string(c.State) != f.State {
			continue
		}
		if f.RequestedBy != "" && c.RequestedBy != f.RequestedBy {
			continue
		}
		if f.ResourceID != "" && c.ResourceID != f.ResourceID {
			continue
		}
		out = append(out, clonePendingConfirmation(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (m *MemoryStore) UpdateConfirmationState(_ context.Context, id string, expectedFrom domain.ConfirmationState, up ConfirmationUpdate) (*domain.PendingConfirmation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.pendingConfirms[id]
	if !ok {
		return nil, ErrNotFound
	}
	if c.State != expectedFrom {
		return nil, ErrConflict
	}
	c.State = up.State
	if up.InboxMessageID != nil {
		c.InboxMessageID = *up.InboxMessageID
	}
	if up.ApprovedAt != nil {
		c.ApprovedAt = up.ApprovedAt
	}
	if up.ConsumedAt != nil {
		c.ConsumedAt = up.ConsumedAt
	}
	if up.DeniedReason != "" {
		c.DeniedReason = up.DeniedReason
	}
	c.UpdatedAt = time.Now().UTC()
	return clonePendingConfirmation(c), nil
}

func (m *MemoryStore) FindLiveStandingGrant(_ context.Context, resourceID, agentID, tool string, now time.Time) (*domain.StandingGrant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.standingGrants {
		if g.RevokedAt != nil {
			continue
		}
		if g.ResourceID == resourceID && g.AgentID == agentID && g.Tool == tool && g.ExpiresAt.After(now) {
			return cloneStandingGrant(g), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemoryStore) UpsertStandingGrant(_ context.Context, g *domain.StandingGrant) (*domain.StandingGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.standingGrants {
		if existing.RevokedAt == nil && existing.ResourceID == g.ResourceID && existing.AgentID == g.AgentID && existing.Tool == g.Tool {
			existing.ExpiresAt = g.ExpiresAt
			existing.CreatedBy = g.CreatedBy
			return cloneStandingGrant(existing), nil
		}
	}
	created := cloneStandingGrant(g)
	created.ID = uuid.NewString()
	created.CreatedAt = time.Now().UTC()
	m.standingGrants[created.ID] = created
	return cloneStandingGrant(created), nil
}

func (m *MemoryStore) GetStandingGrant(_ context.Context, id string) (*domain.StandingGrant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.standingGrants[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneStandingGrant(g), nil
}

func (m *MemoryStore) ListStandingGrants(_ context.Context, f StandingGrantFilter) ([]*domain.StandingGrant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.StandingGrant, 0, len(m.standingGrants))
	for _, g := range m.standingGrants {
		if g.RevokedAt != nil && !f.IncludeRevoked {
			continue
		}
		if f.OwnerUserID != "" {
			res, ok := m.resources[g.ResourceID]
			if !ok || res.OwnerUserID != f.OwnerUserID {
				continue
			}
		}
		out = append(out, cloneStandingGrant(g))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (m *MemoryStore) RevokeStandingGrant(_ context.Context, id string, at time.Time) (*domain.StandingGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.standingGrants[id]
	if !ok || g.RevokedAt != nil {
		return nil, ErrNotFound
	}
	g.RevokedAt = &at
	return cloneStandingGrant(g), nil
}
