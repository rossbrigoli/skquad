package storage

import (
	"context"
	"sync"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// Pending-audit context (S-86): a handler declares the audit entries that
// MUST commit together with the next state mutation by attaching them to the
// context. Mutating store methods drain the list inside their transaction,
// so a committed mutation always carries its audit record and a failed
// audit write rolls the mutation back. Best-effort post-hoc auditing is no
// longer acceptable for significant state changes.

type pendingAuditKey struct{}

// PendingAudits is a drainable, goroutine-safe audit queue carried in the
// context. Drain semantics prevent a second mutation in the same request
// from re-recording the same entries.
type PendingAudits struct {
	mu      sync.Mutex
	entries []*domain.AuditEntry
}

// WithPendingAudit returns a derived context whose next drained mutation
// must persist the given audit entries in the same transaction.
func WithPendingAudit(ctx context.Context, entries ...*domain.AuditEntry) context.Context {
	kept := make([]*domain.AuditEntry, 0, len(entries))
	for _, e := range entries {
		if e != nil {
			kept = append(kept, e)
		}
	}
	return context.WithValue(ctx, pendingAuditKey{}, &PendingAudits{entries: kept})
}

// DrainPendingAudits removes and returns the pending audit entries.
// Called by mutating store methods inside their transaction/lock.
func DrainPendingAudits(ctx context.Context) []*domain.AuditEntry {
	list, ok := ctx.Value(pendingAuditKey{}).(*PendingAudits)
	if !ok || list == nil {
		return nil
	}
	list.mu.Lock()
	defer list.mu.Unlock()
	out := list.entries
	list.entries = nil
	return out
}

// NewAuditEntry is a small builder so httpapi call sites stay readable.
func NewAuditEntry(actorType, actorID, action, resourceType, resourceID, squadID string, metadata []byte) *domain.AuditEntry {
	if len(metadata) == 0 {
		metadata = []byte(`{}`)
	}
	return &domain.AuditEntry{
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		SquadID:      squadID,
		Metadata:     metadata,
	}
}
