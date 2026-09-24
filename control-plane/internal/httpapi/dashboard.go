package httpapi

// Dashboard aggregation (S-116).
//
// GET /api/v1/dashboard returns everything the landing page needs in ONE
// request so the UI does not waterfall N+1 calls per squad/agent. Scoping:
//   - platform_admin → every squad ("all").
//   - everyone else  → owned squads + squads granted to them ("read" action,
//     the same grant semantics loadAccessibleSquad enforces). This closes the
//     S-121 gap for the dashboard: ListSquads filters by owner only, so we
//     enumerate all squads and keep the granted ones via UserMayAccessSquad.
//
// Provider liveness: the registry tracks lifecycle (active/deprecated), not
// reachability, and nothing in llm-gateway exposes a health cache. So we run
// a lightweight HTTP probe of each active provider's base_url with a short
// timeout. Any HTTP response (even 4xx) means the endpoint is up; only a
// connection error/timeout marks it offline. Deprecated providers are not
// probed.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// dashboardProbeTimeout bounds each provider liveness probe. Package-level so
// tests can shrink it.
var dashboardProbeTimeout = 2 * time.Second

// dashboardResourceTypes are the generic registry types surfaced in the
// resources overview. llm_provider is excluded — it has its own section.
var dashboardResourceTypes = []domain.ResourceType{
	domain.ResSkill,
	domain.ResTool,
	domain.ResAPI,
	domain.ResKnowledgeBase,
	domain.ResProjectWorkspace,
}

// DashboardCost mirrors the metering aggregate shape the UI already renders
// via MeteringSummary.
type DashboardCost struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
	Currency     string  `json:"currency,omitempty"`
}

type DashboardAgent struct {
	ID      string             `json:"id"`
	SquadID string             `json:"squad_id"`
	Name    string             `json:"name"`
	Role    string             `json:"role,omitempty"`
	Status  domain.AgentStatus `json:"status"`
	Cost    *DashboardCost     `json:"cost,omitempty"`
}

type DashboardSquad struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Status     string           `json:"status,omitempty"`
	OwnerID    string           `json:"owner_id,omitempty"`
	OwnerName  string           `json:"owner_name,omitempty"`
	TaskCounts map[string]int   `json:"task_counts"`
	Cost       *DashboardCost   `json:"cost,omitempty"`
	Agents     []DashboardAgent `json:"agents"`
}

type DashboardProvider struct {
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Kind      string                `json:"kind,omitempty"`
	BaseURL   string                `json:"base_url,omitempty"`
	Status    domain.ResourceStatus `json:"status"`
	Online    bool                  `json:"online"`
	LatencyMS int64                 `json:"latency_ms,omitempty"`
	Error     string                `json:"error,omitempty"`
}

type DashboardResource struct {
	ID     string                `json:"id"`
	Type   domain.ResourceType   `json:"type"`
	Name   string                `json:"name"`
	Status domain.ResourceStatus `json:"status"`
}

type DashboardPayload struct {
	// Scope is "all" for platform admins, "personal" otherwise.
	Scope     string              `json:"scope"`
	Squads    []DashboardSquad    `json:"squads"`
	Providers []DashboardProvider `json:"providers"`
	Resources []DashboardResource `json:"resources"`
}

func (s *Server) getDashboard(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing principal")
		return
	}
	isAdmin := u.Role == domain.RolePlatformAdmin

	squads, err := s.dashboardSquads(r, u.ID, isAdmin)
	if err != nil {
		writeStorageError(w, err)
		return
	}

	ownerNames := map[string]string{}
	ownerName := func(ownerID string) string {
		if ownerID == "" {
			return ""
		}
		if name, ok := ownerNames[ownerID]; ok {
			return name
		}
		name := ""
		if owner, err := s.store.GetUser(r.Context(), ownerID); err == nil && owner != nil {
			name = owner.Name
			if name == "" {
				name = owner.Email
			}
		}
		ownerNames[ownerID] = name
		return name
	}

	payload := DashboardPayload{Scope: "personal"}
	if isAdmin {
		payload.Scope = "all"
	}
	payload.Squads = make([]DashboardSquad, 0, len(squads))

	for _, squad := range squads {
		entry := DashboardSquad{
			ID:         squad.ID,
			Name:       squad.Name,
			Status:     string(squad.Status),
			OwnerID:    squad.OwnerID,
			OwnerName:  ownerName(squad.OwnerID),
			TaskCounts: map[string]int{},
			Agents:     []DashboardAgent{},
		}

		if board, err := s.store.GetBoard(r.Context(), squad.ID); err == nil && board != nil {
			tasks, err := s.store.ListTasks(r.Context(), board.ID, "")
			if err != nil {
				writeStorageError(w, err)
				return
			}
			for _, t := range tasks {
				entry.TaskCounts[string(t.Status)]++
			}
		}

		if usage, err := s.store.SumMetering(r.Context(), squad.ID, ""); err == nil && usage != nil {
			entry.Cost = costFromMetering(usage)
		}

		agents, err := s.store.ListAgents(r.Context(), squad.ID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		for _, agent := range agents {
			da := DashboardAgent{
				ID:      agent.ID,
				SquadID: agent.SquadID,
				Name:    agent.Name,
				Role:    agent.Role,
				Status:  agent.Status,
			}
			if usage, err := s.store.SumMetering(r.Context(), "", agent.ID); err == nil && usage != nil {
				da.Cost = costFromMetering(usage)
			}
			entry.Agents = append(entry.Agents, da)
		}
		payload.Squads = append(payload.Squads, entry)
	}

	providers, err := s.store.ListLLMProviders(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	payload.Providers = s.probeProviders(r, providers)

	payload.Resources = []DashboardResource{}
	for _, typ := range dashboardResourceTypes {
		resources, err := s.store.ListResources(r.Context(), typ)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		for _, res := range resources {
			payload.Resources = append(payload.Resources, DashboardResource{
				ID:     res.ID,
				Type:   res.Type,
				Name:   res.Name,
				Status: res.Status,
			})
		}
	}

	writeJSON(w, http.StatusOK, payload)
}

// dashboardSquads resolves the squads visible to the caller: everything for
// platform admins, owned + granted ("read") for everyone else.
func (s *Server) dashboardSquads(r *http.Request, userID string, isAdmin bool) ([]*domain.Squad, error) {
	if isAdmin {
		return s.store.ListSquads(r.Context(), "")
	}
	owned, err := s.store.ListSquads(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	all, err := s.store.ListSquads(r.Context(), "")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(owned))
	for _, sq := range owned {
		seen[sq.ID] = true
	}
	out := owned
	for _, sq := range all {
		if seen[sq.ID] {
			continue
		}
		ok, err := s.store.UserMayAccessSquad(r.Context(), userID, sq.ID, "read")
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, sq)
		}
	}
	return out, nil
}

func costFromMetering(ev *domain.MeteringEvent) *DashboardCost {
	if ev == nil {
		return nil
	}
	return &DashboardCost{
		InputTokens:  ev.InputTokens,
		OutputTokens: ev.OutputTokens,
		Cost:         ev.Cost,
		Currency:     ev.Currency,
	}
}

// probeProviders checks each provider's endpoint concurrently. Active
// providers get an HTTP GET with a short timeout; any HTTP response counts
// as online. Inactive (deprecated) providers are reported offline without a
// probe.
func (s *Server) probeProviders(r *http.Request, providers []*domain.LLMProvider) []DashboardProvider {
	out := make([]DashboardProvider, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		out[i] = DashboardProvider{
			ID:     p.ID,
			Name:   p.Name,
			Kind:   p.Kind,
			Status: p.Status,
		}
		if p.Status != domain.ResourceActive {
			out[i].Error = "not probed (inactive)"
			continue
		}
		baseURL := strings.TrimSpace(p.BaseURL)
		if baseURL == "" {
			out[i].Error = "no base_url registered"
			continue
		}
		out[i].BaseURL = baseURL
		wg.Add(1)
		go func(idx int, url string) {
			defer wg.Done()
			online, latency, probeErr := probeEndpoint(r.Context(), url)
			out[idx].Online = online
			out[idx].LatencyMS = latency
			out[idx].Error = probeErr
		}(i, baseURL)
	}
	wg.Wait()
	return out
}

// probeEndpoint performs a lightweight GET and reports reachability. Any
// HTTP status code means the endpoint answered; only transport failures or
// the timeout count as offline.
func probeEndpoint(ctx context.Context, url string) (online bool, latencyMS int64, probeErr string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, dashboardProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, 0, "invalid base_url"
	}
	resp, err := http.DefaultClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return false, latency, "unreachable"
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}()
	return true, latency, ""
}
