package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

// wakeFixture creates a squad + agent + identity with an APPLIED upsert_agent
// outbox event (the wake attribution anchor) and returns the agent credential.
func wakeFixture(t *testing.T, handler http.Handler, store storage.Store, crWriter *fakeCRWriter, squadName, agentName string) (domain.Squad, domain.Agent, string) {
	t.Helper()
	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{
		"name": squadName,
	}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{
		"name": agentName,
	}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	require.NotEmpty(t, credential)
	// Simulate the operator applying the agent's upsert outbox event.
	events, err := store.ListKubernetesOutbox(context.Background(), domain.KubernetesOutboxPending, 50)
	require.NoError(t, err)
	applied := 0
	for _, event := range events {
		if event.AggregateType == domain.KubernetesAggregateAgent && event.AggregateID == agent.ID && event.Operation == domain.KubernetesOpUpsertAgent {
			require.NoError(t, store.MarkKubernetesOutboxApplied(context.Background(), event.ID))
			applied++
		}
	}
	require.Positive(t, applied)
	return squad, agent, credential
}

func wakeAssignTask(t *testing.T, handler http.Handler, squadID, agentID, title string) domain.Task {
	t.Helper()
	var task domain.Task
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squadID+"/board/tasks", map[string]any{
		"title":             title,
		"assignee_agent_id": agentID,
	}, http.StatusCreated, &task)
	return task
}

func TestWakeLatencyColdStartRecordedOnClaim(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), store, crWriter)
	squad, agent, credential := wakeFixture(t, handler, store, crWriter, "Wake Cold Squad", "Wake Cold Agent")
	wakeAssignTask(t, handler, squad.ID, agent.ID, "cold wake task")

	startedAt := time.Now().UTC()
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", map[string]any{
		"started_at": startedAt.Format(time.RFC3339Nano),
	}, http.StatusOK, &claimed)

	events, err := store.ListWakeLatency(context.Background(), squad.ID, time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	got := events[0]
	require.Equal(t, agent.ID, got.AgentID)
	require.Equal(t, squad.ID, got.SquadID)
	require.Equal(t, claimed.ID, got.TaskID)
	require.True(t, got.ColdStart)
	require.Equal(t, startedAt.UTC().Truncate(time.Microsecond), got.ContainerStartedAt.UTC().Truncate(time.Microsecond))
	require.GreaterOrEqual(t, got.E2EMs, got.QueueMs)
	require.GreaterOrEqual(t, got.E2EMs, got.ScaleupMs)
	require.GreaterOrEqual(t, got.E2EMs, got.ClaimDelayMs)
	require.GreaterOrEqual(t, got.ScaleupMs, float64(0))
}

func TestWakeLatencyWarmStartNotCold(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), store, crWriter)
	squad, agent, credential := wakeFixture(t, handler, store, crWriter, "Wake Warm Squad", "Wake Warm Agent")
	wakeAssignTask(t, handler, squad.ID, agent.ID, "warm wake task")

	// Container existed an hour before the wake was even requested.
	startedAt := time.Now().UTC().Add(-time.Hour)
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", map[string]any{
		"started_at": startedAt.Format(time.RFC3339Nano),
	}, http.StatusOK, &claimed)

	events, err := store.ListWakeLatency(context.Background(), squad.ID, time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	got := events[0]
	require.False(t, got.ColdStart)
	require.Equal(t, float64(0), got.ScaleupMs)
	// Warm claim delay is measured from the CR write, not the old container start.
	require.Less(t, got.ClaimDelayMs, float64(60_000))
}

func TestWakeLatencyDedupesPerContainerStart(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), store, crWriter)
	squad, agent, credential := wakeFixture(t, handler, store, crWriter, "Wake Dedup Squad", "Wake Dedup Agent")
	wakeAssignTask(t, handler, squad.ID, agent.ID, "dedup task one")
	wakeAssignTask(t, handler, squad.ID, agent.ID, "dedup task two")

	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	var first domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", map[string]any{
		"started_at": startedAt,
	}, http.StatusOK, &first)
	// Complete the first task so the second claim succeeds on the same container.
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+first.ID+"/complete", map[string]any{
		"status":        string(domain.TaskDone),
		"execution_id":  first.ExecutionID,
		"fencing_token": first.FencingToken,
	}, http.StatusOK, &domain.Task{})
	var second domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", map[string]any{
		"started_at": startedAt,
	}, http.StatusOK, &second)

	events, err := store.ListWakeLatency(context.Background(), squad.ID, time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, events, 1, "same container start records exactly one wake")
	require.Equal(t, first.ID, events[0].TaskID)
}

func TestWakeLatencySkippedWithoutStartedAtOrAppliedUpsert(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), store, crWriter)

	var squad domain.Squad
	doJSON(t, handler, http.MethodPost, "/api/v1/squads", map[string]any{"name": "Wake Skip Squad"}, http.StatusCreated, &squad)
	var agent domain.Agent
	doJSON(t, handler, http.MethodPost, "/api/v1/squads/"+squad.ID+"/agents", map[string]any{"name": "Skip Agent"}, http.StatusCreated, &agent)
	var identity domain.AgentIdentity
	doJSON(t, handler, http.MethodPost, "/api/v1/agents/"+agent.ID+"/identity", nil, http.StatusCreated, &identity)
	credential := crWriter.credentialTokens[identity.CredentialRef]
	wakeAssignTask(t, handler, squad.ID, agent.ID, "skipped wake task")

	// No started_at → nothing recorded.
	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", nil, http.StatusOK, &claimed)
	events, err := store.ListWakeLatency(context.Background(), squad.ID, time.Time{}, 10)
	require.NoError(t, err)
	require.Empty(t, events)

	// started_at but the upsert is still pending (not applied) → nothing recorded.
	complete := domain.Task{}
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/"+claimed.ID+"/complete", map[string]any{
		"status":        string(domain.TaskDone),
		"execution_id":  claimed.ExecutionID,
		"fencing_token": claimed.FencingToken,
	}, http.StatusOK, &complete)
	wakeAssignTask(t, handler, squad.ID, agent.ID, "second skipped wake task")
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", map[string]any{
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	}, http.StatusOK, &claimed)
	events, err = store.ListWakeLatency(context.Background(), squad.ID, time.Time{}, 10)
	require.NoError(t, err)
	require.Empty(t, events, "no applied upsert means no attributable wake")
}

func TestWakeLatencySummaryEndpoint(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	crWriter := &fakeCRWriter{}
	handler := NewWithCRWriter(testConfig(), store, crWriter)
	squad, agent, credential := wakeFixture(t, handler, store, crWriter, "Wake API Squad", "Wake API Agent")
	wakeAssignTask(t, handler, squad.ID, agent.ID, "api wake task")

	var claimed domain.Task
	doAgentJSON(t, handler, agent.ID, credential, http.MethodPost, "/api/v1/agents/me/tasks/claim", map[string]any{
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	}, http.StatusOK, &claimed)

	var resp struct {
		Since   time.Time                 `json:"since"`
		Events  []domain.WakeLatencyEvent `json:"events"`
		Summary struct {
			Count           int     `json:"count"`
			ColdStarts      int     `json:"cold_starts"`
			P50E2EMs        float64 `json:"p50_e2e_ms"`
			P95E2EMs        float64 `json:"p95_e2e_ms"`
			P99E2EMs        float64 `json:"p99_e2e_ms"`
			MaxE2EMs        float64 `json:"max_e2e_ms"`
			P95QueueMs      float64 `json:"p95_queue_ms"`
			P95ScaleupMs    float64 `json:"p95_scaleup_ms"`
			P95ClaimDelayMs float64 `json:"p95_claim_delay_ms"`
			SloTargetMs     float64 `json:"slo_target_ms"`
			SloMet          bool    `json:"slo_met"`
		} `json:"summary"`
	}
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/wake-latency", nil, http.StatusOK, &resp)
	require.Len(t, resp.Events, 1)
	require.Equal(t, 1, resp.Summary.Count)
	require.Equal(t, 1, resp.Summary.ColdStarts)
	require.Equal(t, float64(20000), resp.Summary.SloTargetMs)
	require.True(t, resp.Summary.SloMet)
	require.LessOrEqual(t, resp.Summary.P95E2EMs, resp.Summary.MaxE2EMs)

	// Invalid since → 400.
	doJSONNoBody(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/wake-latency?since=yesterday", nil, http.StatusBadRequest)

	// since in the future → empty but valid.
	var emptyResp struct {
		Events  []domain.WakeLatencyEvent `json:"events"`
		Summary struct {
			Count  int  `json:"count"`
			SloMet bool `json:"slo_met"`
		} `json:"summary"`
	}
	doJSON(t, handler, http.MethodGet, "/api/v1/squads/"+squad.ID+"/wake-latency?since="+time.Now().UTC().Add(time.Hour).Format(time.RFC3339), nil, http.StatusOK, &emptyResp)
	require.Empty(t, emptyResp.Events)
	require.Equal(t, 0, emptyResp.Summary.Count)
	require.False(t, emptyResp.Summary.SloMet)
}

func TestNearestRankPercentile(t *testing.T) {
	t.Parallel()
	require.Equal(t, 0.0, nearestRankPercentile(nil, 0.95))
	values := []float64{30, 10, 20}
	require.Equal(t, 10.0, nearestRankPercentile(values, 0.01))
	require.Equal(t, 20.0, nearestRankPercentile(values, 0.34))
	require.Equal(t, 20.0, nearestRankPercentile(values, 0.5))
	require.Equal(t, 30.0, nearestRankPercentile(values, 0.95))
	require.Equal(t, 30.0, nearestRankPercentile(values, 1.0))
}
