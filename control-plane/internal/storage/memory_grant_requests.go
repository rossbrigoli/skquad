package storage

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// TG-8 slice B: grant-request workflow, in-memory mirror of the Postgres
// implementation in postgres_grant_requests.go. Same filter semantics and
// the same guarded-transition contract (expectedFrom mismatch ⇒ ErrConflict).

func cloneGrantRequest(r *domain.GrantRequest) *domain.GrantRequest {
	out := *r
	if r.RequestedScope != nil {
		out.RequestedScope = append(json.RawMessage(nil), r.RequestedScope...)
	}
	if r.Findings != nil {
		out.Findings = append([]domain.GrantLintFinding(nil), r.Findings...)
	}
	return &out
}

func (m *MemoryStore) CreateGrantRequest(_ context.Context, r *domain.GrantRequest) (*domain.GrantRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	created := cloneGrantRequest(r)
	created.ID = uuid.NewString()
	if created.State == "" {
		created.State = domain.GrantRequestPendingOwner
	}
	if created.Findings == nil {
		created.Findings = []domain.GrantLintFinding{}
	}
	if len(created.RequestedScope) == 0 {
		created.RequestedScope = json.RawMessage(`{}`)
	}
	now := time.Now().UTC()
	created.CreatedAt = now
	created.UpdatedAt = now
	m.grantRequests[created.ID] = created
	return cloneGrantRequest(created), nil
}

func (m *MemoryStore) GetGrantRequest(_ context.Context, id string) (*domain.GrantRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	req, ok := m.grantRequests[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneGrantRequest(req), nil
}

func (m *MemoryStore) ListGrantRequests(_ context.Context, f GrantRequestFilter) ([]*domain.GrantRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.GrantRequest, 0, len(m.grantRequests))
	for _, req := range m.grantRequests {
		if f.State != "" && string(req.State) != f.State {
			continue
		}
		if f.RequesterUserID != "" && req.RequesterUserID != f.RequesterUserID {
			continue
		}
		if f.OwnerUserID != "" {
			// Owner axis = the resource's owner attribute. A request on a
			// resource with no owner matches nobody.
			res, ok := m.resources[req.ResourceID]
			if !ok || res.OwnerUserID != f.OwnerUserID {
				continue
			}
		}
		out = append(out, cloneGrantRequest(req))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (m *MemoryStore) UpdateGrantRequestState(_ context.Context, id string, expectedFrom domain.GrantRequestState, up GrantRequestUpdate) (*domain.GrantRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	req, ok := m.grantRequests[id]
	if !ok {
		return nil, ErrNotFound
	}
	if req.State != expectedFrom {
		return nil, ErrConflict
	}
	req.State = up.State
	if up.ApprovedByOwnerAt != nil {
		req.ApprovedByOwnerAt = up.ApprovedByOwnerAt
	}
	if up.ApprovedByAdminAt != nil {
		req.ApprovedByAdminAt = up.ApprovedByAdminAt
	}
	if up.DeniedReason != "" {
		req.DeniedReason = up.DeniedReason
	}
	req.UpdatedAt = time.Now().UTC()
	return cloneGrantRequest(req), nil
}
