package storage

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const (
	defaultMessageMaxAttempts = 3
	defaultMessageRetryDelay  = 30 * time.Second
	defaultMessageTTL         = 24 * time.Hour
	maxMessageTerminalReason  = 500
)

// MemoryStore is a process-local store used for development and handler tests.
// It is intentionally simple; production persistence belongs in the Postgres
// implementation.
type MemoryStore struct {
	mu sync.RWMutex

	users         map[string]*domain.User
	usersByEmail  map[string]string
	usersByOIDC   map[string]string
	squads        map[string]*domain.Squad
	agents        map[string]*domain.Agent
	identities    map[string]*domain.AgentIdentity
	identityAgent map[string]string
	boards        map[string]*domain.Board
	boardsBySquad map[string]string
	grants        map[string]*domain.AccessGrant
	llmProviders  map[string]*domain.LLMProvider
	resources     map[string]*domain.RegistryResource
	permissions   map[string]*domain.AgentPermission
	metering      map[string]*domain.MeteringEvent
	auditLog      map[string]*domain.AuditEntry
	tasks         map[string]*domain.Task
	taskExecs     map[string]*domain.TaskExecution
	agentMemory   map[string]*domain.AgentMemory
	messages      map[string]*domain.Message
	k8sOutbox     map[string]*domain.KubernetesOutboxEvent
}

// NewMemoryStore creates an empty development store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users:         map[string]*domain.User{},
		usersByEmail:  map[string]string{},
		usersByOIDC:   map[string]string{},
		squads:        map[string]*domain.Squad{},
		agents:        map[string]*domain.Agent{},
		identities:    map[string]*domain.AgentIdentity{},
		identityAgent: map[string]string{},
		boards:        map[string]*domain.Board{},
		boardsBySquad: map[string]string{},
		grants:        map[string]*domain.AccessGrant{},
		llmProviders:  map[string]*domain.LLMProvider{},
		resources:     map[string]*domain.RegistryResource{},
		permissions:   map[string]*domain.AgentPermission{},
		metering:      map[string]*domain.MeteringEvent{},
		auditLog:      map[string]*domain.AuditEntry{},
		tasks:         map[string]*domain.Task{},
		taskExecs:     map[string]*domain.TaskExecution{},
		agentMemory:   map[string]*domain.AgentMemory{},
		messages:      map[string]*domain.Message{},
		k8sOutbox:     map[string]*domain.KubernetesOutboxEvent{},
	}
}

func (m *MemoryStore) enqueueKubernetesOutboxLocked(aggregateType, aggregateID, operation string, payload domain.KubernetesOutboxPayload) {
	raw, _ := json.Marshal(payload)
	now := time.Now().UTC()
	event := &domain.KubernetesOutboxEvent{
		ID:            uuid.NewString(),
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Operation:     operation,
		Payload:       raw,
		Status:        domain.KubernetesOutboxPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	m.k8sOutbox[event.ID] = event
}

func (m *MemoryStore) enqueueSquadOutboxLocked(operation string, squad *domain.Squad) {
	m.enqueueKubernetesOutboxLocked(domain.KubernetesAggregateSquad, squad.ID, operation, domain.KubernetesOutboxPayload{Squad: cloneSquad(squad)})
}

func (m *MemoryStore) enqueueAgentOutboxLocked(operation string, agent *domain.Agent) {
	var identity *domain.AgentIdentity
	if identityID, ok := m.identityAgent[agent.ID]; ok {
		identity = cloneAgentIdentity(m.identities[identityID])
	}
	m.enqueueKubernetesOutboxLocked(domain.KubernetesAggregateAgent, agent.ID, operation, domain.KubernetesOutboxPayload{
		Agent:    cloneAgent(agent),
		Identity: identity,
	})
}

func (m *MemoryStore) GetUser(_ context.Context, id string) (*domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneUser(u), nil
}

func (m *MemoryStore) GetUserByEmail(_ context.Context, email string) (*domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.usersByEmail[strings.ToLower(email)]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneUser(m.users[id]), nil
}

func (m *MemoryStore) UpsertUser(_ context.Context, u *domain.User) (*domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	email := strings.ToLower(strings.TrimSpace(u.Email))
	oidcKey := userOIDCKey(u.OIDCIssuer, u.OIDCSubject)
	if oidcKey != "" {
		if id, ok := m.usersByOIDC[oidcKey]; ok {
			existing := cloneUser(m.users[id])
			existing.Email = email
			existing.EmailVerified = u.EmailVerified
			if u.Name != "" {
				existing.Name = u.Name
			}
			m.users[id] = existing
			m.usersByEmail[email] = id
			return cloneUser(existing), nil
		}
	}
	if oidcKey == "" {
		if id, ok := m.usersByEmail[email]; ok {
			existing := cloneUser(m.users[id])
			if u.Name != "" {
				existing.Name = u.Name
			}
			m.users[id] = existing
			return cloneUser(existing), nil
		}
	}

	now := time.Now().UTC()
	created := cloneUser(u)
	created.ID = uuid.NewString()
	created.Email = email
	created.OIDCIssuer = strings.TrimSpace(u.OIDCIssuer)
	created.OIDCSubject = strings.TrimSpace(u.OIDCSubject)
	if created.Role == "" {
		created.Role = domain.RoleUser
	}
	created.CreatedAt = now
	m.users[created.ID] = created
	m.usersByEmail[email] = created.ID
	if oidcKey != "" {
		m.usersByOIDC[oidcKey] = created.ID
	}
	return cloneUser(created), nil
}

func (m *MemoryStore) SetUserRole(_ context.Context, id string, role domain.Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !role.Valid() {
		return ErrConflict
	}
	u, ok := m.users[id]
	if !ok {
		return ErrNotFound
	}
	u.Role = role
	return nil
}

func (m *MemoryStore) ListUsers(_ context.Context) ([]*domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, cloneUser(u))
	}
	slices.SortFunc(out, func(a, b *domain.User) int {
		return strings.Compare(a.Email, b.Email)
	})
	return out, nil
}

func (m *MemoryStore) CreateSquad(_ context.Context, s *domain.Squad) (*domain.Squad, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.squads {
		if existing.OwnerID == s.OwnerID && strings.EqualFold(existing.Name, s.Name) {
			return nil, ErrConflict
		}
	}

	now := time.Now().UTC()
	created := cloneSquad(s)
	created.ID = uuid.NewString()
	if created.Status == "" {
		created.Status = domain.SquadActive
	}
	created.CreatedAt = now
	created.UpdatedAt = now
	m.squads[created.ID] = created

	board := &domain.Board{ID: uuid.NewString(), SquadID: created.ID, CreatedAt: now}
	m.boards[board.ID] = board
	m.boardsBySquad[created.ID] = board.ID
	m.enqueueSquadOutboxLocked(domain.KubernetesOpUpsertSquad, created)
	return cloneSquad(created), nil
}

func (m *MemoryStore) GetSquad(_ context.Context, id string) (*domain.Squad, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.squads[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneSquad(s), nil
}

func (m *MemoryStore) GetSquadByName(_ context.Context, ownerID, name string) (*domain.Squad, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.squads {
		if s.OwnerID == ownerID && strings.EqualFold(s.Name, name) {
			return cloneSquad(s), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemoryStore) UpdateSquad(_ context.Context, s *domain.Squad) (*domain.Squad, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.squads[s.ID]
	if !ok {
		return nil, ErrNotFound
	}
	for _, other := range m.squads {
		if other.ID != s.ID && other.OwnerID == existing.OwnerID && strings.EqualFold(other.Name, s.Name) {
			return nil, ErrConflict
		}
	}
	updated := cloneSquad(s)
	updated.OwnerID = existing.OwnerID
	updated.Namespace = existing.Namespace
	updated.CreatedAt = existing.CreatedAt
	updated.UpdatedAt = time.Now().UTC()
	m.squads[s.ID] = updated
	m.enqueueSquadOutboxLocked(domain.KubernetesOpUpsertSquad, updated)
	return cloneSquad(updated), nil
}

func (m *MemoryStore) DeleteSquad(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	squad, ok := m.squads[id]
	if !ok {
		return ErrNotFound
	}
	m.enqueueSquadOutboxLocked(domain.KubernetesOpDeleteSquad, squad)
	delete(m.squads, id)
	if boardID, ok := m.boardsBySquad[id]; ok {
		delete(m.boards, boardID)
		delete(m.boardsBySquad, id)
		for taskID, task := range m.tasks {
			if task.BoardID == boardID {
				delete(m.tasks, taskID)
			}
		}
	}
	for agentID, agent := range m.agents {
		if agent.SquadID == id {
			delete(m.agents, agentID)
		}
	}
	for grantID, grant := range m.grants {
		if grant.SquadID == id {
			delete(m.grants, grantID)
		}
	}
	return nil
}

func (m *MemoryStore) ListSquads(_ context.Context, ownerID string) ([]*domain.Squad, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.Squad, 0, len(m.squads))
	for _, s := range m.squads {
		if ownerID == "" || s.OwnerID == ownerID {
			out = append(out, cloneSquad(s))
		}
	}
	slices.SortFunc(out, func(a, b *domain.Squad) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (m *MemoryStore) CreateAgent(_ context.Context, a *domain.Agent) (*domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.squads[a.SquadID]; !ok {
		return nil, ErrNotFound
	}
	for _, existing := range m.agents {
		if existing.SquadID == a.SquadID && strings.EqualFold(existing.Name, a.Name) {
			return nil, ErrConflict
		}
	}
	now := time.Now().UTC()
	created := cloneAgent(a)
	created.ID = uuid.NewString()
	if created.Status == "" {
		created.Status = domain.AgentIdle
	}
	created.CreatedAt = now
	created.UpdatedAt = now
	m.agents[created.ID] = created
	m.enqueueAgentOutboxLocked(domain.KubernetesOpUpsertAgent, created)
	return cloneAgent(created), nil
}

func (m *MemoryStore) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.agents[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneAgent(a), nil
}

func (m *MemoryStore) UpdateAgent(_ context.Context, a *domain.Agent) (*domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.agents[a.ID]
	if !ok {
		return nil, ErrNotFound
	}
	for _, other := range m.agents {
		if other.ID != a.ID && other.SquadID == existing.SquadID && strings.EqualFold(other.Name, a.Name) {
			return nil, ErrConflict
		}
	}
	updated := cloneAgent(a)
	updated.SquadID = existing.SquadID
	updated.CreatedAt = existing.CreatedAt
	updated.UpdatedAt = time.Now().UTC()
	m.agents[a.ID] = updated
	m.enqueueAgentOutboxLocked(domain.KubernetesOpUpsertAgent, updated)
	return cloneAgent(updated), nil
}

func (m *MemoryStore) DeleteAgent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[id]
	if !ok {
		return ErrNotFound
	}
	m.enqueueAgentOutboxLocked(domain.KubernetesOpDeleteAgent, agent)
	delete(m.agents, id)
	for _, task := range m.tasks {
		if task.AssigneeAgentID == id {
			task.AssigneeAgentID = ""
			task.UpdatedAt = time.Now().UTC()
		}
	}
	return nil
}

func (m *MemoryStore) ListAgents(_ context.Context, squadID string) ([]*domain.Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*domain.Agent{}
	for _, a := range m.agents {
		if a.SquadID == squadID {
			out = append(out, cloneAgent(a))
		}
	}
	slices.SortFunc(out, func(a, b *domain.Agent) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (m *MemoryStore) SetAgentStatus(_ context.Context, id string, status domain.AgentStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return ErrNotFound
	}
	a.Status = status
	a.UpdatedAt = time.Now().UTC()
	m.enqueueAgentOutboxLocked(domain.KubernetesOpUpsertAgent, a)
	return nil
}

func (m *MemoryStore) CreateAgentIdentity(_ context.Context, i *domain.AgentIdentity) (*domain.AgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[i.AgentID]
	if !ok {
		return nil, ErrNotFound
	}
	if _, ok := m.identityAgent[i.AgentID]; ok {
		return nil, ErrConflict
	}
	created := cloneAgentIdentity(i)
	created.ID = uuid.NewString()
	created.CreatedAt = time.Now().UTC()
	m.identities[created.ID] = created
	m.identityAgent[created.AgentID] = created.ID
	agent.IdentityID = created.ID
	agent.UpdatedAt = created.CreatedAt
	m.enqueueAgentOutboxLocked(domain.KubernetesOpUpsertAgent, agent)
	return cloneAgentIdentity(created), nil
}

func (m *MemoryStore) GetAgentIdentity(_ context.Context, agentID string) (*domain.AgentIdentity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	identityID, ok := m.identityAgent[agentID]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneAgentIdentity(m.identities[identityID]), nil
}

func (m *MemoryStore) RotateAgentIdentity(_ context.Context, agentID string, credentialRef string, credentialHash string, virtualKeyRef string) (*domain.AgentIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	identityID, ok := m.identityAgent[agentID]
	if !ok {
		return nil, ErrNotFound
	}
	identity := m.identities[identityID]
	identity.CredentialRef = credentialRef
	identity.CredentialHash = credentialHash
	identity.VirtualKeyRef = virtualKeyRef
	identity.RotatedAt = time.Now().UTC()
	m.enqueueAgentOutboxLocked(domain.KubernetesOpUpsertAgent, m.agents[agentID])
	return cloneAgentIdentity(identity), nil
}

func (m *MemoryStore) CreateGrant(_ context.Context, g *domain.AccessGrant) (*domain.AccessGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.squads[g.SquadID]; !ok {
		return nil, ErrNotFound
	}
	for _, existing := range m.grants {
		if existing.SquadID == g.SquadID && existing.GranteeType == g.GranteeType && existing.GranteeID == g.GranteeID {
			return nil, ErrConflict
		}
	}
	created := cloneGrant(g)
	created.ID = uuid.NewString()
	created.CreatedAt = time.Now().UTC()
	m.grants[created.ID] = created
	return cloneGrant(created), nil
}

func (m *MemoryStore) GetGrant(_ context.Context, id string) (*domain.AccessGrant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	grant, ok := m.grants[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneGrant(grant), nil
}

func (m *MemoryStore) RevokeGrant(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.grants[id]; !ok {
		return ErrNotFound
	}
	delete(m.grants, id)
	return nil
}

func (m *MemoryStore) ListGrants(_ context.Context, squadID string) ([]*domain.AccessGrant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*domain.AccessGrant{}
	for _, grant := range m.grants {
		if grant.SquadID == squadID {
			out = append(out, cloneGrant(grant))
		}
	}
	slices.SortFunc(out, func(a, b *domain.AccessGrant) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (m *MemoryStore) UserMayAccessSquad(_ context.Context, userID, squadID string, action string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if squad, ok := m.squads[squadID]; ok && squad.OwnerID == userID {
		return true, nil
	}
	for _, grant := range m.grants {
		if grant.SquadID == squadID && grant.GranteeType == domain.GranteeUser && grant.GranteeID == userID && grantAllows(grant.Permissions, action) {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemoryStore) AgentMayMessageSquad(_ context.Context, agentID, squadID string, action string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, grant := range m.grants {
		if grant.SquadID == squadID && grant.GranteeType == domain.GranteeAgent && grant.GranteeID == agentID && grantAllows(grant.Permissions, action) {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemoryStore) CreateLLMProvider(_ context.Context, p *domain.LLMProvider) (*domain.LLMProvider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.llmProviders {
		if strings.EqualFold(existing.Name, p.Name) {
			return nil, ErrConflict
		}
	}
	created := cloneLLMProvider(p)
	created.ID = uuid.NewString()
	if created.Status == "" {
		created.Status = domain.ResourceActive
	}
	created.CreatedAt = time.Now().UTC()
	m.llmProviders[created.ID] = created
	return cloneLLMProvider(created), nil
}

func (m *MemoryStore) GetLLMProvider(_ context.Context, id string) (*domain.LLMProvider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	provider, ok := m.llmProviders[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneLLMProvider(provider), nil
}

func (m *MemoryStore) UpdateLLMProvider(_ context.Context, p *domain.LLMProvider) (*domain.LLMProvider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.llmProviders[p.ID]
	if !ok {
		return nil, ErrNotFound
	}
	for _, other := range m.llmProviders {
		if other.ID != p.ID && strings.EqualFold(other.Name, p.Name) {
			return nil, ErrConflict
		}
	}
	updated := cloneLLMProvider(p)
	updated.RegisteredBy = existing.RegisteredBy
	updated.CreatedAt = existing.CreatedAt
	m.llmProviders[p.ID] = updated
	return cloneLLMProvider(updated), nil
}

func (m *MemoryStore) DeprecateLLMProvider(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	provider, ok := m.llmProviders[id]
	if !ok {
		return ErrNotFound
	}
	provider.Status = domain.ResourceDeprecated
	return nil
}

func (m *MemoryStore) ListLLMProviders(_ context.Context) ([]*domain.LLMProvider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.LLMProvider, 0, len(m.llmProviders))
	for _, provider := range m.llmProviders {
		out = append(out, cloneLLMProvider(provider))
	}
	slices.SortFunc(out, func(a, b *domain.LLMProvider) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (m *MemoryStore) CreateResource(_ context.Context, r *domain.RegistryResource) (*domain.RegistryResource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.resources {
		if existing.Type == r.Type && strings.EqualFold(existing.Name, r.Name) {
			return nil, ErrConflict
		}
	}
	created := cloneResource(r)
	created.ID = uuid.NewString()
	if created.Status == "" {
		created.Status = domain.ResourceActive
	}
	created.CreatedAt = time.Now().UTC()
	m.resources[created.ID] = created
	return cloneResource(created), nil
}

func (m *MemoryStore) GetResource(_ context.Context, typ domain.ResourceType, id string) (*domain.RegistryResource, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	resource, ok := m.resources[id]
	if !ok || resource.Type != typ {
		return nil, ErrNotFound
	}
	return cloneResource(resource), nil
}

func (m *MemoryStore) UpdateResource(_ context.Context, r *domain.RegistryResource) (*domain.RegistryResource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.resources[r.ID]
	if !ok || existing.Type != r.Type {
		return nil, ErrNotFound
	}
	for _, other := range m.resources {
		if other.ID != r.ID && other.Type == r.Type && strings.EqualFold(other.Name, r.Name) {
			return nil, ErrConflict
		}
	}
	updated := cloneResource(r)
	updated.Type = existing.Type
	updated.RegisteredBy = existing.RegisteredBy
	updated.CreatedAt = existing.CreatedAt
	m.resources[r.ID] = updated
	return cloneResource(updated), nil
}

func (m *MemoryStore) DeprecateResource(_ context.Context, typ domain.ResourceType, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	resource, ok := m.resources[id]
	if !ok || resource.Type != typ {
		return ErrNotFound
	}
	resource.Status = domain.ResourceDeprecated
	return nil
}

func (m *MemoryStore) ListResources(_ context.Context, typ domain.ResourceType) ([]*domain.RegistryResource, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*domain.RegistryResource{}
	for _, resource := range m.resources {
		if resource.Type == typ {
			out = append(out, cloneResource(resource))
		}
	}
	slices.SortFunc(out, func(a, b *domain.RegistryResource) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (m *MemoryStore) GrantAgentPermission(_ context.Context, p *domain.AgentPermission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[p.AgentID]; !ok {
		return ErrNotFound
	}
	key := permissionKey(p.AgentID, p.ResourceType, p.ResourceID)
	if _, ok := m.permissions[key]; ok {
		return nil
	}
	created := cloneAgentPermission(p)
	created.ID = uuid.NewString()
	created.CreatedAt = time.Now().UTC()
	m.permissions[key] = created
	return nil
}

func (m *MemoryStore) RevokeAgentPermission(_ context.Context, agentID string, typ domain.ResourceType, resourceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[agentID]; !ok {
		return ErrNotFound
	}
	delete(m.permissions, permissionKey(agentID, typ, resourceID))
	return nil
}

func (m *MemoryStore) ListAgentPermissions(_ context.Context, agentID string) ([]*domain.AgentPermission, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.agents[agentID]; !ok {
		return nil, ErrNotFound
	}
	out := []*domain.AgentPermission{}
	for _, perm := range m.permissions {
		if perm.AgentID == agentID {
			out = append(out, cloneAgentPermission(perm))
		}
	}
	slices.SortFunc(out, func(a, b *domain.AgentPermission) int {
		if a.ResourceType != b.ResourceType {
			return strings.Compare(string(a.ResourceType), string(b.ResourceType))
		}
		return strings.Compare(a.ResourceID, b.ResourceID)
	})
	return out, nil
}

func (m *MemoryStore) SetAgentPermissions(_ context.Context, agentID string, perms []domain.AgentPermission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[agentID]; !ok {
		return ErrNotFound
	}
	for key, perm := range m.permissions {
		if perm.AgentID == agentID {
			delete(m.permissions, key)
		}
	}
	now := time.Now().UTC()
	for _, perm := range perms {
		created := cloneAgentPermission(&perm)
		created.ID = uuid.NewString()
		created.AgentID = agentID
		created.CreatedAt = now
		m.permissions[permissionKey(agentID, created.ResourceType, created.ResourceID)] = created
	}
	return nil
}

func (m *MemoryStore) AgentHasPermission(_ context.Context, agentID string, typ domain.ResourceType, resourceID string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.agents[agentID]; !ok {
		return false, ErrNotFound
	}
	_, ok := m.permissions[permissionKey(agentID, typ, resourceID)]
	return ok, nil
}

func (m *MemoryStore) RecordMetering(_ context.Context, event *domain.MeteringEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[event.AgentID]; !ok {
		return ErrNotFound
	}
	if _, ok := m.squads[event.SquadID]; !ok {
		return ErrNotFound
	}
	created := cloneMeteringEvent(event)
	created.ID = uuid.NewString()
	if created.Currency == "" {
		created.Currency = "USD"
	}
	if created.Timestamp.IsZero() {
		created.Timestamp = time.Now().UTC()
	}
	m.metering[created.ID] = created
	return nil
}

func (m *MemoryStore) SumMetering(_ context.Context, squadID, agentID string) (*domain.MeteringEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := &domain.MeteringEvent{
		SquadID:  squadID,
		AgentID:  agentID,
		Currency: "USD",
	}
	for _, event := range m.metering {
		if squadID != "" && event.SquadID != squadID {
			continue
		}
		if agentID != "" && event.AgentID != agentID {
			continue
		}
		out.InputTokens += event.InputTokens
		out.OutputTokens += event.OutputTokens
		out.Cost += event.Cost
		if out.Timestamp.IsZero() || event.Timestamp.After(out.Timestamp) {
			out.Timestamp = event.Timestamp
		}
	}
	return out, nil
}

func (m *MemoryStore) RecordAudit(_ context.Context, entry *domain.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	created := cloneAuditEntry(entry)
	created.ID = uuid.NewString()
	if len(created.Metadata) == 0 {
		created.Metadata = []byte(`{}`)
	}
	if created.Timestamp.IsZero() {
		created.Timestamp = time.Now().UTC()
	}
	m.auditLog[created.ID] = created
	return nil
}

func (m *MemoryStore) ListAudit(_ context.Context, squadID string, limit int) ([]*domain.AuditEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 {
		limit = 100
	}
	out := []*domain.AuditEntry{}
	for _, entry := range m.auditLog {
		if squadID == "" || entry.SquadID == squadID {
			out = append(out, cloneAuditEntry(entry))
		}
	}
	slices.SortFunc(out, func(a, b *domain.AuditEntry) int {
		if !a.Timestamp.Equal(b.Timestamp) {
			if a.Timestamp.After(b.Timestamp) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) GetBoard(_ context.Context, squadID string) (*domain.Board, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	boardID, ok := m.boardsBySquad[squadID]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneBoard(m.boards[boardID]), nil
}

func (m *MemoryStore) CreateTask(_ context.Context, t *domain.Task) (*domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.boards[t.BoardID]; !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	created := cloneTask(t)
	created.ID = uuid.NewString()
	if created.Status == "" {
		created.Status = domain.TaskTodo
	}
	created.Position = m.nextTaskPosition(t.BoardID, created.Status)
	created.CreatedAt = now
	created.UpdatedAt = now
	m.tasks[created.ID] = created
	return cloneTask(created), nil
}

func (m *MemoryStore) GetTask(_ context.Context, id string) (*domain.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneTask(t), nil
}

func (m *MemoryStore) UpdateTask(_ context.Context, t *domain.Task) (*domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.tasks[t.ID]
	if !ok {
		return nil, ErrNotFound
	}
	updated := cloneTask(t)
	updated.BoardID = existing.BoardID
	updated.SquadID = existing.SquadID
	updated.CreatedByType = existing.CreatedByType
	updated.CreatedByID = existing.CreatedByID
	updated.CreatedAt = existing.CreatedAt
	if updated.Status != existing.Status {
		updated.Position = m.nextTaskPosition(existing.BoardID, updated.Status)
	}
	updated.UpdatedAt = time.Now().UTC()
	m.tasks[t.ID] = updated
	return cloneTask(updated), nil
}

func (m *MemoryStore) DeleteTask(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; !ok {
		return ErrNotFound
	}
	delete(m.tasks, id)
	return nil
}

func (m *MemoryStore) ListTasks(_ context.Context, boardID string, status domain.TaskStatus) ([]*domain.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*domain.Task{}
	for _, t := range m.tasks {
		if t.BoardID == boardID && (status == "" || t.Status == status) {
			out = append(out, cloneTask(t))
		}
	}
	slices.SortFunc(out, func(a, b *domain.Task) int {
		if a.Status != b.Status {
			return strings.Compare(string(a.Status), string(b.Status))
		}
		return a.Position - b.Position
	})
	return out, nil
}

func (m *MemoryStore) ListAgentTasks(_ context.Context, agentID string) ([]*domain.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.agents[agentID]; !ok {
		return nil, ErrNotFound
	}
	out := []*domain.Task{}
	for _, t := range m.tasks {
		if t.AssigneeAgentID == agentID {
			out = append(out, cloneTask(t))
		}
	}
	slices.SortFunc(out, func(a, b *domain.Task) int {
		if a.Status != b.Status {
			return strings.Compare(string(a.Status), string(b.Status))
		}
		return a.Position - b.Position
	})
	return out, nil
}

func (m *MemoryStore) ClaimNextTask(_ context.Context, agentID string, workerID string, leaseFor time.Duration) (*domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[agentID]; !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	if leaseFor <= 0 {
		leaseFor = 5 * time.Minute
	}
	if workerID == "" {
		workerID = agentID
	}
	for _, exec := range m.taskExecs {
		if exec.AgentID == agentID && exec.Status == domain.TaskExecutionActive && exec.LeaseExpiresAt.After(now) {
			return nil, ErrNotFound
		}
	}
	var candidate *domain.Task
	for _, task := range m.tasks {
		if task.AssigneeAgentID == agentID && task.Status == domain.TaskInProgress && !m.taskHasActiveExecutionLocked(task.ID, now) {
			if candidate == nil || task.UpdatedAt.Before(candidate.UpdatedAt) {
				candidate = task
			}
		}
	}
	if candidate == nil {
		for _, task := range m.tasks {
			if task.AssigneeAgentID == agentID && task.Status == domain.TaskTodo {
				if candidate == nil || task.Position < candidate.Position {
					candidate = task
				}
			}
		}
	}
	if candidate == nil {
		return nil, ErrNotFound
	}
	if candidate.Status != domain.TaskInProgress {
		candidate.Status = domain.TaskInProgress
		candidate.Position = m.nextTaskPosition(candidate.BoardID, domain.TaskInProgress)
	}
	candidate.UpdatedAt = now
	exec := &domain.TaskExecution{
		ID:             uuid.NewString(),
		TaskID:         candidate.ID,
		AgentID:        agentID,
		WorkerID:       workerID,
		FencingToken:   uuid.NewString(),
		Status:         domain.TaskExecutionActive,
		LeaseExpiresAt: now.Add(leaseFor),
		StartedAt:      now,
		UpdatedAt:      now,
	}
	m.taskExecs[exec.ID] = exec
	return taskWithExecution(candidate, exec), nil
}

func (m *MemoryStore) HeartbeatTaskExecution(_ context.Context, agentID string, executionID string, fencingToken string, leaseFor time.Duration) (*domain.TaskExecution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	exec, ok := m.taskExecs[executionID]
	if !ok || exec.AgentID != agentID || exec.FencingToken != fencingToken {
		return nil, ErrConflict
	}
	if exec.Status != domain.TaskExecutionActive {
		return nil, ErrConflict
	}
	if leaseFor <= 0 {
		leaseFor = 5 * time.Minute
	}
	now := time.Now().UTC()
	exec.LeaseExpiresAt = now.Add(leaseFor)
	exec.UpdatedAt = now
	return cloneTaskExecution(exec), nil
}

func (m *MemoryStore) CompleteTaskExecution(_ context.Context, agentID string, taskID string, executionID string, fencingToken string, status domain.TaskStatus, summary string) (*domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	exec, ok := m.taskExecs[executionID]
	if !ok || exec.AgentID != agentID || exec.TaskID != taskID || exec.FencingToken != fencingToken {
		return nil, ErrConflict
	}
	if exec.Status != domain.TaskExecutionActive {
		return nil, ErrConflict
	}
	task, ok := m.tasks[taskID]
	if !ok || task.AssigneeAgentID != agentID {
		return nil, ErrNotFound
	}
	if status != domain.TaskInReview && status != domain.TaskDone && status != domain.TaskBlocked {
		return nil, ErrConflict
	}
	now := time.Now().UTC()
	task.Status = status
	task.Position = m.nextTaskPosition(task.BoardID, status)
	task.UpdatedAt = now
	if status == domain.TaskBlocked {
		exec.Status = domain.TaskExecutionBlocked
	} else {
		exec.Status = domain.TaskExecutionCompleted
	}
	exec.ResultStatus = status
	exec.ResultSummary = summary
	exec.CompletedAt = now
	exec.UpdatedAt = now
	return taskWithExecution(task, exec), nil
}

func (m *MemoryStore) ListBoardTaskExecutions(_ context.Context, boardID string) ([]*domain.TaskExecution, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*domain.TaskExecution{}
	for _, exec := range m.taskExecs {
		if exec.Status != domain.TaskExecutionActive {
			continue
		}
		task, ok := m.tasks[exec.TaskID]
		if !ok || task.BoardID != boardID {
			continue
		}
		out = append(out, cloneTaskExecution(exec))
	}
	return out, nil
}

// ReapExpiredTaskExecutions expires dead attempts and re-queues their tasks.
// Mirrors the Postgres implementation: only active attempts whose lease
// lapsed before cutoff are expired, and a task is re-queued to todo only
// when no other live attempt remains.
func (m *MemoryStore) ReapExpiredTaskExecutions(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	reaped := 0
	reapedTasks := map[string]struct{}{}
	for _, exec := range m.taskExecs {
		if exec.Status != domain.TaskExecutionActive || !exec.LeaseExpiresAt.Before(cutoff) {
			continue
		}
		exec.Status = domain.TaskExecutionExpired
		exec.ResultSummary = "lease expired without completion"
		exec.CompletedAt = now
		exec.UpdatedAt = now
		reaped++
		reapedTasks[exec.TaskID] = struct{}{}
	}
	for taskID := range reapedTasks {
		task, ok := m.tasks[taskID]
		if !ok || task.Status != domain.TaskInProgress {
			continue
		}
		if m.taskHasActiveExecutionLocked(taskID, now) {
			continue
		}
		task.Status = domain.TaskTodo
		task.Position = m.nextTaskPosition(task.BoardID, domain.TaskTodo)
		task.UpdatedAt = now
	}
	return reaped, nil
}

func (m *MemoryStore) taskHasActiveExecutionLocked(taskID string, now time.Time) bool {
	for _, exec := range m.taskExecs {
		if exec.TaskID == taskID && exec.Status == domain.TaskExecutionActive && exec.LeaseExpiresAt.After(now) {
			return true
		}
	}
	return false
}

func (m *MemoryStore) CreateAgentMemory(_ context.Context, memory *domain.AgentMemory) (*domain.AgentMemory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[memory.AgentID]; !ok {
		return nil, ErrNotFound
	}
	if memory.SquadID != "" {
		if _, ok := m.squads[memory.SquadID]; !ok {
			return nil, ErrNotFound
		}
	}
	if memory.SourceTaskID != "" {
		if _, ok := m.tasks[memory.SourceTaskID]; !ok {
			return nil, ErrNotFound
		}
	}
	created := cloneAgentMemory(memory)
	created.ID = uuid.NewString()
	if len(created.Metadata) == 0 {
		created.Metadata = []byte(`{}`)
	}
	applyMemoryDefaults(created)
	created.CreatedAt = time.Now().UTC()
	m.agentMemory[created.ID] = created
	return cloneAgentMemory(created), nil
}

func (m *MemoryStore) ListAgentMemory(_ context.Context, agentID string, squadID string, queryEmbedding []float64, limit int) ([]*domain.AgentMemory, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.agents[agentID]; !ok {
		return nil, ErrNotFound
	}
	if limit <= 0 {
		limit = 10
	}
	out := []*domain.AgentMemory{}
	for _, item := range m.agentMemory {
		if item.AgentID == agentID && (item.SquadID == "" || item.SquadID == squadID) {
			out = append(out, cloneAgentMemory(item))
		}
	}
	if len(queryEmbedding) > 0 {
		slices.SortFunc(out, func(a, b *domain.AgentMemory) int {
			aScore, aOK := cosineSimilarity(a.Embedding, queryEmbedding)
			bScore, bOK := cosineSimilarity(b.Embedding, queryEmbedding)
			if aOK != bOK {
				if aOK {
					return -1
				}
				return 1
			}
			if aOK && bOK && aScore != bScore {
				if aScore > bScore {
					return -1
				}
				return 1
			}
			return compareMemoryRecency(a, b)
		})
	} else {
		slices.SortFunc(out, compareMemoryRecency)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func applyMemoryDefaults(memory *domain.AgentMemory) {
	if memory.TrustLevel == "" {
		memory.TrustLevel = "raw_model_output"
	}
	if memory.Provenance == "" {
		memory.Provenance = "task_completion"
	}
	if memory.ReviewStatus == "" {
		memory.ReviewStatus = "pending_review"
	}
	if memory.RawContent == "" && memory.TrustLevel == "raw_model_output" {
		memory.RawContent = memory.Content
	}
}

func compareMemoryRecency(a, b *domain.AgentMemory) int {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		if a.CreatedAt.After(b.CreatedAt) {
			return -1
		}
		return 1
	}
	return strings.Compare(a.ID, b.ID)
}

func cosineSimilarity(a, b []float64) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	var dot, aNorm, bNorm float64
	for i := range a {
		dot += a[i] * b[i]
		aNorm += a[i] * a[i]
		bNorm += b[i] * b[i]
	}
	if aNorm == 0 || bNorm == 0 {
		return 0, false
	}
	return dot / (math.Sqrt(aNorm) * math.Sqrt(bNorm)), true
}

func (m *MemoryStore) LeaseKubernetesOutbox(_ context.Context, limit int, leaseFor time.Duration) ([]*domain.KubernetesOutboxEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC()
	events := make([]*domain.KubernetesOutboxEvent, 0, len(m.k8sOutbox))
	for _, event := range m.k8sOutbox {
		if event.Status != domain.KubernetesOutboxPending && event.Status != domain.KubernetesOutboxFailed {
			continue
		}
		if event.NextAttemptAt.After(now) || (!event.LockedUntil.IsZero() && event.LockedUntil.After(now)) {
			continue
		}
		events = append(events, event)
	}
	slices.SortFunc(events, func(a, b *domain.KubernetesOutboxEvent) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	if len(events) > limit {
		events = events[:limit]
	}
	out := make([]*domain.KubernetesOutboxEvent, 0, len(events))
	for _, event := range events {
		event.LockedUntil = now.Add(leaseFor)
		event.UpdatedAt = now
		out = append(out, cloneKubernetesOutboxEvent(event))
	}
	return out, nil
}

func (m *MemoryStore) MarkKubernetesOutboxApplied(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	event, ok := m.k8sOutbox[id]
	if !ok {
		return ErrNotFound
	}
	event.Status = domain.KubernetesOutboxApplied
	event.LockedUntil = time.Time{}
	event.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *MemoryStore) MarkKubernetesOutboxFailed(_ context.Context, id string, lastError string, retryAfter time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	event, ok := m.k8sOutbox[id]
	if !ok {
		return ErrNotFound
	}
	event.Status = domain.KubernetesOutboxFailed
	event.Attempts++
	event.LastError = lastError
	event.NextAttemptAt = time.Now().UTC().Add(retryAfter)
	event.LockedUntil = time.Time{}
	event.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *MemoryStore) ListKubernetesOutbox(_ context.Context, status domain.KubernetesOutboxStatus, limit int) ([]*domain.KubernetesOutboxEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 {
		limit = 100
	}
	out := []*domain.KubernetesOutboxEvent{}
	for _, event := range m.k8sOutbox {
		if status == "" || event.Status == status {
			out = append(out, cloneKubernetesOutboxEvent(event))
		}
	}
	slices.SortFunc(out, func(a, b *domain.KubernetesOutboxEvent) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) CreateMessage(_ context.Context, msg *domain.Message) (*domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	target, ok := m.agents[msg.ToAgentID]
	if !ok {
		return nil, ErrNotFound
	}
	created := cloneMessage(msg)
	created.ID = uuid.NewString()
	created.SquadID = target.SquadID
	if len(created.Payload) == 0 {
		created.Payload = []byte(`{}`)
	}
	if created.Type == "" {
		created.Type = domain.MessageConsult
	}
	if created.Status == "" {
		created.Status = domain.MessagePending
	}
	if created.MaxAttempts <= 0 {
		created.MaxAttempts = defaultMessageMaxAttempts
	}
	if created.NextRetryAt.IsZero() {
		created.NextRetryAt = now
	}
	if created.ExpiresAt.IsZero() {
		created.ExpiresAt = now.Add(defaultMessageTTL)
	}
	created.CreatedAt = now
	m.messages[created.ID] = created
	return cloneMessage(created), nil
}

func (m *MemoryStore) ListPendingMessages(_ context.Context, agentID string) ([]*domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[agentID]; !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	out := []*domain.Message{}
	for _, msg := range m.messages {
		expireMessageIfDue(msg, now)
		if msg.ToAgentID == agentID && msg.Status == domain.MessagePending && !msg.NextRetryAt.After(now) {
			out = append(out, cloneMessage(msg))
		}
	}
	sortMessages(out)
	return out, nil
}

func (m *MemoryStore) HasPendingMessages(_ context.Context, agentID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[agentID]; !ok {
		return false, ErrNotFound
	}
	now := time.Now().UTC()
	for _, msg := range m.messages {
		expireMessageIfDue(msg, now)
		if msg.ToAgentID == agentID && msg.Status == domain.MessagePending {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemoryStore) WaitForAgentWork(ctx context.Context, agentID string, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.hasReadyWorkLocked(agentID, time.Now().UTC())
	}
	deadline := time.Now().UTC().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		available, err := m.hasReadyWorkLocked(agentID, time.Now().UTC())
		m.mu.Unlock()
		if available || err != nil {
			return available, err
		}
		if time.Now().UTC().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *MemoryStore) hasReadyWorkLocked(agentID string, now time.Time) (bool, error) {
	if _, ok := m.agents[agentID]; !ok {
		return false, ErrNotFound
	}
	for _, task := range m.tasks {
		if task.AssigneeAgentID == agentID && (task.Status == domain.TaskTodo || task.Status == domain.TaskInProgress) {
			return true, nil
		}
	}
	for _, msg := range m.messages {
		expireMessageIfDue(msg, now)
		if msg.ToAgentID == agentID && msg.Status == domain.MessagePending && !msg.NextRetryAt.After(now) {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemoryStore) ListAgentMessageHistory(_ context.Context, agentID string) ([]*domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[agentID]; !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	out := []*domain.Message{}
	for _, msg := range m.messages {
		expireMessageIfDue(msg, now)
		if msg.ToAgentID == agentID {
			out = append(out, cloneMessage(msg))
		}
	}
	sortMessages(out)
	return out, nil
}

func (m *MemoryStore) AckMessage(_ context.Context, agentID string, messageID string) (*domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.messages[messageID]
	if !ok || msg.ToAgentID != agentID {
		return nil, ErrNotFound
	}
	if msg.Status == domain.MessagePending {
		msg.Status = domain.MessageDelivered
		msg.DeliveredAt = time.Now().UTC()
	}
	return cloneMessage(msg), nil
}

func (m *MemoryStore) FailMessage(_ context.Context, agentID string, messageID string, reason string) (*domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.messages[messageID]
	if !ok || msg.ToAgentID != agentID {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	expireMessageIfDue(msg, now)
	if msg.Status != domain.MessagePending {
		return cloneMessage(msg), nil
	}
	msg.Attempts++
	if msg.MaxAttempts <= 0 {
		msg.MaxAttempts = defaultMessageMaxAttempts
	}
	if msg.Attempts >= msg.MaxAttempts {
		msg.Status = domain.MessageDead
		msg.DeliveredAt = now
		msg.TerminalReason = trimMessageReason(reason)
		if msg.TerminalReason == "" {
			msg.TerminalReason = "retry attempts exhausted"
		}
		return cloneMessage(msg), nil
	}
	msg.NextRetryAt = now.Add(defaultMessageRetryDelay)
	return cloneMessage(msg), nil
}

func (m *MemoryStore) nextTaskPosition(boardID string, status domain.TaskStatus) int {
	next := 1
	for _, task := range m.tasks {
		if task.BoardID == boardID && task.Status == status && task.Position >= next {
			next = task.Position + 1
		}
	}
	return next
}

func sortMessages(messages []*domain.Message) {
	slices.SortFunc(messages, func(a, b *domain.Message) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.Before(b.CreatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func cloneUser(u *domain.User) *domain.User {
	if u == nil {
		return nil
	}
	v := *u
	return &v
}

func cloneSquad(s *domain.Squad) *domain.Squad {
	if s == nil {
		return nil
	}
	v := *s
	v.OperatingModel = slices.Clone(s.OperatingModel)
	return &v
}

func cloneAgent(a *domain.Agent) *domain.Agent {
	if a == nil {
		return nil
	}
	v := *a
	v.Permissions = slices.Clone(a.Permissions)
	return &v
}

func cloneAgentIdentity(i *domain.AgentIdentity) *domain.AgentIdentity {
	if i == nil {
		return nil
	}
	v := *i
	return &v
}

func cloneKubernetesOutboxEvent(event *domain.KubernetesOutboxEvent) *domain.KubernetesOutboxEvent {
	if event == nil {
		return nil
	}
	v := *event
	v.Payload = slices.Clone(event.Payload)
	return &v
}

func cloneBoard(b *domain.Board) *domain.Board {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

func cloneTask(t *domain.Task) *domain.Task {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

func taskWithExecution(t *domain.Task, exec *domain.TaskExecution) *domain.Task {
	out := cloneTask(t)
	if out == nil || exec == nil {
		return out
	}
	out.ExecutionID = exec.ID
	out.WorkerID = exec.WorkerID
	out.FencingToken = exec.FencingToken
	out.LeaseExpiresAt = exec.LeaseExpiresAt
	return out
}

func cloneTaskExecution(exec *domain.TaskExecution) *domain.TaskExecution {
	if exec == nil {
		return nil
	}
	v := *exec
	return &v
}

func cloneAgentMemory(memory *domain.AgentMemory) *domain.AgentMemory {
	if memory == nil {
		return nil
	}
	v := *memory
	v.Metadata = slices.Clone(memory.Metadata)
	v.Embedding = slices.Clone(memory.Embedding)
	return &v
}

func cloneMessage(m *domain.Message) *domain.Message {
	if m == nil {
		return nil
	}
	v := *m
	v.Payload = append([]byte(nil), m.Payload...)
	return &v
}

func expireMessageIfDue(msg *domain.Message, now time.Time) {
	if msg == nil || msg.Status != domain.MessagePending || msg.ExpiresAt.IsZero() || msg.ExpiresAt.After(now) {
		return
	}
	msg.Status = domain.MessageExpired
	msg.DeliveredAt = now
	if msg.TerminalReason == "" {
		msg.TerminalReason = "message expired before delivery"
	}
}

func trimMessageReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > maxMessageTerminalReason {
		return reason[:maxMessageTerminalReason]
	}
	return reason
}

func userOIDCKey(issuer, subject string) string {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return ""
	}
	return issuer + "\x00" + subject
}

func grantAllows(permissions string, action string) bool {
	action = strings.TrimSpace(strings.ToLower(action))
	for _, part := range strings.Split(permissions, ",") {
		permission := strings.TrimSpace(strings.ToLower(part))
		if permission == "*" || permission == "admin" || permission == action {
			return true
		}
		if action == "ping" && permission == "talk" {
			return true
		}
	}
	return false
}

func cloneGrant(g *domain.AccessGrant) *domain.AccessGrant {
	if g == nil {
		return nil
	}
	v := *g
	return &v
}

func cloneLLMProvider(p *domain.LLMProvider) *domain.LLMProvider {
	if p == nil {
		return nil
	}
	v := *p
	v.Models = slices.Clone(p.Models)
	v.Pricing = slices.Clone(p.Pricing)
	return &v
}

func cloneResource(r *domain.RegistryResource) *domain.RegistryResource {
	if r == nil {
		return nil
	}
	v := *r
	v.Manifest = slices.Clone(r.Manifest)
	return &v
}

func cloneAgentPermission(p *domain.AgentPermission) *domain.AgentPermission {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneMeteringEvent(event *domain.MeteringEvent) *domain.MeteringEvent {
	if event == nil {
		return nil
	}
	v := *event
	return &v
}

func cloneAuditEntry(entry *domain.AuditEntry) *domain.AuditEntry {
	if entry == nil {
		return nil
	}
	v := *entry
	v.Metadata = slices.Clone(entry.Metadata)
	return &v
}

func permissionKey(agentID string, typ domain.ResourceType, resourceID string) string {
	return agentID + ":" + string(typ) + ":" + resourceID
}
