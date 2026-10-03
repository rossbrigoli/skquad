package httpapi

// S-203 WP1: budget control-plane API + scoped cost aggregation.
//
// Budgets (platform_admin only):
//   GET/PUT /api/v1/admin/budgets/platform        — default / max / platform-wide limit
//   GET/PUT /api/v1/admin/budgets/users/{userID}  — per-user monthly budget + spend status
//   GET     /api/v1/admin/budgets                 — admin overview (all users + platform)
//
// Lowering the global max clamps every user budget above it in the same
// request (epic S-203 req 7) — the clamp is a single store UPDATE, so
// there is no window where an over-max budget is visible.
//
// Cost aggregation (any authenticated user, scoped):
//   GET /api/v1/costs/summary?days=N — total / running (month-to-date)
//   cost plus daily series grouped by squad, agent, model and provider.
//   Scoping mirrors the dashboard: platform admins see everything,
//   everyone else only owned + granted squads. The budget block exposes
//   the caller's own current-month spend vs budget so WP2/WP3 can
//   render/consume it without extra round-trips.
//
// A user's spend is measured over the squads they OWN (the budget covers
// the user's squads, not squads merely granted to them for reading).

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// maxBudgetUSD bounds every budget value accepted from the API.
const maxBudgetUSD = 1_000_000_000

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

type userBudgetStatusView struct {
	UserID           string   `json:"user_id"`
	Email            string   `json:"email,omitempty"`
	Name             string   `json:"name,omitempty"`
	MonthlyBudgetUSD *float64 `json:"monthly_budget_usd"`
	MTDCost          float64  `json:"mtd_cost"`
	RemainingUSD     *float64 `json:"remaining_usd"`
	OverBudget       bool     `json:"over_budget"`
	UpdatedAt        string   `json:"updated_at,omitempty"`
	UpdatedBy        string   `json:"updated_by,omitempty"`
}

type platformBudgetView struct {
	DefaultMonthlyUSD       *float64 `json:"default_monthly_usd"`
	MaxUSD                  *float64 `json:"max_usd"`
	PlatformMonthlyLimitUSD *float64 `json:"platform_monthly_limit_usd"`
	PlatformMTDCost         float64  `json:"platform_mtd_cost"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// validateBudgetRange checks a submitted budget amount at the API
// boundary (the store accepts anything; policy lives here).
func validateBudgetRange(w http.ResponseWriter, amount float64) bool {
	if amount < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "budget must not be negative")
		return false
	}
	if amount > maxBudgetUSD {
		writeError(w, http.StatusBadRequest, "bad_request", "budget exceeds the accepted maximum")
		return false
	}
	return true
}

// userMTDCost sums the current-calendar-month metering cost across the
// squads owned by userID. A user with no squads costs nothing.
func (s *Server) userMTDCost(r *http.Request, userID string, mtdStart time.Time) (float64, error) {
	return s.userMTDCostCtx(r.Context(), userID, mtdStart)
}

// userMTDCostCtx is the context-based variant used by the WP3
// enforcement paths (no HTTP request in scope).
func (s *Server) userMTDCostCtx(ctx context.Context, userID string, mtdStart time.Time) (float64, error) {
	squads, err := s.store.ListSquads(ctx, userID)
	if err != nil {
		return 0, err
	}
	if len(squads) == 0 {
		return 0, nil
	}
	squadIDs := make([]string, 0, len(squads))
	for _, sq := range squads {
		squadIDs = append(squadIDs, sq.ID)
	}
	rows, err := s.store.SumMeteringDaily(ctx, mtdStart, squadIDs)
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, row := range rows {
		total += row.Cost
	}
	return total, nil
}

func (s *Server) platformMTDCost(r *http.Request, mtdStart time.Time) (float64, error) {
	return s.platformMTDCostCtx(r.Context(), mtdStart)
}

// platformMTDCostCtx sums the month-to-date metering cost across all
// squads (WP3 platform-limit evaluation).
func (s *Server) platformMTDCostCtx(ctx context.Context, mtdStart time.Time) (float64, error) {
	rows, err := s.store.SumMeteringDaily(ctx, mtdStart, nil)
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, row := range rows {
		total += row.Cost
	}
	return total, nil
}

// buildUserBudgetStatus assembles the spend-vs-budget view for one user.
func (s *Server) buildUserBudgetStatus(r *http.Request, u *domain.User, mtdStart time.Time) (userBudgetStatusView, error) {
	view := userBudgetStatusView{UserID: u.ID, Email: u.Email, Name: u.Name}
	mtd, err := s.userMTDCost(r, u.ID, mtdStart)
	if err != nil {
		return view, err
	}
	view.MTDCost = mtd
	budget, err := s.store.GetUserBudget(r.Context(), u.ID)
	switch {
	case err == nil && budget != nil:
		view.MonthlyBudgetUSD = &budget.MonthlyBudgetUSD
		remaining := budget.MonthlyBudgetUSD - mtd
		view.RemainingUSD = &remaining
		view.OverBudget = mtd > budget.MonthlyBudgetUSD
		view.UpdatedAt = budget.UpdatedAt.Format(time.RFC3339)
		view.UpdatedBy = budget.UpdatedBy
	case errors.Is(err, storage.ErrNotFound):
		// No budget set: unlimited (subject only to platform limits).
	default:
		return view, err
	}
	return view, nil
}

func mtdStartUTC(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// ensureDefaultBudget auto-applies the platform default budget to a
// user that has no budget row yet (S-203 WP1: user creation / first
// login). Idempotent and best-effort: a budget write failure is logged
// but must never block authentication.
func (s *Server) ensureDefaultBudget(r *http.Request, userID string) {
	if _, err := s.store.EnsureUserDefaultBudget(r.Context(), userID); err != nil {
		log.Printf("budget: could not apply platform default to user %s: %v", userID, err)
	}
}

// ---------------------------------------------------------------------------
// Platform budget endpoints (platform_admin only)
// ---------------------------------------------------------------------------

func (s *Server) getPlatformBudget(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	budgets, err := s.store.GetPlatformBudgets(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	mtd, err := s.platformMTDCost(r, mtdStartUTC(time.Now().UTC()))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, platformBudgetView{
		DefaultMonthlyUSD:       budgets.DefaultMonthlyUSD,
		MaxUSD:                  budgets.MaxUSD,
		PlatformMonthlyLimitUSD: budgets.PlatformMonthlyLimitUSD,
		PlatformMTDCost:         mtd,
	})
}

// putPlatformBudget sets any subset of the platform budget knobs. An
// explicit null clears the knob. Setting the max clamps every user
// budget above the new max in the same operation (epic S-203 req 7).
func (s *Server) putPlatformBudget(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	// Decode into a raw map so an explicit null (clear the knob) is
	// distinguishable from an absent field (leave untouched): Go nils
	// pointer fields on null, so presence must be read from the map.
	var fields map[string]json.RawMessage
	if !decodeJSON(w, r, &fields) {
		return
	}
	if len(fields) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request",
			"at least one of default_monthly_usd / max_usd / platform_monthly_limit_usd is required")
		return
	}
	known := map[string]bool{
		"default_monthly_usd":        true,
		"max_usd":                    true,
		"platform_monthly_limit_usd": true,
	}
	for k := range fields {
		if !known[k] {
			writeError(w, http.StatusBadRequest, "bad_request", "unknown field: "+k)
			return
		}
	}
	parseKnob := func(name string) (*float64, bool) {
		raw, present := fields[name]
		if !present {
			return nil, false
		}
		if string(raw) == "null" {
			return nil, true // explicit clear
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false
		}
		return &v, true
	}
	defVal, defSet := parseKnob("default_monthly_usd")
	maxVal, maxSet := parseKnob("max_usd")
	limitVal, limitSet := parseKnob("platform_monthly_limit_usd")
	if (defSet && defVal == nil && fields["default_monthly_usd"] != nil && string(fields["default_monthly_usd"]) != "null") ||
		(maxSet && maxVal == nil && string(fields["max_usd"]) != "null") ||
		(limitSet && limitVal == nil && string(fields["platform_monthly_limit_usd"]) != "null") {
		writeError(w, http.StatusBadRequest, "bad_request", "budget values must be numbers or null")
		return
	}
	for _, v := range []*float64{defVal, maxVal, limitVal} {
		if v != nil && !validateBudgetRange(w, *v) {
			return
		}
	}

	current, err := s.store.GetPlatformBudgets(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	// Consistency: the default may not exceed the max (either value in
	// this request or the one already stored).
	effMax := current.MaxUSD
	if maxSet {
		effMax = maxVal
	}
	effDefault := current.DefaultMonthlyUSD
	if defSet {
		effDefault = defVal
	}
	if effMax != nil && effDefault != nil && *effDefault > *effMax {
		writeError(w, http.StatusBadRequest, "bad_request",
			"default_monthly_usd must not exceed max_usd")
		return
	}

	userID := currentUser(r.Context()).ID
	writes := []struct {
		key   string
		value *float64
		set   bool
	}{
		{domain.PlatformSettingBudgetDefaultUSD, defVal, defSet},
		{domain.PlatformSettingBudgetMaxUSD, maxVal, maxSet},
		{domain.PlatformSettingBudgetPlatformMonthlyLimitUSD, limitVal, limitSet},
	}
	for _, wr := range writes {
		if !wr.set {
			continue
		}
		if err := s.store.SetPlatformBudgetSetting(r.Context(), wr.key, wr.value, userID); err != nil {
			writeStorageError(w, err)
			return
		}
	}

	resp := map[string]any{}
	if maxSet && maxVal != nil {
		clamped, err := s.store.ClampUserBudgetsToMax(r.Context(), *maxVal, userID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		resp["clamped_user_budgets"] = clamped
	}

	meta, _ := json.Marshal(map[string]any{
		"default_monthly_usd":        defVal,
		"max_usd":                    maxVal,
		"platform_monthly_limit_usd": limitVal,
		"clamped_user_budgets":       resp["clamped_user_budgets"],
	})
	_ = s.recordSystemAudit(r.Context(), "budget.platform.update", "platform_settings", "", "", meta)

	// S-203 WP3: platform budget changes can newly block (lowered
	// limit/clamped budgets) or resume (raised limit) users — sweep the
	// whole platform so enforcement tracks the change immediately.
	s.sweepBudgets(r.Context())

	budgets, err := s.store.GetPlatformBudgets(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	resp["default_monthly_usd"] = budgets.DefaultMonthlyUSD
	resp["max_usd"] = budgets.MaxUSD
	resp["platform_monthly_limit_usd"] = budgets.PlatformMonthlyLimitUSD
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Per-user budget endpoints (platform_admin only)
// ---------------------------------------------------------------------------

func (s *Server) getUserBudgetAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	userID := chi.URLParam(r, "userID")
	u, err := s.store.GetUser(r.Context(), userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	view, err := s.buildUserBudgetStatus(r, u, mtdStartUTC(time.Now().UTC()))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// putUserBudgetAdmin sets one user's monthly budget. The value must not
// exceed the platform max (lowering the max clamps instead — see
// putPlatformBudget).
func (s *Server) putUserBudgetAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	userID := chi.URLParam(r, "userID")
	var req struct {
		MonthlyBudgetUSD *float64 `json:"monthly_budget_usd"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.MonthlyBudgetUSD == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "monthly_budget_usd is required")
		return
	}
	if !validateBudgetRange(w, *req.MonthlyBudgetUSD) {
		return
	}
	if _, err := s.store.GetUser(r.Context(), userID); err != nil {
		writeStorageError(w, err)
		return
	}
	budgets, err := s.store.GetPlatformBudgets(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if budgets.MaxUSD != nil && *req.MonthlyBudgetUSD > *budgets.MaxUSD {
		writeError(w, http.StatusBadRequest, "budget_exceeds_max",
			"user budget must not exceed the platform max")
		return
	}
	adminID := currentUser(r.Context()).ID
	if err := s.store.SetUserBudget(r.Context(), userID, *req.MonthlyBudgetUSD, adminID); err != nil {
		writeStorageError(w, err)
		return
	}
	meta, _ := json.Marshal(map[string]any{"user_id": userID, "monthly_budget_usd": *req.MonthlyBudgetUSD})
	_ = s.recordSystemAudit(r.Context(), "budget.user.update", "user", userID, "", meta)
	// S-203 WP3: re-evaluate the user so raising the budget above the
	// current MTD spend clears the block and resumes scheduling, while
	// lowering it below spend blocks (and notifies) immediately.
	if err := s.evaluateUserBudget(r.Context(), userID, time.Now().UTC(), budgets); err != nil {
		log.Printf("budget-enforce: post-update evaluation failed for %s: %v", userID, err)
	}
	blocked := false
	if block, err := s.store.GetBudgetBlock(r.Context(), userID); err == nil {
		blocked = block.EffectiveBlocked(currentBudgetPeriod(time.Now()))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":            userID,
		"monthly_budget_usd": *req.MonthlyBudgetUSD,
		"blocked":            blocked,
	})
}

// delete... intentionally omitted in WP1: clearing a budget is done by
// setting it to the platform default or 0 until WP3 needs the null.

// listBudgetsAdmin returns the platform budget block plus every user's
// budget + current-month spend (admin overview / WP3 polling surface).
func (s *Server) listBudgetsAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	budgets, err := s.store.GetPlatformBudgets(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeStorageError(w, err)
		return
	}
	mtdStart := mtdStartUTC(time.Now().UTC())
	views := make([]userBudgetStatusView, 0, len(users))
	for _, u := range users {
		view, err := s.buildUserBudgetStatus(r, u, mtdStart)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		views = append(views, view)
	}
	platformMTD, err := s.platformMTDCost(r, mtdStart)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"platform": platformBudgetView{
			DefaultMonthlyUSD:       budgets.DefaultMonthlyUSD,
			MaxUSD:                  budgets.MaxUSD,
			PlatformMonthlyLimitUSD: budgets.PlatformMonthlyLimitUSD,
			PlatformMTDCost:         platformMTD,
		},
		"users": views,
	})
}

// ---------------------------------------------------------------------------
// Cost aggregation (any authenticated user, scoped)
// ---------------------------------------------------------------------------

// CostSummaryPayload powers the WP2 Cost Management UI. running_cost is
// the current-calendar-month running total (the number compared against
// the monthly budget); total_cost is all-time; the daily series cover
// the requested window grouped by squad, agent, model and provider.
type CostSummaryPayload struct {
	Scope       string        `json:"scope"`
	Currency    string        `json:"currency,omitempty"`
	Days        []string      `json:"days"`
	MTDStart    string        `json:"mtd_start"`
	TotalCost   float64       `json:"total_cost"`
	MTDCost     float64       `json:"mtd_cost"`
	RunningCost float64       `json:"running_cost"`
	BySquad     []UsageSeries `json:"by_squad"`
	ByAgent     []UsageSeries `json:"by_agent"`
	ByModel     []UsageSeries `json:"by_model"`
	ByProvider  []UsageSeries `json:"by_provider"`
	// Budget is the caller's own budget status (null when the caller has
	// no budget set). Admin-only per-user detail lives under
	// /admin/budgets.
	Budget *userBudgetStatusView `json:"budget,omitempty"`
}

func (s *Server) getCostSummary(w http.ResponseWriter, r *http.Request) {
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
	mtdStart := mtdStartUTC(now)
	mtdDay := mtdStart.Format(usageDayLayout)
	windowStart := axis[0]

	squads, err := s.dashboardSquads(r, u.ID, isAdmin)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	squadIDs := make([]string, 0, len(squads))
	for _, sq := range squads {
		squadIDs = append(squadIDs, sq.ID)
	}

	payload := CostSummaryPayload{
		Scope:      "personal",
		Currency:   "USD",
		Days:       axis,
		MTDStart:   mtdDay,
		BySquad:    []UsageSeries{},
		ByAgent:    []UsageSeries{},
		ByModel:    []UsageSeries{},
		ByProvider: []UsageSeries{},
	}
	if isAdmin {
		payload.Scope = "all"
	}

	// Empty allowlist for a non-admin means "nothing visible" — never
	// fall through to the unfiltered query (mirrors getDashboardUsage).
	if len(squadIDs) > 0 || isAdmin {
		var filter []string
		if !isAdmin {
			filter = squadIDs
		}
		// All-time rows: total_cost needs the full history; the window
		// and MTD cuts are taken client-side from the same aggregate.
		rows, err := s.store.SumMeteringDaily(r.Context(), time.Time{}, filter)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		buildCostSummary(&payload, rows, windowStart, mtdDay)
	}
	payload.RunningCost = payload.MTDCost

	budgetView, err := s.buildUserBudgetStatus(r, u, mtdStart)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if budgetView.MonthlyBudgetUSD != nil {
		payload.Budget = &budgetView
	}

	writeJSON(w, http.StatusOK, payload)
}

// buildCostSummary folds daily metering rows into the grouped daily
// series and the total/MTD scalars.
func buildCostSummary(payload *CostSummaryPayload, rows []domain.MeteringDailyRow, windowStart, mtdDay string) {
	squadAcc := map[string]*usageSeriesAcc{}
	agentAcc := map[string]*usageSeriesAcc{}
	modelAcc := map[string]*usageSeriesAcc{}
	provAcc := map[string]*usageSeriesAcc{}

	for _, row := range rows {
		if row.Currency != "" {
			payload.Currency = row.Currency
		}
		if row.Day >= mtdDay {
			payload.MTDCost += row.Cost
		}
		payload.TotalCost += row.Cost
		if row.Day < windowStart {
			continue
		}
		accumulateCostSeries(squadAcc, row.SquadID, row.SquadName, "", "", row)
		accumulateCostSeries(agentAcc, row.AgentID, row.AgentName, row.SquadID, row.SquadName, row)
		model := row.Model
		if model == "" {
			model = "unknown model"
		}
		accumulateCostSeries(modelAcc, model, model, "", "", row)
		provName := row.ProviderName
		if provName == "" {
			provName = "unknown provider"
		}
		accumulateCostSeries(provAcc, row.ProviderID, provName, "", "", row)
	}

	payload.BySquad = finalizeUsageSeries(payload.Days, squadAcc)
	payload.ByAgent = finalizeUsageSeries(payload.Days, agentAcc)
	payload.ByModel = finalizeUsageSeries(payload.Days, modelAcc)
	payload.ByProvider = finalizeUsageSeries(payload.Days, provAcc)
}

// accumulateCostSeries adds one row to a keyed series accumulator,
// creating the series (with optional squad labels) on first sight.
func accumulateCostSeries(acc map[string]*usageSeriesAcc, id, name, squadID, squadName string, row domain.MeteringDailyRow) {
	a := acc[id]
	if a == nil {
		a = &usageSeriesAcc{
			meta:  UsageSeries{ID: id, Name: name, SquadID: squadID, SquadName: squadName},
			byDay: map[string]*UsagePoint{},
		}
		acc[id] = a
	}
	point, ok := a.byDay[row.Day]
	if !ok {
		point = &UsagePoint{Day: row.Day}
		a.byDay[row.Day] = point
	}
	point.InputTokens += row.InputTokens
	point.OutputTokens += row.OutputTokens
	point.Tokens += row.InputTokens + row.OutputTokens
	point.Cost += row.Cost
}
