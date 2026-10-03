package domain

import "time"

// S-203 WP3: Budget enforcement state.
//
// Enforcement is persisted (rather than computed purely on the fly) so
// the scheduling decision is a cheap, explicit check: an agent may only
// be started/woken when its squad owner is not budget-blocked.
//
// A BudgetBlock row is scoped to a calendar month `period` ("2006-01",
// UTC). A row whose period is NOT the current period is stale: budgets
// reset at month boundaries, so a stale row never blocks. Writers always
// write the current period, which also resets flags carried over from a
// previous month.
//
// Two independent sources can block a user:
//   - blocked_by_user:     the user's own effective budget is exhausted.
//   - blocked_by_platform: the platform-wide monthly limit is exhausted.
//
// The user is blocked while EITHER flag is set for the current period.
// Clearing one source leaves the other intact, so a user blocked by the
// platform stays blocked even after raising their personal budget.
type BudgetBlock struct {
	UserID            string    `json:"user_id"`
	Period            string    `json:"period"`
	BlockedByUser     bool      `json:"blocked_by_user"`
	BlockedByPlatform bool      `json:"blocked_by_platform"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// EffectiveBlocked reports whether this row blocks the user in the given
// period. A nil row (never blocked) or a row from another period is not
// in effect.
func (b *BudgetBlock) EffectiveBlocked(period string) bool {
	return b != nil && b.Period == period && (b.BlockedByUser || b.BlockedByPlatform)
}

// Budget enforcement sources. The source string is used both as the
// block-flag selector and the notification marker prefix.
const (
	BudgetSourceUser     = "user"
	BudgetSourcePlatform = "platform"
)

// Budget notification markers. One marker per source per threshold per
// calendar month; claiming a marker is atomic, so exactly one
// notification fires per threshold per user per month.
const (
	BudgetNotifyUser80      = "user:80"
	BudgetNotifyUser90      = "user:90"
	BudgetNotifyUser100     = "user:100"
	BudgetNotifyPlatform80  = "platform:80"
	BudgetNotifyPlatform90  = "platform:90"
	BudgetNotifyPlatform100 = "platform:100"
)

// BudgetNotifyMarker builds the marker for a source ("user"/"platform")
// and threshold percentage (80/90/100).
func BudgetNotifyMarker(source string, threshold int) string {
	return source + ":" + itoa(threshold)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
