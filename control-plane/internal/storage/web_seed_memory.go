package storage

// TG-3 (S-249): MemoryStore counterpart of migration
// 0046_web_seed.sql. Production seeding runs in SQL; this helper gives
// MemoryStore-based tests/dev the same idempotent state: the system
// `web` resource plus grants to every agent while the builtin
// web_fetch is enabled platform-wide.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// SystemWebUserID is the fixed system principal owning the seeded web
// resource and grants (mirrors migration 0046).
const SystemWebUserID = "736b7175-6164-4000-8000-000000000001"

// SystemWebResourceID is the fixed id of the system `web` resource
// (mirrors migration 0046).
const SystemWebResourceID = "736b7175-6164-4000-8000-000000000002"

// SystemWebCeiling mirrors the BT-6 defaults: 256 KiB cap, no rate
// limit, no private-network reach, empty denylists.
const SystemWebCeiling = `{"deny_domains": [], "deny_cidrs": [], "max_bytes": 262144, "allow_private_network": false}`

// SeedSystemWeb idempotently registers the system web resource and
// grants it to all agents when the builtin web_fetch is enabled.
func (m *MemoryStore) SeedSystemWeb(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.resources[SystemWebResourceID]; !ok {
		now := time.Now().UTC()
		m.resources[SystemWebResourceID] = &domain.RegistryResource{
			ID:             SystemWebResourceID,
			Type:           domain.ResWeb,
			Name:           "system-web",
			Description:    "Platform-wide governed web fetch (migration from built-in BT-6).",
			Manifest:       json.RawMessage(`{}`),
			Status:         domain.ResourceActive,
			RegisteredBy:   SystemWebUserID,
			CreatedAt:      now,
			EndpointConfig: json.RawMessage(SystemWebCeiling),
			PolicyCeiling:  json.RawMessage(SystemWebCeiling),
			RiskTier:       "medium",
			EgressClass:    "public",
		}
	}

	wf, err := m.GetBuiltinTool(context.Background(), domain.BuiltinToolWebFetch)
	if err != nil || wf == nil || !wf.Enabled {
		// Builtin web_fetch disabled platform-wide: no grants seeded.
		return nil
	}
	for agentID := range m.agents {
		key := permissionKey(agentID, domain.ResWeb, SystemWebResourceID)
		if _, ok := m.permissions[key]; ok {
			continue
		}
		m.permissions[key] = &domain.AgentPermission{
			AgentID:      agentID,
			ResourceType: domain.ResWeb,
			ResourceID:   SystemWebResourceID,
			GrantedBy:    SystemWebUserID,
			CreatedAt:    time.Now().UTC(),
			Constraints:  json.RawMessage(`{}`),
		}
	}
	return nil
}
