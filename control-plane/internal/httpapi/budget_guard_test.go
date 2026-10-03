package httpapi

// S-203 WP3: budget enforcement tests — threshold evaluation
// (79/80/89/90/99/100%), once-per-month notification markers,
// platform-limit aggregation, resume-on-raise, no-budget = unlimited,
// concurrent evaluation idempotency, and the scheduling guard.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func guardServer(t *testing.T) (*Server, *storage.MemoryStore) {
	t.Helper()
	store := storage.NewMemoryStore()
	return &Server{store: store}, store
}

func mustUser(t *testing.T, store *storage.MemoryStore, email string) string {
	t.Helper()
	u, err := store.UpsertUser(context.Background(), &domain.User{Email: email, Name: email, Role: domain.RoleUser})
	require.NoError(t, err)
	return u.ID
}

// seedSquadAgent creates a squad owned by ownerID with one idle agent.
func seedSquadAgent(t *testing.T, store *storage.MemoryStore, ownerID, tag string) (string, string) {
	t.Helper()
	ctx := context.Background()
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: "squad-" + tag, OwnerID: ownerID})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{Name: "agent-" + tag, SquadID: squad.ID, Role: "worker", Status: domain.AgentIdle})
	require.NoError(t, err)
	return squad.ID, agent.ID
}

// spend records a metering event (cost) against a squad/agent this month.
func spend(t *testing.T, store *storage.MemoryStore, squadID, agentID string, cost float64) {
	t.Helper()
	require.NoError(t, store.RecordMetering(context.Background(), &domain.MeteringEvent{
		AgentID:   agentID,
		SquadID:   squadID,
		Cost:      cost,
		Currency:  "USD",
		Timestamp: time.Now().UTC(),
	}))
}

func inboxByKind(t *testing.T, store *storage.MemoryStore, userID string, kind domain.InboxKind) []*domain.InboxMessage {
	t.Helper()
	msgs, err := store.ListInboxMessages(context.Background(), userID, false, 500)
	require.NoError(t, err)
	out := []*domain.InboxMessage{}
	for _, m := range msgs {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func evalUser(t *testing.T, s *Server, userID string, platform domain.PlatformBudgets) {
	t.Helper()
	require.NoError(t, s.evaluateUserBudget(context.Background(), userID, time.Now().UTC(), platform))
}

func blockedNow(t *testing.T, store *storage.MemoryStore, userID string) bool {
	t.Helper()
	block, err := store.GetBudgetBlock(context.Background(), userID)
	if err != nil {
		require.ErrorIs(t, err, storage.ErrNotFound)
		return false
	}
	return block.EffectiveBlocked(currentBudgetPeriod(time.Now()))
}

// Threshold sweep: 79% silent, 80% warning, 89% silent, 90% warning,
// 99% silent, 100% stopped + blocked. No repeats.
func TestBudgetThresholdNotifications(t *testing.T) {
	s, store := guardServer(t)
	userID := mustUser(t, store, "thresh@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "thresh")
	require.NoError(t, store.SetUserBudget(context.Background(), userID, 100, "admin"))

	spend(t, store, squadID, agentID, 79)
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.Empty(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning), "79% must not notify")
	require.False(t, blockedNow(t, store, userID))

	spend(t, store, squadID, agentID, 1) // 80%
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning), 1, "80% warns once")
	require.False(t, blockedNow(t, store, userID))

	spend(t, store, squadID, agentID, 9) // 89%
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning), 1, "89% adds no warning")

	spend(t, store, squadID, agentID, 1) // 90%
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning), 2, "90% warns once more")
	require.False(t, blockedNow(t, store, userID))

	spend(t, store, squadID, agentID, 9) // 99%
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning), 2, "99% adds nothing")
	require.Empty(t, inboxByKind(t, store, userID, domain.InboxBudgetStopped))

	spend(t, store, squadID, agentID, 1) // 100%
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.True(t, blockedNow(t, store, userID), "100% blocks")
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetStopped), 1)

	// Re-evaluation at the same spend: no duplicate notifications.
	evalUser(t, s, userID, domain.PlatformBudgets{})
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning), 2)
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetStopped), 1)

	// The stop message mentions the stop + resume semantics.
	stopped := inboxByKind(t, store, userID, domain.InboxBudgetStopped)[0]
	require.Contains(t, stopped.Body, "stopped")
	require.Contains(t, stopped.Body, "Raising")
	require.Empty(t, stopped.SquadID, "budget notifications are user-level (no squad)")
}

// No budget row and no platform default ⇒ unlimited: never blocked, no
// notifications, even at huge spend.
func TestNoBudgetIsUnlimited(t *testing.T) {
	s, store := guardServer(t)
	userID := mustUser(t, store, "free@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "free")

	spend(t, store, squadID, agentID, 99999)
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.False(t, blockedNow(t, store, userID))
	require.Empty(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning))
	require.Empty(t, inboxByKind(t, store, userID, domain.InboxBudgetStopped))
}

// Platform default applies when the user has no own budget row.
func TestPlatformDefaultIsEffectiveBudget(t *testing.T) {
	s, store := guardServer(t)
	userID := mustUser(t, store, "defaulted@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "defaulted")
	def := 50.0

	spend(t, store, squadID, agentID, 40) // 80% of default
	evalUser(t, s, userID, domain.PlatformBudgets{DefaultMonthlyUSD: &def})
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning), 1)

	spend(t, store, squadID, agentID, 10) // 100% of default
	evalUser(t, s, userID, domain.PlatformBudgets{DefaultMonthlyUSD: &def})
	require.True(t, blockedNow(t, store, userID))
}

// Own budget row wins over the platform default.
func TestOwnBudgetOverridesDefault(t *testing.T) {
	s, store := guardServer(t)
	userID := mustUser(t, store, "own@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "own")
	require.NoError(t, store.SetUserBudget(context.Background(), userID, 1000, "admin"))
	def := 10.0

	spend(t, store, squadID, agentID, 15) // over the default, under own budget
	evalUser(t, s, userID, domain.PlatformBudgets{DefaultMonthlyUSD: &def})
	require.False(t, blockedNow(t, store, userID), "own budget must win over the default")
	require.Empty(t, inboxByKind(t, store, userID, domain.InboxBudgetStopped))
}

// Resume: blocked user → admin raises the budget above MTD → block
// cleared → the busy transition (agent start) is allowed again.
func TestResumeOnBudgetRaise(t *testing.T) {
	s, store := guardServer(t)
	ctx := context.Background()
	userID := mustUser(t, store, "resume@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "resume")
	require.NoError(t, store.SetUserBudget(ctx, userID, 10, "admin"))

	spend(t, store, squadID, agentID, 12)
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.True(t, blockedNow(t, store, userID))

	// While blocked: the busy transition (wake/start path) is refused
	// and the agent stays idle.
	require.NoError(t, s.setAgentStatusAndMirror(ctx, agentID, domain.AgentBusy))
	agent, err := store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.Equal(t, domain.AgentIdle, agent.Status, "blocked owner's agent must not start")

	// Raise the budget above MTD spend.
	require.NoError(t, store.SetUserBudget(ctx, userID, 50, "admin"))
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.False(t, blockedNow(t, store, userID), "raise above spend clears the block")

	// Scheduling resumes: busy transition now sticks.
	require.NoError(t, s.setAgentStatusAndMirror(ctx, agentID, domain.AgentBusy))
	agent, err = store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.Equal(t, domain.AgentBusy, agent.Status)
}

// A running (busy) agent keeps its busy status while its owner is
// blocked — the stop happens at end of turn, never mid-turn.
func TestBusyAgentSurvivesBlockMidTurn(t *testing.T) {
	s, store := guardServer(t)
	ctx := context.Background()
	userID := mustUser(t, store, "midturn@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "midturn")
	require.NoError(t, store.SetAgentStatus(ctx, agentID, domain.AgentBusy))
	require.NoError(t, store.SetUserBudget(ctx, userID, 5, "admin"))

	spend(t, store, squadID, agentID, 6)
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.True(t, blockedNow(t, store, userID))

	// Busy heartbeat from the running agent keeps it busy (no mid-turn kill).
	require.NoError(t, s.setAgentStatusAndMirror(ctx, agentID, domain.AgentBusy))
	agent, err := store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.Equal(t, domain.AgentBusy, agent.Status)

	// End of turn: the idle transition still works (pod tears down via
	// the mirror's blocked idle-timeout override).
	require.NoError(t, s.setAgentStatusAndMirror(ctx, agentID, domain.AgentIdle))
	agent, err = store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.Equal(t, domain.AgentIdle, agent.Status)
}

// Platform-wide limit aggregation: the limit is compared against the
// total spend of ALL users and blocks every squad owner the same way.
func TestPlatformLimitAggregatesAcrossUsers(t *testing.T) {
	s, store := guardServer(t)
	ctx := context.Background()
	alice := mustUser(t, store, "alice-p@example.com")
	bob := mustUser(t, store, "bob-p@example.com")
	aSquad, aAgent := seedSquadAgent(t, store, alice, "pa")
	bSquad, bAgent := seedSquadAgent(t, store, bob, "pb")
	limit := 100.0
	platform := domain.PlatformBudgets{PlatformMonthlyLimitUSD: &limit}

	// 40 + 30 = 70: under the limit, nobody blocked or notified.
	spend(t, store, aSquad, aAgent, 40)
	spend(t, store, bSquad, bAgent, 30)
	require.NoError(t, s.evaluatePlatformBudget(ctx, time.Now().UTC(), platform))
	require.False(t, blockedNow(t, store, alice))
	require.False(t, blockedNow(t, store, bob))
	require.Empty(t, inboxByKind(t, store, alice, domain.InboxBudgetWarning))

	// 85 + 20 = 105 ≥ limit: both owners blocked with platform-sourced
	// notifications (80/90/100 markers).
	spend(t, store, aSquad, aAgent, 45)
	spend(t, store, bSquad, bAgent, 20)
	require.NoError(t, s.evaluatePlatformBudget(ctx, time.Now().UTC(), platform))
	require.True(t, blockedNow(t, store, alice))
	require.True(t, blockedNow(t, store, bob))
	require.Len(t, inboxByKind(t, store, alice, domain.InboxBudgetStopped), 1)
	require.Len(t, inboxByKind(t, store, bob, domain.InboxBudgetStopped), 1)
	require.Len(t, inboxByKind(t, store, bob, domain.InboxBudgetWarning), 2, "80% + 90% platform warnings")
	require.Contains(t, inboxByKind(t, store, bob, domain.InboxBudgetStopped)[0].Body, "platform")

	// Re-evaluation adds no duplicates.
	require.NoError(t, s.evaluatePlatformBudget(ctx, time.Now().UTC(), platform))
	require.Len(t, inboxByKind(t, store, alice, domain.InboxBudgetStopped), 1)
	require.Len(t, inboxByKind(t, store, alice, domain.InboxBudgetWarning), 2)
}

// Clearing the platform limit (explicit null) resumes everyone it had
// blocked — and only them.
func TestPlatformLimitClearedResumes(t *testing.T) {
	s, store := guardServer(t)
	ctx := context.Background()
	alice := mustUser(t, store, "clear-a@example.com")
	bob := mustUser(t, store, "clear-b@example.com")
	aSquad, aAgent := seedSquadAgent(t, store, alice, "ca")
	bSquad, bAgent := seedSquadAgent(t, store, bob, "cb")
	limit := 50.0
	platform := domain.PlatformBudgets{PlatformMonthlyLimitUSD: &limit}

	spend(t, store, aSquad, aAgent, 30)
	spend(t, store, bSquad, bAgent, 25)
	require.NoError(t, s.evaluatePlatformBudget(ctx, time.Now().UTC(), platform))
	require.True(t, blockedNow(t, store, alice))
	require.True(t, blockedNow(t, store, bob))

	// Alice is ALSO blocked by her own budget — clearing the platform
	// limit must leave her blocked.
	require.NoError(t, store.SetUserBudget(ctx, alice, 5, "admin"))
	require.NoError(t, s.evaluateUserBudget(ctx, alice, time.Now().UTC(), domain.PlatformBudgets{}))

	require.NoError(t, s.evaluatePlatformBudget(ctx, time.Now().UTC(), domain.PlatformBudgets{}))
	require.True(t, blockedNow(t, store, alice), "own-budget block survives the platform clear")
	require.False(t, blockedNow(t, store, bob), "platform-only block cleared")
}

// A zero budget blocks on the first recorded cost but never blocks a
// user who has spent nothing.
func TestZeroBudgetBlocksOnFirstSpend(t *testing.T) {
	s, store := guardServer(t)
	userID := mustUser(t, store, "zero@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "zero")

	evalUser(t, s, userID, domain.PlatformBudgets{}) // no spend yet, no budget row
	// No budget row at all → unlimited; set an explicit zero budget:
	require.NoError(t, store.SetUserBudget(context.Background(), userID, 0, "admin"))
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.False(t, blockedNow(t, store, userID), "zero spend against zero budget does not block")

	spend(t, store, squadID, agentID, 0.01)
	evalUser(t, s, userID, domain.PlatformBudgets{})
	require.True(t, blockedNow(t, store, userID), "any spend over a zero budget blocks")
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetStopped), 1)
}

// Concurrent evaluations for the same user must produce exactly one
// notification per threshold and one block transition (idempotent,
// race-safe claims).
func TestConcurrentEvaluationIdempotency(t *testing.T) {
	s, store := guardServer(t)
	userID := mustUser(t, store, "race@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "race")
	require.NoError(t, store.SetUserBudget(context.Background(), userID, 10, "admin"))
	spend(t, store, squadID, agentID, 15) // 150%

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.evaluateUserBudget(context.Background(), userID, time.Now().UTC(), domain.PlatformBudgets{})
		}()
	}
	wg.Wait()

	require.True(t, blockedNow(t, store, userID))
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetWarning), 2, "exactly 80% + 90%")
	require.Len(t, inboxByKind(t, store, userID, domain.InboxBudgetStopped), 1, "exactly one stop")
}

// Period rollover: a block from a previous period is stale and does not
// block the current month; markers from the previous month do not
// suppress this month's notifications.
func TestPeriodRolloverResetsEnforcement(t *testing.T) {
	s, store := guardServer(t)
	ctx := context.Background()
	userID := mustUser(t, store, "rollover@example.com")
	lastPeriod := "2020-01"
	require.NoError(t, store.SetUserBudget(ctx, userID, 10, "admin"))

	// Simulate last month's block + markers.
	_, err := store.SetBudgetBlock(ctx, userID, lastPeriod, domain.BudgetSourceUser, true)
	require.NoError(t, err)
	claimed, err := store.ClaimBudgetNotification(ctx, userID, lastPeriod, domain.BudgetNotifyUser80)
	require.NoError(t, err)
	require.True(t, claimed)

	// This month: stale row does not block.
	require.False(t, blockedNow(t, store, userID))

	// This month's 80% marker is claimable despite last month's.
	claimed, err = store.ClaimBudgetNotification(ctx, userID, currentBudgetPeriod(time.Now()), domain.BudgetNotifyUser80)
	require.NoError(t, err)
	require.True(t, claimed, "new month re-arms notifications")

	// Writing the current period resets the stale flag.
	changed, err := store.SetBudgetBlock(ctx, userID, currentBudgetPeriod(time.Now()), domain.BudgetSourceUser, false)
	require.NoError(t, err)
	require.True(t, changed, "period rollover reports the reset")
	block, err := store.GetBudgetBlock(ctx, userID)
	require.NoError(t, err)
	require.Equal(t, currentBudgetPeriod(time.Now()), block.Period)
	require.False(t, block.BlockedByUser)
	_ = s
}

// The scheduling guard refuses wake for a blocked owner via the HTTP
// wake endpoint.
func TestWakeRefusedForBlockedOwner(t *testing.T) {
	handler, store := newBudgetFixture(t)
	ctx := context.Background()
	ownerID := userIDFor(t, handler, authOwner)
	_, agentID := seedSquadAgent(t, store, ownerID, "wakeblock")
	// Put the owner over a tiny budget and evaluate through the guard's
	// own evaluation path (what the metering ingest triggers).
	require.NoError(t, store.SetUserBudget(ctx, ownerID, 5, "admin"))
	agent, err := store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.NoError(t, store.RecordMetering(ctx, &domain.MeteringEvent{
		AgentID: agentID, SquadID: agent.SquadID, Cost: 7, Currency: "USD", Timestamp: time.Now().UTC(),
	}))
	s := &Server{store: store}
	require.NoError(t, s.evaluateUserBudget(ctx, ownerID, time.Now().UTC(), domain.PlatformBudgets{}))
	require.True(t, blockedNow(t, store, ownerID))

	var out map[string]any
	doJSONAuth(t, handler, authOwner, http.MethodPost, "/api/v1/agents/"+agentID+"/wake", nil, http.StatusOK, &out)
	require.Equal(t, false, out["waking"])
	require.Equal(t, budgetStopNotice, out["reason"])

	// And the busy mirror path stays refused too.
	require.NoError(t, s.setAgentStatusAndMirror(ctx, agentID, domain.AgentBusy))
	agent, err = store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.Equal(t, domain.AgentIdle, agent.Status)
}
