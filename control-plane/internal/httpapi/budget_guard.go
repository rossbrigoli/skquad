package httpapi

// S-203 WP3: budget enforcement — stop agents at limit + notifications.
//
// Evaluation points:
//   - after every metered LLM call (ingestGatewayMetering) the squad
//     owner's month-to-date spend is compared against their effective
//     budget (own budget row, else the platform default, else
//     unlimited), and the platform-wide MTD against the platform limit.
//   - after any budget mutation (admin PUT endpoints) the affected
//     users are re-evaluated so raising/resetting a budget resumes
//     scheduling without further manual steps.
//
// Enforcement state is persisted per user per calendar month
// (budget_blocks) so the scheduling check is a cheap explicit lookup,
// and notifications are claimed atomically (budget_notify_markers) so
// each threshold fires exactly once per user per month even under
// concurrent turns.
//
// Stop semantics: a block never kills a running pod mid-turn. The block
// prevents every *start* path (wake, message/task-driven busy
// transitions, heartbeat idle→busy upgrade). When the in-flight turn
// ends the agent goes idle, the CR mirror writes desiredActive=false
// with a zeroed idle timeout for blocked owners (see the outbox
// worker), and the operator tears the pod down immediately.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// budgetThresholds are the warning levels that trigger inbox
// notifications; 100 additionally stops the user's agents.
var budgetThresholds = []int{80, 90}

// currentBudgetPeriod returns the calendar-month period key ("2006-01",
// UTC) used for enforcement state and notification markers.
func currentBudgetPeriod(now time.Time) string { return now.UTC().Format("2006-01") }

// effectiveUserBudget resolves the budget a user's spend is measured
// against: their own budget row when present, otherwise the platform
// default. nil means unlimited (no budget row and no platform default).
func (s *Server) effectiveUserBudget(ctx context.Context, userID string, platform domain.PlatformBudgets) (*float64, error) {
	budget, err := s.store.GetUserBudget(ctx, userID)
	if err == nil && budget != nil {
		return &budget.MonthlyBudgetUSD, nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	return platform.DefaultMonthlyUSD, nil
}

// enforceBudgetsAfterMetering runs after a metering event is recorded.
// It evaluates the squad owner's budget and the platform-wide limit.
// Best-effort by design: enforcement failures are logged and must never
// fail the metering ingest itself.
func (s *Server) enforceBudgetsAfterMetering(ctx context.Context, squadID string) {
	squad, err := s.store.GetSquad(ctx, squadID)
	if err != nil {
		log.Printf("budget-enforce: squad %s lookup failed, skipping evaluation: %v", squadID, err)
		return
	}
	platform, err := s.store.GetPlatformBudgets(ctx)
	if err != nil {
		log.Printf("budget-enforce: platform budgets unreadable, skipping evaluation: %v", err)
		return
	}
	now := time.Now().UTC()
	if squad.OwnerID != "" {
		if err := s.evaluateUserBudget(ctx, squad.OwnerID, now, platform); err != nil {
			log.Printf("budget-enforce: user %s evaluation failed: %v", squad.OwnerID, err)
		}
	}
	if err := s.evaluatePlatformBudget(ctx, now, platform); err != nil {
		log.Printf("budget-enforce: platform evaluation failed: %v", err)
	}
}

// evaluateUserBudget compares one user's MTD spend against their
// effective budget and applies the verdict. No effective budget ⇒
// unlimited: never blocked, no notifications.
func (s *Server) evaluateUserBudget(ctx context.Context, userID string, now time.Time, platform domain.PlatformBudgets) error {
	budget, err := s.effectiveUserBudget(ctx, userID, platform)
	if err != nil {
		return err
	}
	if budget == nil {
		return nil
	}
	mtd, err := s.userMTDCostCtx(ctx, userID, mtdStartUTC(now))
	if err != nil {
		return err
	}
	return s.applyBudgetVerdict(ctx, domain.BudgetSourceUser, userID, now, *budget, mtd)
}

// evaluatePlatformBudget compares the platform-wide MTD spend against
// the platform monthly limit and applies the verdict to every squad
// owner (the limit is enforced the same way across all users). A
// cleared/absent limit resumes everyone previously blocked by it.
func (s *Server) evaluatePlatformBudget(ctx context.Context, now time.Time, platform domain.PlatformBudgets) error {
	if platform.PlatformMonthlyLimitUSD == nil {
		resumed, err := s.store.ClearBudgetBlocksBySource(ctx, domain.BudgetSourcePlatform)
		if err != nil {
			return err
		}
		for _, userID := range resumed {
			s.remirrorUserAgents(ctx, userID)
		}
		return nil
	}
	mtd, err := s.platformMTDCostCtx(ctx, mtdStartUTC(now))
	if err != nil {
		return err
	}
	for _, ownerID := range s.squadOwnerIDs(ctx) {
		if err := s.applyBudgetVerdict(ctx, domain.BudgetSourcePlatform, ownerID, now, *platform.PlatformMonthlyLimitUSD, mtd); err != nil {
			log.Printf("budget-enforce: platform verdict for user %s failed: %v", ownerID, err)
		}
	}
	return nil
}

// squadOwnerIDs returns the distinct owners of all squads — the users
// affected by platform-level enforcement.
func (s *Server) squadOwnerIDs(ctx context.Context) []string {
	squads, err := s.store.ListSquads(ctx, "")
	if err != nil {
		log.Printf("budget-enforce: list squads failed: %v", err)
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, squad := range squads {
		if squad.OwnerID != "" && !seen[squad.OwnerID] {
			seen[squad.OwnerID] = true
			out = append(out, squad.OwnerID)
		}
	}
	return out
}

// applyBudgetVerdict evaluates one spend-vs-budget pair for one subject
// (source: the user's own budget or the platform-wide limit) and
// applies warnings, the block flag, and the resume path.
//
// Blocked semantics: spend strictly greater than zero AND at or above
// the budget. A zero budget therefore blocks on the first recorded
// cost (a zero allowance means any spend exceeds it) but never blocks a
// user who has not spent anything.
func (s *Server) applyBudgetVerdict(ctx context.Context, source, userID string, now time.Time, budget, spend float64) error {
	period := currentBudgetPeriod(now)
	if budget > 0 {
		pct := spend / budget * 100
		for _, threshold := range budgetThresholds {
			if pct >= float64(threshold) {
				if err := s.notifyBudgetThreshold(ctx, source, userID, period, threshold, spend, budget, false); err != nil {
					log.Printf("budget-enforce: %s %d%% notification failed for %s: %v", source, threshold, userID, err)
				}
			}
		}
	}
	blockedNow := spend > 0 && spend >= budget

	wasBlocked := false
	existing, err := s.store.GetBudgetBlock(ctx, userID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err == nil {
		wasBlocked = existing.EffectiveBlocked(period)
	}
	if _, err := s.store.SetBudgetBlock(ctx, userID, period, source, blockedNow); err != nil {
		return err
	}
	if blockedNow {
		if err := s.notifyBudgetThreshold(ctx, source, userID, period, 100, spend, budget, true); err != nil {
			log.Printf("budget-enforce: %s 100%% notification failed for %s: %v", source, userID, err)
		}
		// Newly blocked: push fresh CRs so idle-warm pods (desiredActive
		// already false) scale to zero immediately under the blocked
		// idle-timeout override. Busy agents keep desiredActive=true and
		// are torn down at end of turn, never mid-turn.
		if !wasBlocked {
			s.remirrorUserAgents(ctx, userID)
		}
		return nil
	}
	// Resume path: this source no longer blocks. If the combined state
	// flipped blocked→clear, re-mirror the user's agents so the CRs
	// lose the blocked idle-timeout override and scheduling resumes.
	if wasBlocked {
		after, err := s.store.GetBudgetBlock(ctx, userID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if !after.EffectiveBlocked(period) {
			s.remirrorUserAgents(ctx, userID)
		}
	}
	return nil
}

// notifyBudgetThreshold sends the inbox notification for one threshold
// if (and only if) this call wins the atomic marker claim for the
// period. Losing the claim is the normal case for repeat evaluations
// and is not an error.
func (s *Server) notifyBudgetThreshold(ctx context.Context, source, userID, period string, threshold int, spend, budget float64, stopped bool) error {
	marker := domain.BudgetNotifyMarker(source, threshold)
	claimed, err := s.store.ClaimBudgetNotification(ctx, userID, period, marker)
	if err != nil || !claimed {
		return err
	}
	kind := domain.InboxBudgetWarning
	if stopped {
		kind = domain.InboxBudgetStopped
	}
	subject, body := budgetNotificationText(source, threshold, spend, budget, stopped)
	_, err = s.store.CreateInboxMessage(ctx, &domain.InboxMessage{
		UserID:  userID,
		Kind:    kind,
		Message: subject,
		Subject: subject,
		Body:    body,
	})
	return err
}

// budgetNotificationText renders the subject/body for a budget
// notification. Amounts are USD; percentages are of the effective
// budget (or the platform limit for platform-sourced messages).
func budgetNotificationText(source string, threshold int, spend, budget float64, stopped bool) (subject, body string) {
	scope := "monthly budget"
	if source == domain.BudgetSourcePlatform {
		scope = "platform-wide monthly spend limit"
	}
	if !stopped {
		subject = fmt.Sprintf("Budget warning: %d%% of your %s used", threshold, scope)
		body = fmt.Sprintf(
			"You have used %d%% of your %s: $%.2f spent of $%.2f.\n\n"+
				"When the limit is reached your agents will stop at the end of their current turn and will not be scheduled again until the limit is raised.\n\n"+
				"Review your spend in the Cost Management screen.",
			threshold, scope, spend, budget,
		)
		return subject, body
	}
	if source == domain.BudgetSourcePlatform {
		subject = "Platform monthly limit reached — your agents are stopped"
		body = fmt.Sprintf(
			"The platform-wide monthly spend limit of $%.2f has been reached (platform spend: $%.2f).\n\n"+
				"Your agents were stopped at the end of their current turn and will not be scheduled while the platform limit is exhausted.\n\n"+
				"Your platform administrator can raise or reset the platform limit; scheduling resumes automatically.",
			budget, spend,
		)
		return subject, body
	}
	subject = "Monthly budget reached — your agents are stopped"
	body = fmt.Sprintf(
		"You have used your full monthly budget: $%.2f spent of $%.2f.\n\n"+
			"Your agents were stopped at the end of their current turn and will not be scheduled again.\n\n"+
			"Raising or resetting your budget resumes scheduling automatically — no other action is needed.",
		spend, budget,
	)
	return subject, body
}

// remirrorUserAgents re-enqueues the CR upsert for every agent owned
// (via squads) by the user so the mirror picks up the current block
// state — restoring the normal idle timeout after a resume.
func (s *Server) remirrorUserAgents(ctx context.Context, userID string) {
	squads, err := s.store.ListSquads(ctx, userID)
	if err != nil {
		log.Printf("budget-enforce: remirror list squads failed for %s: %v", userID, err)
		return
	}
	for _, squad := range squads {
		agents, err := s.store.ListAgents(ctx, squad.ID)
		if err != nil {
			log.Printf("budget-enforce: remirror list agents failed for squad %s: %v", squad.ID, err)
			continue
		}
		for _, agent := range agents {
			if err := s.store.SetAgentStatus(ctx, agent.ID, agent.Status); err != nil {
				log.Printf("budget-enforce: remirror agent %s failed: %v", agent.ID, err)
			}
		}
	}
}

// sweepBudgets re-evaluates the platform limit and every user. Called
// after platform budget changes (which can clamp many budgets at once)
// and usable as a general reconciliation hook.
func (s *Server) sweepBudgets(ctx context.Context) {
	now := time.Now().UTC()
	platform, err := s.store.GetPlatformBudgets(ctx)
	if err != nil {
		log.Printf("budget-enforce: sweep platform budgets failed: %v", err)
		return
	}
	if err := s.evaluatePlatformBudget(ctx, now, platform); err != nil {
		log.Printf("budget-enforce: sweep platform evaluation failed: %v", err)
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		log.Printf("budget-enforce: sweep list users failed: %v", err)
		return
	}
	for _, u := range users {
		if err := s.evaluateUserBudget(ctx, u.ID, now, platform); err != nil {
			log.Printf("budget-enforce: sweep user %s failed: %v", u.ID, err)
		}
	}
}

// agentBudgetBlocked is the cheap scheduling check shared by the start
// paths. A storage failure fails OPEN for reads of unknown state only at
// the notification layer; here it must not silently start a pod for a
// possibly-blocked owner, so callers treat err as "not blocked" but
// log — matching the best-effort posture of the rest of enforcement.
func (s *Server) agentBudgetBlocked(ctx context.Context, agentID string) bool {
	blocked, err := s.store.IsAgentBudgetBlocked(ctx, agentID, currentBudgetPeriod(time.Now()))
	if err != nil {
		log.Printf("budget-enforce: block check failed for agent %s: %v", agentID, err)
		return false
	}
	return blocked
}

// budgetStopNotice is appended to API responses that refuse to start an
// agent because of budget enforcement.
const budgetStopNotice = "budget_blocked"
