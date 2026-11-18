package httpapi

// S-321 / S-203 WP3-R3 (WI-9): metrics smoke tests — every budget
// enforcement signal must actually increment its Prometheus series on
// the rejection/error path. Counters are process-global, so each test
// asserts the DELTA around its action, not absolute values.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/metrics"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// histogramCountSum collects a histogram collector and returns the
// total sample count and sum across its label combinations.
func histogramCountSum(t *testing.T, c prometheus.Collector) (uint64, float64) {
	t.Helper()
	ch := make(chan prometheus.Metric, 4096)
	c.Collect(ch)
	close(ch)
	var count uint64
	var sum float64
	for m := range ch {
		var dm dto.Metric
		require.NoError(t, m.Write(&dm))
		if h := dm.GetHistogram(); h != nil {
			count += h.GetSampleCount()
			sum += h.GetSampleSum()
		}
	}
	return count, sum
}

func counterDelta(t *testing.T, before, after float64) float64 { return after - before }

func readCounter(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := cv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	var dm dto.Metric
	require.NoError(t, c.Write(&dm))
	return dm.GetCounter().GetValue()
}

// A confirmed-block chat send must increment
// turns_rejected_budget_total{source="user",site="send"}.
func TestMetricsSendRejectionCounted(t *testing.T) {
	handler, store := newBudgetFixture(t)
	ctx := context.Background()
	ownerID := userIDFor(t, handler, authOwner)
	squadID, agentID := seedSquadAgent(t, store, ownerID, "metrsend")
	require.NoError(t, store.SetUserBudget(ctx, ownerID, 10, "admin"))
	spend(t, store, squadID, agentID, 12)
	s := &Server{store: store}
	require.NoError(t, s.evaluateUserBudget(ctx, ownerID, time.Now().UTC(), domain.PlatformBudgets{}))

	before := readCounter(t, metrics.TurnsRejectedBudget, domain.BudgetSourceUser, "send")
	rec := doAuthRaw(t, handler, authOwner, http.MethodPost, "/api/v1/agents/"+agentID+"/chat", map[string]any{"message": "hello?"})
	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	after := readCounter(t, metrics.TurnsRejectedBudget, domain.BudgetSourceUser, "send")
	require.Equal(t, 1.0, counterDelta(t, before, after), "blocked send must count one rejected turn (site=send)")
}

// A refused wake must increment
// turns_rejected_budget_total{source="user",site="wake"}.
func TestMetricsWakeRejectionCounted(t *testing.T) {
	handler, store := newBudgetFixture(t)
	ctx := context.Background()
	ownerID := userIDFor(t, handler, authOwner)
	_, agentID := seedSquadAgent(t, store, ownerID, "metrwake")
	_, err := store.SetBudgetBlock(ctx, ownerID, currentBudgetPeriod(time.Now()), domain.BudgetSourceUser, true)
	require.NoError(t, err)

	before := readCounter(t, metrics.TurnsRejectedBudget, domain.BudgetSourceUser, "wake")
	rec := doAuthRaw(t, handler, authOwner, http.MethodPost, "/api/v1/agents/"+agentID+"/wake", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	after := readCounter(t, metrics.TurnsRejectedBudget, domain.BudgetSourceUser, "wake")
	require.Equal(t, 1.0, counterDelta(t, before, after), "refused wake must count one rejected turn (site=wake)")
}

// A refused idle→busy transition at the chokepoint must increment
// turns_rejected_budget_total{source="user",site="chokepoint"}; the
// already-busy pass-through must NOT count.
func TestMetricsChokepointRejectionCounted(t *testing.T) {
	base := storage.NewMemoryStore()
	f := &budgetErrStore{MemoryStore: base}
	s := &Server{store: f}
	ctx := context.Background()
	userID := mustUser(t, base, "metrchoke@example.com")
	_, agentID := seedSquadAgent(t, base, userID, "metrchoke")
	_, err := base.SetBudgetBlock(ctx, userID, currentBudgetPeriod(time.Now()), domain.BudgetSourceUser, true)
	require.NoError(t, err)

	before := readCounter(t, metrics.TurnsRejectedBudget, domain.BudgetSourceUser, "chokepoint")
	require.NoError(t, s.setAgentStatusAndMirror(ctx, agentID, domain.AgentBusy))
	after := readCounter(t, metrics.TurnsRejectedBudget, domain.BudgetSourceUser, "chokepoint")
	require.Equal(t, 1.0, counterDelta(t, before, after), "refused idle→busy must count one rejected turn")

	// Already-busy pass-through: admitted turn, not a rejection.
	passBefore := readCounter(t, metrics.TurnsRejectedBudget, domain.BudgetSourceUser, "chokepoint")
	require.NoError(t, f.MemoryStore.SetAgentStatus(ctx, agentID, domain.AgentBusy))
	require.NoError(t, s.setAgentStatusAndMirror(ctx, agentID, domain.AgentBusy))
	passAfter := readCounter(t, metrics.TurnsRejectedBudget, domain.BudgetSourceUser, "chokepoint")
	require.Equal(t, 0.0, counterDelta(t, passBefore, passAfter), "busy pass-through must not count as rejected")
}

// A forced check failure on an admission path must increment
// budget_check_errors_total{site="admission"} (fail-closed visibility).
func TestMetricsAdmissionCheckErrorCounted(t *testing.T) {
	base := storage.NewMemoryStore()
	f := &budgetErrStore{MemoryStore: base}
	s := &Server{store: f}
	ctx := context.Background()
	userID := mustUser(t, base, "metradm@example.com")
	_, agentID := seedSquadAgent(t, base, userID, "metradm")
	f.failBudgetCheck = true

	before := readCounter(t, metrics.BudgetCheckErrors, "admission")
	require.Equal(t, BudgetUnverifiable, s.checkAgentBudget(ctx, agentID))
	after := readCounter(t, metrics.BudgetCheckErrors, "admission")
	require.Equal(t, 1.0, counterDelta(t, before, after), "forced check error must count one admission error")
}

// Block creation must increment budget_blocks_created_total{source} and
// observe the accepted overrun (spend − cap) in the histogram.
func TestMetricsBlockCreatedAndOverrunObserved(t *testing.T) {
	s, store := guardServer(t)
	userID := mustUser(t, store, "metrblock@example.com")
	squadID, agentID := seedSquadAgent(t, store, userID, "metrblock")
	require.NoError(t, store.SetUserBudget(context.Background(), userID, 100, "admin"))
	spend(t, store, squadID, agentID, 130)

	beforeBlocks := readCounter(t, metrics.BudgetBlocksCreated, domain.BudgetSourceUser)
	beforeCount, beforeSum := histogramCountSum(t, metrics.BudgetOverrunDollars)

	evalUser(t, s, userID, domain.PlatformBudgets{})

	afterBlocks := readCounter(t, metrics.BudgetBlocksCreated, domain.BudgetSourceUser)
	require.Equal(t, 1.0, counterDelta(t, beforeBlocks, afterBlocks), "new block must count once")

	afterCount, afterSum := histogramCountSum(t, metrics.BudgetOverrunDollars)
	require.GreaterOrEqual(t, afterCount, beforeCount+1, "overrun must be observed")
	// This test's own observation contributes exactly 30.00; other
	// tests may have added samples, so assert the delta sum is a
	// multiple consistent with at least our 30.
	require.GreaterOrEqual(t, afterSum-beforeSum, 30.0-1e-9, "overrun sum must include this test's 30.00")
}

// remirrorUserAgents must observe the fan-out duration histogram.
func TestMetricsFanoutDurationObserved(t *testing.T) {
	s, store := guardServer(t)
	userID := mustUser(t, store, "metrfan@example.com")
	seedSquadAgent(t, store, userID, "metrfan")

	before, _ := histogramCountSum(t, metrics.BudgetFanoutRemirrorDuration)
	s.remirrorUserAgents(context.Background(), userID)
	after, _ := histogramCountSum(t, metrics.BudgetFanoutRemirrorDuration)
	require.GreaterOrEqual(t, after, before+1, "fan-out must observe duration")
}

// The /metrics endpoint must serve the registry in Prometheus text
// format and include the budget series family.
func TestMetricsEndpointServesBudgetSeries(t *testing.T) {
	handler, store := newBudgetFixture(t)
	ctx := context.Background()
	ownerID := userIDFor(t, handler, authOwner)
	_, err := store.SetBudgetBlock(ctx, ownerID, currentBudgetPeriod(time.Now()), domain.BudgetSourceUser, true)
	require.NoError(t, err)
	// Touch one series so the family is present.
	metrics.BudgetBlocksCreated.WithLabelValues(domain.BudgetSourceUser).Inc()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "budget_blocks_created_total")
	require.Contains(t, string(body), "budget_check_errors_total")
	require.Contains(t, string(body), "budget_overrun_dollars")
	require.Contains(t, string(body), "budget_fanout_remirror_duration_seconds")
}
