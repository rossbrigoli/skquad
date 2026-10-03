package storage

// S-203 WP1: budget persistence.
//
// Per-user monthly budgets (user_budgets, migration 0036) plus the
// platform-level knobs that live in platform_settings. The clamp-on-lower
// rule (epic S-203 req 7) is a single UPDATE so lowering
// budget_max_usd can never leave a budget above the max visible between
// two writes.

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// BudgetStore persists per-user monthly budgets and the platform budget
// configuration. Platform-level values are read/written through the
// platform_settings store; the helpers here parse/serialize them so the
// HTTP layer never touches raw strings.
type BudgetStore interface {
	// GetPlatformBudgets reads the three platform budget knobs
	// (default / max / platform-wide limit) from platform_settings.
	GetPlatformBudgets(ctx context.Context) (domain.PlatformBudgets, error)
	// SetPlatformBudgetSetting writes one platform budget setting by
	// key; a nil value clears (deletes) it.
	SetPlatformBudgetSetting(ctx context.Context, key string, value *float64, updatedBy string) error
	// GetUserBudget returns the user's monthly budget. ErrNotFound when
	// the user has no budget row (budget unset).
	GetUserBudget(ctx context.Context, userID string) (*domain.UserBudget, error)
	// SetUserBudget upserts the user's monthly budget. Range/max
	// validation happens at the API boundary.
	SetUserBudget(ctx context.Context, userID string, amount float64, updatedBy string) error
	// ClearUserBudget removes the user's budget row (true when a row
	// existed).
	ClearUserBudget(ctx context.Context, userID string) (bool, error)
	// ListUserBudgets returns every user budget (admin overview).
	ListUserBudgets(ctx context.Context) ([]domain.UserBudget, error)
	// EnsureUserDefaultBudget inserts the platform default budget for a
	// user that has no budget row yet (first login / user creation).
	// Returns true when a row was inserted. No platform default set ⇒
	// (false, nil), never an error on "nothing to do".
	EnsureUserDefaultBudget(ctx context.Context, userID string) (bool, error)
	// ClampUserBudgetsToMax lowers every user budget strictly above max
	// down to max and returns how many rows were clamped (epic req 7).
	ClampUserBudgetsToMax(ctx context.Context, max float64, updatedBy string) (int, error)
}

// parseBudgetUSD parses a platform_settings budget value. ok=false when
// the key is absent or empty (unset). A corrupt value returns an error so
// the caller can surface it rather than silently treating it as unset.
func parseBudgetUSD(raw string, found bool) (*float64, error) {
	if !found || raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, errors.New("corrupt budget setting value: " + raw)
	}
	return &v, nil
}

// formatBudgetUSD serializes a budget value for platform_settings.
func formatBudgetUSD(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// GetPlatformBudgets reads the three platform budget settings.
func (p *PostgresStore) GetPlatformBudgets(ctx context.Context) (domain.PlatformBudgets, error) {
	out := domain.PlatformBudgets{}
	var err error
	if out.DefaultMonthlyUSD, err = p.getBudgetSetting(ctx, domain.PlatformSettingBudgetDefaultUSD); err != nil {
		return out, err
	}
	if out.MaxUSD, err = p.getBudgetSetting(ctx, domain.PlatformSettingBudgetMaxUSD); err != nil {
		return out, err
	}
	if out.PlatformMonthlyLimitUSD, err = p.getBudgetSetting(ctx, domain.PlatformSettingBudgetPlatformMonthlyLimitUSD); err != nil {
		return out, err
	}
	return out, nil
}

func (p *PostgresStore) getBudgetSetting(ctx context.Context, key string) (*float64, error) {
	raw, found, err := p.GetPlatformSetting(ctx, key)
	if err != nil {
		return nil, err
	}
	return parseBudgetUSD(raw, found)
}

// SetPlatformBudgetSetting writes one platform budget setting; a nil
// value clears it.
func (p *PostgresStore) SetPlatformBudgetSetting(ctx context.Context, key string, value *float64, updatedBy string) error {
	if value == nil {
		_, err := p.pool.Exec(ctx, `DELETE FROM platform_settings WHERE key = $1`, key)
		return mapPgErr(err)
	}
	return p.SetPlatformSetting(ctx, key, formatBudgetUSD(*value), updatedBy)
}

func (p *PostgresStore) GetUserBudget(ctx context.Context, userID string) (*domain.UserBudget, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT user_id::text, monthly_budget_usd::double precision, updated_at, updated_by
		FROM user_budgets
		WHERE user_id = $1::uuid
	`, userID)
	var b domain.UserBudget
	if err := row.Scan(&b.UserID, &b.MonthlyBudgetUSD, &b.UpdatedAt, &b.UpdatedBy); err != nil {
		return nil, mapPgErr(err)
	}
	return &b, nil
}

func (p *PostgresStore) SetUserBudget(ctx context.Context, userID string, amount float64, updatedBy string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO user_budgets (user_id, monthly_budget_usd, updated_at, updated_by)
		VALUES ($1::uuid, $2, now(), $3)
		ON CONFLICT (user_id)
		DO UPDATE SET monthly_budget_usd = EXCLUDED.monthly_budget_usd,
		            updated_at = now(),
		            updated_by = EXCLUDED.updated_by
	`, userID, amount, updatedBy)
	return mapPgErr(err)
}

func (p *PostgresStore) ClearUserBudget(ctx context.Context, userID string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM user_budgets WHERE user_id = $1::uuid`, userID)
	if err != nil {
		return false, mapPgErr(err)
	}
	return tag.RowsAffected() > 0, nil
}

func (p *PostgresStore) ListUserBudgets(ctx context.Context) ([]domain.UserBudget, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT user_id::text, monthly_budget_usd::double precision, updated_at, updated_by
		FROM user_budgets
		ORDER BY user_id
	`)
	if err != nil {
		return nil, mapPgErr(err)
	}
	defer rows.Close()
	out := []domain.UserBudget{}
	for rows.Next() {
		var b domain.UserBudget
		if err := rows.Scan(&b.UserID, &b.MonthlyBudgetUSD, &b.UpdatedAt, &b.UpdatedBy); err != nil {
			return nil, mapPgErr(err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// EnsureUserDefaultBudget inserts the platform default only when the
// user has no budget row; DO NOTHING keeps it idempotent for the
// every-login call.
func (p *PostgresStore) EnsureUserDefaultBudget(ctx context.Context, userID string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO user_budgets (user_id, monthly_budget_usd, updated_by)
		SELECT $1::uuid, NULLIF(ps.value, '')::numeric, 'system:default'
		FROM platform_settings ps
		WHERE ps.key = $2 AND NULLIF(ps.value, '') IS NOT NULL
		ON CONFLICT (user_id) DO NOTHING
	`, userID, domain.PlatformSettingBudgetDefaultUSD)
	if err != nil {
		return false, mapPgErr(err)
	}
	return tag.RowsAffected() > 0, nil
}

func (p *PostgresStore) ClampUserBudgetsToMax(ctx context.Context, max float64, updatedBy string) (int, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE user_budgets
		SET monthly_budget_usd = $1, updated_at = now(), updated_by = $2
		WHERE monthly_budget_usd > $1
	`, max, updatedBy)
	if err != nil {
		return 0, mapPgErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------
// MemoryStore
// ---------------------------------------------------------------------------

func (m *MemoryStore) GetPlatformBudgets(_ context.Context) (domain.PlatformBudgets, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := domain.PlatformBudgets{}
	var err error
	if out.DefaultMonthlyUSD, err = budgetSetting(m, domain.PlatformSettingBudgetDefaultUSD); err != nil {
		return out, err
	}
	if out.MaxUSD, err = budgetSetting(m, domain.PlatformSettingBudgetMaxUSD); err != nil {
		return out, err
	}
	if out.PlatformMonthlyLimitUSD, err = budgetSetting(m, domain.PlatformSettingBudgetPlatformMonthlyLimitUSD); err != nil {
		return out, err
	}
	return out, nil
}

func budgetSetting(m *MemoryStore, key string) (*float64, error) {
	raw, found := m.platformSettings[key]
	return parseBudgetUSD(raw, found)
}

func (m *MemoryStore) SetPlatformBudgetSetting(ctx context.Context, key string, value *float64, updatedBy string) error {
	if value == nil {
		m.mu.Lock()
		delete(m.platformSettings, key)
		m.mu.Unlock()
		return nil
	}
	return m.SetPlatformSetting(ctx, key, formatBudgetUSD(*value), updatedBy)
}

func (m *MemoryStore) GetUserBudget(_ context.Context, userID string) (*domain.UserBudget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.userBudgets[userID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (m *MemoryStore) SetUserBudget(_ context.Context, userID string, amount float64, updatedBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.userBudgets[userID]
	if !ok {
		b = &domain.UserBudget{UserID: userID}
		m.userBudgets[userID] = b
	}
	b.MonthlyBudgetUSD = amount
	b.UpdatedAt = time.Now().UTC()
	b.UpdatedBy = updatedBy
	return nil
}

func (m *MemoryStore) ClearUserBudget(_ context.Context, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.userBudgets[userID]
	delete(m.userBudgets, userID)
	return ok, nil
}

func (m *MemoryStore) ListUserBudgets(_ context.Context) ([]domain.UserBudget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.UserBudget, 0, len(m.userBudgets))
	for _, b := range m.userBudgets {
		out = append(out, *b)
	}
	return out, nil
}

func (m *MemoryStore) EnsureUserDefaultBudget(_ context.Context, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.userBudgets[userID]; ok {
		return false, nil
	}
	raw, found := m.platformSettings[domain.PlatformSettingBudgetDefaultUSD]
	def, err := parseBudgetUSD(raw, found)
	if err != nil || def == nil {
		return false, err
	}
	m.userBudgets[userID] = &domain.UserBudget{
		UserID:           userID,
		MonthlyBudgetUSD: *def,
		UpdatedAt:        time.Now().UTC(),
		UpdatedBy:        "system:default",
	}
	return true, nil
}

func (m *MemoryStore) ClampUserBudgetsToMax(_ context.Context, max float64, updatedBy string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clamped := 0
	for _, b := range m.userBudgets {
		if b.MonthlyBudgetUSD > max {
			b.MonthlyBudgetUSD = max
			b.UpdatedAt = time.Now().UTC()
			b.UpdatedBy = updatedBy
			clamped++
		}
	}
	return clamped, nil
}
