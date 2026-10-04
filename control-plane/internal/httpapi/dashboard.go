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
	"slices"
	"strconv"
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

	payload := DashboardPayload{Scope: "personal"}
	if isAdmin {
		payload.Scope = "all"
	}
	payload.Squads = make([]DashboardSquad, 0, len(squads))

	for _, squad := range squads {
		entry, err := s.dashboardSquadEntry(r.Context(), squad, ownerNames)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		payload.Squads = append(payload.Squads, entry)
	}

	providers, err := s.store.ListLLMProviders(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	payload.Providers = s.probeProviders(r, providers)

	resources, err := s.dashboardResources(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	payload.Resources = resources

	writeJSON(w, http.StatusOK, payload)
}

// dashboardOwnerName resolves a display name for a squad owner, caching
// lookups per request. Extracted from getDashboard for cognitive
// complexity (S-126 / S3776).
func (s *Server) dashboardOwnerName(ctx context.Context, cache map[string]string, ownerID string) string {
	if ownerID == "" {
		return ""
	}
	if name, ok := cache[ownerID]; ok {
		return name
	}
	name := ""
	if owner, err := s.store.GetUser(ctx, ownerID); err == nil && owner != nil {
		name = owner.Name
		if name == "" {
			name = owner.Email
		}
	}
	cache[ownerID] = name
	return name
}

// dashboardSquadEntry builds the dashboard row for one squad: task status
// counts, cost aggregate, and its agents with per-agent cost. Extracted
// from getDashboard for cognitive complexity (S-126 / S3776).
func (s *Server) dashboardSquadEntry(ctx context.Context, squad *domain.Squad, ownerNames map[string]string) (DashboardSquad, error) {
	entry := DashboardSquad{
		ID:         squad.ID,
		Name:       squad.Name,
		Status:     string(squad.Status),
		OwnerID:    squad.OwnerID,
		OwnerName:  s.dashboardOwnerName(ctx, ownerNames, squad.OwnerID),
		TaskCounts: map[string]int{},
		Agents:     []DashboardAgent{},
	}

	if board, err := s.store.GetBoard(ctx, squad.ID); err == nil && board != nil {
		tasks, err := s.store.ListTasks(ctx, board.ID, "")
		if err != nil {
			return entry, err
		}
		for _, t := range tasks {
			entry.TaskCounts[string(t.Status)]++
		}
	}

	if usage, err := s.store.SumMetering(ctx, squad.ID, "", time.Time{}); err == nil && usage != nil {
		entry.Cost = costFromMetering(usage)
	}

	agents, err := s.store.ListAgents(ctx, squad.ID)
	if err != nil {
		return entry, err
	}
	for _, agent := range agents {
		da := DashboardAgent{
			ID:      agent.ID,
			SquadID: agent.SquadID,
			Name:    agent.Name,
			Role:    agent.Role,
			Status:  agent.Status,
		}
		if usage, err := s.store.SumMetering(ctx, "", agent.ID, time.Time{}); err == nil && usage != nil {
			da.Cost = costFromMetering(usage)
		}
		entry.Agents = append(entry.Agents, da)
	}
	return entry, nil
}

// dashboardResources collects the generic registry resources surfaced in
// the resources overview. Extracted from getDashboard for cognitive
// complexity (S-126 / S3776).
func (s *Server) dashboardResources(ctx context.Context) ([]DashboardResource, error) {
	out := []DashboardResource{}
	for _, typ := range dashboardResourceTypes {
		resources, err := s.store.ListResources(ctx, typ)
		if err != nil {
			return nil, err
		}
		for _, res := range resources {
			out = append(out, DashboardResource{
				ID:     res.ID,
				Type:   res.Type,
				Name:   res.Name,
				Status: res.Status,
			})
		}
	}
	return out, nil
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

// ---------------------------------------------------------------------------
// S-190: dashboard usage series (daily histograms + MTD totals).
//
// GET /api/v1/dashboard/usage?days=30 powers the dashboard histograms.
// Auth scoping mirrors getDashboard: platform admins get platform-wide
// series plus the `platform` block (total/MTD cost, user and agent
// counts); everyone else is scoped to owned + granted squads. The day
// axis is emitted server-side (UTC calendar days) so every series is
// zero-filled and aligned — the frontend never interpolates dates.
// ---------------------------------------------------------------------------

const (
	defaultUsageDays = 30
	maxUsageDays     = 90
	usageDayLayout   = "2006-01-02"
)

// UsagePoint is one day's aggregate for a series.
type UsagePoint struct {
	Day          string  `json:"day"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Tokens       int     `json:"tokens"`
	Cost         float64 `json:"cost"`
}

// UsageSeries is one histogram line: a squad (by_squad) or an agent
// (by_agent, with its squad for labeling). Points align 1:1 with the
// payload's Days axis.
type UsageSeries struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	SquadID   string       `json:"squad_id,omitempty"`
	SquadName string       `json:"squad_name,omitempty"`
	Points    []UsagePoint `json:"points"`
}

// ProviderModelUsage is one model's month-to-date usage under a provider.
type ProviderModelUsage struct {
	Model  string  `json:"model"`
	Tokens int     `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// ProviderUsage is a provider's month-to-date rollup plus per-model rows.
type ProviderUsage struct {
	ProviderID   string               `json:"provider_id"`
	ProviderName string               `json:"provider_name"`
	Tokens       int                  `json:"tokens"`
	Cost         float64              `json:"cost"`
	Models       []ProviderModelUsage `json:"models"`
}

// PlatformUsage is platform-admin-only: whole-platform cost and counts.
type PlatformUsage struct {
	TotalCost float64 `json:"total_cost"`
	MTDCost   float64 `json:"mtd_cost"`
	Users     int     `json:"users"`
	Agents    int     `json:"agents"`
}

type DashboardUsagePayload struct {
	Scope    string   `json:"scope"`
	Days     []string `json:"days"`
	MTDStart string   `json:"mtd_start"`
	Currency string   `json:"currency,omitempty"`
	// SquadMTDCost is the month-to-date cost across the caller's visible
	// squads (platform-wide for admins — same scoping rule as the series).
	SquadMTDCost float64         `json:"squad_mtd_cost"`
	BySquad      []UsageSeries   `json:"by_squad"`
	ByAgent      []UsageSeries   `json:"by_agent"`
	Providers    []ProviderUsage `json:"providers"`
	// Platform is present only for platform admins.
	Platform *PlatformUsage `json:"platform,omitempty"`
}

// usageDaysParam parses ?days= with a sane clamp so nobody asks for a year
// of buckets on a 30s poll.
func usageDaysParam(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("days"))
	if raw == "" {
		return defaultUsageDays
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return defaultUsageDays
	}
	if n > maxUsageDays {
		return maxUsageDays
	}
	return n
}

func (s *Server) getDashboardUsage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing principal")
		return
	}
	isAdmin := u.Role == domain.RolePlatformAdmin

	now := time.Now().UTC()
	days := usageDaysParam(r)
	axis := make([]string, days)
	for i := range axis {
		axis[i] = now.AddDate(0, 0, -(days - 1 - i)).Format(usageDayLayout)
	}
	since, err := time.Parse(usageDayLayout, axis[0])
	if err != nil { // unreachable: axis built from the same layout
		writeError(w, http.StatusInternalServerError, "bad_day_axis", err.Error())
		return
	}
	mtdStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	mtdDay := mtdStart.Format(usageDayLayout)

	squads, err := s.dashboardSquads(r, u.ID, isAdmin)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	squadIDs := make([]string, 0, len(squads))
	for _, sq := range squads {
		squadIDs = append(squadIDs, sq.ID)
	}

	payload := DashboardUsagePayload{
		Scope:     "personal",
		Days:      axis,
		MTDStart:  mtdDay,
		Currency:  "USD",
		BySquad:   []UsageSeries{},
		ByAgent:   []UsageSeries{},
		Providers: []ProviderUsage{},
	}
	if isAdmin {
		payload.Scope = "all"
	}

	// A non-admin with no visible squads has nothing to aggregate; skip
	// the query so the empty allowlist is never mistaken for "all".
	if len(squadIDs) > 0 || isAdmin {
		var filter []string
		if !isAdmin {
			filter = squadIDs
		}
		rows, err := s.store.SumMeteringDaily(r.Context(), since, filter)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		buildUsageSeries(&payload, rows)
	}

	if isAdmin {
		platform, err := s.platformUsage(r.Context(), mtdStart)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		payload.Platform = platform
	}

	writeJSON(w, http.StatusOK, payload)
}

// getOrCreateSeriesAcc returns the accumulator for id, creating it with
// the given metadata on first sight.
func getOrCreateSeriesAcc(acc map[string]*usageSeriesAcc, id string, meta UsageSeries) *usageSeriesAcc {
	a, ok := acc[id]
	if !ok {
		a = &usageSeriesAcc{meta: meta, byDay: map[string]*UsagePoint{}}
		acc[id] = a
	}
	return a
}

// accumulateProviderModel folds one daily row into the month-to-date
// provider and provider/model rollups.
func accumulateProviderModel(provAcc map[string]*ProviderUsage, modelAcc map[string]map[string]*ProviderModelUsage, row domain.MeteringDailyRow) {
	prov := provAcc[row.ProviderID]
	if prov == nil {
		name := row.ProviderName
		if name == "" {
			name = "unknown provider"
		}
		prov = &ProviderUsage{ProviderID: row.ProviderID, ProviderName: name, Models: []ProviderModelUsage{}}
		provAcc[row.ProviderID] = prov
		modelAcc[row.ProviderID] = map[string]*ProviderModelUsage{}
	}
	model := row.Model
	if model == "" {
		model = "unknown model"
	}
	mm := modelAcc[row.ProviderID]
	mu, ok := mm[model]
	if !ok {
		mu = &ProviderModelUsage{Model: model}
		mm[model] = mu
	}
	mu.Tokens += row.InputTokens + row.OutputTokens
	mu.Cost += row.Cost
	prov.Tokens += row.InputTokens + row.OutputTokens
	prov.Cost += row.Cost
}

// usageSeriesAcc accumulates one series' per-day points inside
// buildUsageSeries.
type usageSeriesAcc struct {
	meta  UsageSeries
	byDay map[string]*UsagePoint
}

// buildUsageSeries folds daily rows into squad series, agent series and
// the provider/model MTD rollup. Series are sorted by name so chart
// colors stay stable between polls.
func buildUsageSeries(payload *DashboardUsagePayload, rows []domain.MeteringDailyRow) {
	squadAcc := map[string]*usageSeriesAcc{}
	agentAcc := map[string]*usageSeriesAcc{}
	provAcc := map[string]*ProviderUsage{}
	modelAcc := map[string]map[string]*ProviderModelUsage{}

	for _, row := range rows {
		if row.Currency != "" {
			payload.Currency = row.Currency
		}
		if row.Day >= payload.MTDStart {
			payload.SquadMTDCost += row.Cost
		}

		squad := getOrCreateSeriesAcc(squadAcc, row.SquadID, UsageSeries{ID: row.SquadID, Name: row.SquadName})
		accumulateUsagePoint(squad, row)

		agent := getOrCreateSeriesAcc(agentAcc, row.AgentID, UsageSeries{
			ID: row.AgentID, Name: row.AgentName, SquadID: row.SquadID, SquadName: row.SquadName,
		})
		accumulateUsagePoint(agent, row)

		// Provider/model rollup is month-to-date only.
		if row.Day < payload.MTDStart {
			continue
		}
		accumulateProviderModel(provAcc, modelAcc, row)
	}

	payload.BySquad = finalizeUsageSeries(payload.Days, squadAcc)
	payload.ByAgent = finalizeUsageSeries(payload.Days, agentAcc)

	for pid, prov := range provAcc {
		models := make([]ProviderModelUsage, 0, len(modelAcc[pid]))
		for _, mu := range modelAcc[pid] {
			models = append(models, *mu)
		}
		slices.SortFunc(models, func(a, b ProviderModelUsage) int { return strings.Compare(a.Model, b.Model) })
		prov.Models = models
		payload.Providers = append(payload.Providers, *prov)
	}
	slices.SortFunc(payload.Providers, func(a, b ProviderUsage) int {
		if c := strings.Compare(a.ProviderName, b.ProviderName); c != 0 {
			return c
		}
		return strings.Compare(a.ProviderID, b.ProviderID)
	})
}

func accumulateUsagePoint(acc *usageSeriesAcc, row domain.MeteringDailyRow) {
	point, ok := acc.byDay[row.Day]
	if !ok {
		point = &UsagePoint{Day: row.Day}
		acc.byDay[row.Day] = point
	}
	point.InputTokens += row.InputTokens
	point.OutputTokens += row.OutputTokens
	point.Tokens += row.InputTokens + row.OutputTokens
	point.Cost += row.Cost
}

// finalizeUsageSeries flattens accumulators into name-sorted series whose
// points align 1:1 with the day axis, zero-filling missing days.
func finalizeUsageSeries(days []string, acc map[string]*usageSeriesAcc) []UsageSeries {
	out := make([]UsageSeries, 0, len(acc))
	for _, a := range acc {
		s := a.meta
		s.Points = make([]UsagePoint, 0, len(days))
		for _, day := range days {
			if point, ok := a.byDay[day]; ok {
				s.Points = append(s.Points, *point)
			} else {
				s.Points = append(s.Points, UsagePoint{Day: day})
			}
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b UsageSeries) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (s *Server) platformUsage(ctx context.Context, mtdStart time.Time) (*PlatformUsage, error) {
	total, err := s.store.SumMetering(ctx, "", "", time.Time{})
	if err != nil {
		return nil, err
	}
	mtd, err := s.store.SumMetering(ctx, "", "", mtdStart)
	if err != nil {
		return nil, err
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	agents, err := s.store.ListAllAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := &PlatformUsage{Users: len(users), Agents: len(agents)}
	if total != nil {
		out.TotalCost = total.Cost
	}
	if mtd != nil {
		out.MTDCost = mtd.Cost
	}
	return out, nil
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
