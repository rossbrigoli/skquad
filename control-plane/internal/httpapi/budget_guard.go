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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"
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
	s.notifyBudgetWarnings(ctx, source, userID, period, spend, budget)
	blockedNow := spend > 0 && spend >= budget

	wasBlocked, err := s.budgetBlockedFor(ctx, userID, period)
	if err != nil {
		return err
	}
	if _, err := s.store.SetBudgetBlock(ctx, userID, period, source, blockedNow); err != nil {
		return err
	}
	if blockedNow {
		s.notifyBudgetStopped(ctx, source, userID, period, spend, budget)
		// Newly blocked: push fresh CRs so idle-warm pods (desiredActive
		// already false) scale to zero immediately under the blocked
		// idle-timeout override. Busy agents keep desiredActive=true and
		// are torn down at end of turn, never mid-turn.
		if !wasBlocked {
			// S-320 / WI-5: expire pending chat rows addressed to this
			// user's agents before they can resurrect later, then remirror.
			s.expirePendingChatForBlockedUser(ctx, userID)
			s.remirrorUserAgents(ctx, userID)
		}
		return nil
	}
	// Resume path: this source no longer blocks. If the combined state
	// flipped blocked→clear, re-mirror the user's agents so the CRs
	// lose the blocked idle-timeout override and scheduling resumes.
	if wasBlocked {
		stillBlocked, err := s.budgetBlockedFor(ctx, userID, period)
		if err != nil {
			return err
		}
		if !stillBlocked {
			s.remirrorUserAgents(ctx, userID)
		}
	}
	return nil
}

// notifyBudgetWarnings fires the incremental threshold notifications
// (80/90/… as configured) for one spend-vs-budget pair. Best-effort:
// notification failures are logged, never propagated.
func (s *Server) notifyBudgetWarnings(ctx context.Context, source, userID, period string, spend, budget float64) {
	if budget <= 0 {
		return
	}
	pct := spend / budget * 100
	for _, threshold := range budgetThresholds {
		if pct < float64(threshold) {
			continue
		}
		if err := s.notifyBudgetThreshold(ctx, budgetNotifyParams{
			Source: source, UserID: userID, Period: period,
			Threshold: threshold, Spend: spend, Budget: budget,
		}); err != nil {
			log.Printf("budget-enforce: %s %d%% notification failed for %s: %v", source, threshold, userID, err)
		}
	}
}

// notifyBudgetStopped fires the terminal 100%/stopped notification.
// Best-effort like the warning path.
func (s *Server) notifyBudgetStopped(ctx context.Context, source, userID, period string, spend, budget float64) {
	if err := s.notifyBudgetThreshold(ctx, budgetNotifyParams{
		Source: source, UserID: userID, Period: period,
		Threshold: 100, Spend: spend, Budget: budget, Stopped: true,
	}); err != nil {
		log.Printf("budget-enforce: %s 100%% notification failed for %s: %v", source, userID, err)
	}
}

// budgetBlockedFor reads the effective blocked state for one user in one
// period. A missing block row means "not blocked".
func (s *Server) budgetBlockedFor(ctx context.Context, userID, period string) (bool, error) {
	existing, err := s.store.GetBudgetBlock(ctx, userID)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return existing.EffectiveBlocked(period), nil
}

// notifyBudgetThreshold sends the inbox notification for one threshold
// if (and only if) this call wins the atomic marker claim for the
// period. Losing the claim is the normal case for repeat evaluations
// and is not an error. S-203 WP4: the same claim also drives the bell
// notification (budget_warning / budget_stopped NotificationType), so
// the inbox row and the bell alert share one exactly-once-per-threshold
// -per-month guarantee. The bell emit is best-effort: a failure is
// logged by the caller and never rolls back the inbox write.
// budgetNotifyParams carries one threshold-notification request (S-189:
// params struct replaces the 8-arg signature).
type budgetNotifyParams struct {
	Source    string
	UserID    string
	Period    string
	Threshold int
	Spend     float64
	Budget    float64
	Stopped   bool
}

func (s *Server) notifyBudgetThreshold(ctx context.Context, p budgetNotifyParams) error {
	source, userID, period, threshold := p.Source, p.UserID, p.Period, p.Threshold
	spend, budget, stopped := p.Spend, p.Budget, p.Stopped
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
	if _, err := s.store.CreateInboxMessage(ctx, &domain.InboxMessage{
		UserID:  userID,
		Kind:    kind,
		Message: subject,
		Subject: subject,
		Body:    body,
	}); err != nil {
		return err
	}
	return s.emitBudgetNotification(ctx, userID, kind, subject)
}

// emitBudgetNotification files the user-level bell alert (S-203 WP4).
// User-scoped: no squad/task context, so the web layer links it to
// /costs. Honors the S-199 mute preferences exactly like the other
// notification channels.
func (s *Server) emitBudgetNotification(ctx context.Context, userID string, kind domain.InboxKind, message string) error {
	notifType := domain.NotificationBudgetWarning
	severity := domain.NotificationWarning
	if kind == domain.InboxBudgetStopped {
		notifType = domain.NotificationBudgetStopped
		severity = domain.NotificationError
	}
	if notificationMuted(ctx, s.store, userID, notifType) {
		slog.Info("budget notification skipped: type muted by user preference",
			"type", string(notifType), "user", userID)
		return nil
	}
	_, err := s.store.CreateNotification(ctx, &domain.Notification{
		UserID:   userID,
		Type:     notifType,
		Severity: severity,
		Message:  trimRunes(strings.TrimSpace(message), maxInboxMessageChars),
	})
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		slog.Warn("emit budget notification", "error", err, "type", string(notifType), "user", userID)
	}
	return nil
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

// budgetVerdict is the three-state result of the scheduling budget
// check (S-319 / S-203 WP3-R1, plan WI-1). A check failure is typed
// distinctly from a confirmed block: "could not verify" is not "exhausted"
// — different user message, different metric, different operator urgency.
type budgetVerdict int

const (
	// BudgetAllowed: owner is verifiably not blocked.
	BudgetAllowed budgetVerdict = iota
	// BudgetBlocked: an explicit block row exists for the current period.
	BudgetBlocked
	// BudgetUnverifiable: the store check errored. Start paths treat
	// this like blocked (fail closed) — this is a money control and must
	// not fail open.
	BudgetUnverifiable
)

// checkAgentBudget is the cheap scheduling check shared by the start
// paths. S-319: a storage failure no longer fails open. It returns
// BudgetUnverifiable so callers refuse *new* starts (idle→busy, wake,
// pre-warm) while the already-busy pass-through in
// setAgentStatusAndMirror keeps its never-interrupt guarantee: an
// admitted turn is never disturbed by a check error.
func (s *Server) checkAgentBudget(ctx context.Context, agentID string) budgetVerdict {
	blocked, err := s.store.IsAgentBudgetBlocked(ctx, agentID, currentBudgetPeriod(time.Now()))
	if err != nil {
		log.Printf("budget-enforce: block check failed for agent %s (fail-closed as unverifiable): %v", agentID, err)
		return BudgetUnverifiable
	}
	if blocked {
		return BudgetBlocked
	}
	return BudgetAllowed
}

// budgetStopNotice is appended to API responses that refuse to start an
// agent because of a confirmed budget block.
const budgetStopNotice = "budget_blocked"

// budgetExpiredChatReason is the terminal_reason stamped on chat rows
// expired by a newly-created budget block (S-320 / WI-5).
const budgetExpiredChatReason = "expired_budget"

// unblock actions surfaced in the 402 rejection body (S-320 / WI-4):
// the concrete thing the recipient must do to resume sends.
const (
	budgetUnblockRaiseBudget   = "raise_budget"
	budgetUnblockContactAdmin  = "contact_platform_admin"
)

// expirePendingChatForBlockedUser (S-320 / S-203 WP3-R2 WI-5) expires
// — never deletes — user→agent chat messages still `pending` to agents
// of every squad owned by the newly-blocked user, so a later budget
// raise cannot resurrect stale input as a surprise turn (plan G3).
// Rows survive with status=expired and terminal_reason=expired_budget,
// and one audit entry per affected squad records the sweep. Task rows
// are untouched: task queues stay visible while blocked (WI-6).
// Best-effort like the rest of the enforcement path: failures are
// logged, never propagated (the block itself already stands).
func (s *Server) expirePendingChatForBlockedUser(ctx context.Context, userID string) {
	squads, err := s.store.ListSquads(ctx, userID)
	if err != nil {
		log.Printf("budget-enforce: pending-chat expiry list squads failed for %s: %v", userID, err)
		return
	}
	for _, squad := range squads {
		expired, err := s.store.ExpirePendingChatMessagesForSquad(ctx, squad.ID, budgetExpiredChatReason)
		if err != nil {
			log.Printf("budget-enforce: pending-chat expiry failed for squad %s: %v", squad.ID, err)
			continue
		}
		if expired == 0 {
			continue
		}
		metadata, _ := json.Marshal(map[string]any{
			"reason":           budgetExpiredChatReason,
			"expired_messages": expired,
		})
		if err := s.store.RecordAudit(ctx, &domain.AuditEntry{
			ActorType:    "system",
			ActorID:      "budget-enforcement",
			Action:       "budget.chat_expired",
			ResourceType: "squad",
			ResourceID:   squad.ID,
			SquadID:      squad.ID,
			Metadata:     metadata,
		}); err != nil {
			log.Printf("budget-enforce: pending-chat expiry audit write failed for squad %s: %v", squad.ID, err)
		}
	}
}

// budgetSendRejection is the typed, actionable body returned when a
// user-facing send is refused before the message row is created
// (S-320 / S-203 WP3-R2 WI-4). Clients branch on `code`
// (budget_blocked vs budget_verification_failed) — never on message
// strings. The flat shape (code + fields + message) is deliberate per
// the remediation plan so the rejection is self-describing.
type budgetSendRejection struct {
	Code          string  `json:"code"`
	Source        string  `json:"source,omitempty"`
	Period        string  `json:"period,omitempty"`
	SpentUSD      float64 `json:"spent_usd"`
	CapUSD        float64 `json:"cap_usd"`
	UnblockAction string  `json:"unblock_action,omitempty"`
	Message       string  `json:"message"`

	status int
}

// checkBudgetForSend evaluates the budget verdict for a user-facing
// message creation (chat send / task-page composer) BEFORE any row is
// written. Returns nil when the send may proceed unchanged; returns a
// typed rejection for BudgetBlocked (402, with source/period/spend/cap
// resolved from the block row + budget/spend lookups) and for
// BudgetUnverifiable (503, retry — clearly distinct from exhaustion).
//
// The chokepoint (setAgentStatusAndMirror) remains the enforcement
// layer; this check exists to produce the response the client deserves
// and to keep un-sendable rows out of the table (plan §3.2).
func (s *Server) checkBudgetForSend(ctx context.Context, agentID string) *budgetSendRejection {
	switch s.checkAgentBudget(ctx, agentID) {
	case BudgetAllowed:
		return nil
	case BudgetUnverifiable:
		return &budgetSendRejection{
			Code:   budgetVerificationFailedNotice,
			status: http.StatusServiceUnavailable,
			Message: "Your budget could not be verified right now, so no new agent work was started. " +
				"This is a temporary system issue, not a budget exhaustion — please retry shortly; " +
				"nothing you sent before was lost.",
		}
	}
	period := currentBudgetPeriod(time.Now())
	if rej, confirmed := s.budgetBlockedSendDetails(ctx, agentID, period); confirmed {
		if rej != nil {
			return rej
		}
		// Block confirmed but the detail lookups failed: still a hard
		// block (fail closed) — reject with a generic 402 rather than
		// fabricate numbers or imply a retry would help.
		return &budgetSendRejection{
			Code:   budgetStopNotice,
			Period: period,
			status: http.StatusPaymentRequired,
			Message: "Your budget is exhausted for " + period + ", so this message was not sent. " +
			"Your previous turn completed — nothing was lost. " +
			"Raise or reset the budget (or contact your platform administrator if the block is platform-wide) to resume.",
		}
	}
	// The block row flipped clear between the verdict read and the
	// detail read (a benign race the plan accepts): admit the send; the
	// chokepoint still guards the actual start.
	return nil
}

// budgetBlockedSendDetails resolves the actionable detail for a
// confirmed block on agentID's owner. confirmed=false means the block
// row is no longer effective for the current period (send may proceed);
// confirmed=true with rej=nil means the block stands but the numbers
// could not be read (generic 402). When BOTH sources block, the
// platform source is reported: it is the one the user cannot resolve
// themselves (T6).
func (s *Server) budgetBlockedSendDetails(ctx context.Context, agentID, period string) (*budgetSendRejection, bool) {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, true
	}
	squad, err := s.store.GetSquad(ctx, agent.SquadID)
	if err != nil {
		return nil, true
	}
	block, err := s.store.GetBudgetBlock(ctx, squad.OwnerID)
	if err != nil || !block.EffectiveBlocked(period) {
		return nil, false
	}
	platform, err := s.store.GetPlatformBudgets(ctx)
	if err != nil {
		return nil, true
	}
	now := time.Now().UTC()
	if block.BlockedByPlatform && platform.PlatformMonthlyLimitUSD != nil {
		spent, err := s.platformMTDCostCtx(ctx, mtdStartUTC(now))
		if err != nil {
			return nil, true
		}
		return &budgetSendRejection{
			Code:          budgetStopNotice,
			Source:        domain.BudgetSourcePlatform,
			Period:        period,
			SpentUSD:      spent,
			CapUSD:        *platform.PlatformMonthlyLimitUSD,
			UnblockAction: budgetUnblockContactAdmin,
			status:        http.StatusPaymentRequired,
			Message: fmt.Sprintf(
				"The platform-wide monthly spend limit of $%.2f is exhausted (platform spend: $%.2f, period %s), so this message was not sent. " +
					"Your previous turn completed — nothing was lost. " +
					"Your platform administrator can raise or reset the platform limit; your agents resume scheduling automatically once it clears.",
				*platform.PlatformMonthlyLimitUSD, spent, period,
			),
		}, true
	}
	if block.BlockedByUser {
		capUSD, err := s.effectiveUserBudget(ctx, squad.OwnerID, platform)
		if err != nil || capUSD == nil {
			return nil, true
		}
		spent, err := s.userMTDCostCtx(ctx, squad.OwnerID, mtdStartUTC(now))
		if err != nil {
			return nil, true
		}
		return &budgetSendRejection{
			Code:          budgetStopNotice,
			Source:        domain.BudgetSourceUser,
			Period:        period,
			SpentUSD:      spent,
			CapUSD:        *capUSD,
			UnblockAction: budgetUnblockRaiseBudget,
			status:        http.StatusPaymentRequired,
			Message: fmt.Sprintf(
				"Your monthly budget is exhausted: $%.2f spent of $%.2f (period %s), so this message was not sent. " +
					"Your previous turn completed — nothing was lost. " +
					"Raising or resetting your budget resumes scheduling automatically — no other action is needed.",
				spent, *capUSD, period,
			),
		}, true
	}
	// Neither source blocks in this period: not actually blocked.
	return nil, false
}

// writeBudgetSendRejection emits the typed rejection with its mapped
// HTTP status (402 exhaustion / 503 verification failure).
func writeBudgetSendRejection(w http.ResponseWriter, rej *budgetSendRejection) {
	status := rej.status
	if status == 0 {
		status = http.StatusPaymentRequired
	}
	writeJSON(w, status, rej)
}

// budgetVerificationFailedNotice is returned when the budget check
// itself failed, so the client can distinguish "blocked" from "could not
// verify, retry" (S-319 / plan §3: typed distinct from exhaustion).
const budgetVerificationFailedNotice = "budget_verification_failed"
