package httpapi

// S-197: stuck-task scanner tests.
//
// Coverage:
//   - stale in-progress task ⇒ one task_stuck notification to the squad owner
//   - fresh task row / recent thread message / recent heartbeat ⇒ no alert
//   - dedupe: second sweep within the window files nothing
//   - expired window ⇒ the task re-alerts
//   - S-199 mute preference ⇒ no alert
//   - non in-progress tasks are never considered
//   - the RunStuckTaskScanner loop sweeps and stops on cancel
//
// Timing convention: threshold 100ms with 150ms sleeps — wide enough for
// CI jitter, small enough to keep the suite fast.

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	stuckThreshold = 100 * time.Millisecond
	stuckSleep     = 150 * time.Millisecond
)

type stuckFixture struct {
	store *storage.MemoryStore
	owner string
	squad *domain.Squad
	agent *domain.Agent
	board *domain.Board
}

func newStuckFixture(t *testing.T, name string) *stuckFixture {
	t.Helper()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	ownerUser, err := store.UpsertUser(ctx, &domain.User{Email: "owner-" + name + "@example.com", Name: "owner-" + name})
	require.NoError(t, err)
	owner := ownerUser.ID
	squad, err := store.CreateSquad(ctx, &domain.Squad{Name: name, OwnerID: owner, Status: domain.SquadActive})
	require.NoError(t, err)
	agent, err := store.CreateAgent(ctx, &domain.Agent{SquadID: squad.ID, Name: name + "-agent", Status: domain.AgentIdle})
	require.NoError(t, err)
	board, err := store.GetBoard(ctx, squad.ID)
	require.NoError(t, err)
	return &stuckFixture{store: store, owner: owner, squad: squad, agent: agent, board: board}
}

func (f *stuckFixture) createTask(t *testing.T, title string, status domain.TaskStatus) *domain.Task {
	t.Helper()
	task, err := f.store.CreateTask(context.Background(), &domain.Task{
		BoardID: f.board.ID, SquadID: f.squad.ID, Title: title,
		Status: status, AssigneeAgentID: f.agent.ID,
		CreatedByType: "user", CreatedByID: f.owner,
	})
	require.NoError(t, err)
	return task
}

func (f *stuckFixture) notifications(t *testing.T) []*domain.Notification {
	t.Helper()
	items, err := f.store.ListNotifications(context.Background(), f.owner, false, 100)
	require.NoError(t, err)
	return items
}

func TestStuckScannerFilesStaleInProgressTask(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-stale")
	task := f.createTask(t, "abandoned", domain.TaskInProgress)
	time.Sleep(stuckSleep)

	filed, err := ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Equal(t, 1, filed)

	items := f.notifications(t)
	require.Len(t, items, 1)
	require.Equal(t, domain.NotificationTaskStuck, items[0].Type)
	require.Equal(t, domain.NotificationWarning, items[0].Severity)
	require.Equal(t, task.ID, items[0].TaskID)
	require.Equal(t, f.agent.ID, items[0].AgentID)
	require.Contains(t, items[0].Message, "T-"+strconv.Itoa(task.TaskNumber))
	require.Contains(t, items[0].Message, "in-progress")
}

func TestStuckScannerSkipsFreshTask(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-fresh")
	f.createTask(t, "just moved", domain.TaskInProgress)

	filed, err := ScanStuckTasksOnce(context.Background(), f.store, time.Hour)
	require.NoError(t, err)
	require.Zero(t, filed)
	require.Empty(t, f.notifications(t))
}

func TestStuckScannerThreadActivitySuppresses(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-thread")
	task := f.createTask(t, "quiet but talking", domain.TaskInProgress)
	time.Sleep(stuckSleep)

	// A task-thread message (payload carries task_id) counts as activity.
	payload, err := json.Marshal(map[string]string{"message": "still working", "task_id": task.ID})
	require.NoError(t, err)
	_, err = f.store.CreateMessage(context.Background(), &domain.Message{
		FromType: "agent", FromID: f.agent.ID, ToAgentID: f.agent.ID,
		SquadID: f.squad.ID, Type: domain.MessagePing, Payload: payload,
		Status: domain.MessageDelivered,
	})
	require.NoError(t, err)

	filed, err := ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Zero(t, filed)
}

func TestStuckScannerHeartbeatSuppresses(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-heartbeat")
	task := f.createTask(t, "claimed and beating", domain.TaskTodo)
	claimed, err := f.store.ClaimNextTask(context.Background(), f.agent.ID, "worker-1", time.Hour)
	require.NoError(t, err)
	require.Equal(t, task.ID, claimed.ID)
	time.Sleep(stuckSleep)

	_, err = f.store.HeartbeatTaskExecution(context.Background(), f.agent.ID, claimed.ExecutionID, claimed.FencingToken, time.Hour)
	require.NoError(t, err)

	filed, err := ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Zero(t, filed)
}

func TestStuckScannerDedupeWithinWindow(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-dedupe")
	f.createTask(t, "stuck once", domain.TaskInProgress)
	time.Sleep(stuckSleep)

	filed, err := ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Equal(t, 1, filed)

	// Immediate second sweep: the prior alert is inside the window.
	filed, err = ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Zero(t, filed)
	require.Len(t, f.notifications(t), 1)
}

func TestStuckScannerReAlertsAfterWindow(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-realert")
	f.createTask(t, "stuck forever", domain.TaskInProgress)
	time.Sleep(stuckSleep)

	filed, err := ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Equal(t, 1, filed)

	// Once the dedupe window passes, the still-stuck task alerts again.
	time.Sleep(stuckSleep)
	filed, err = ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Equal(t, 1, filed)
	require.Len(t, f.notifications(t), 2)
}

func TestStuckScannerRespectsMutePreference(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-muted")
	f.createTask(t, "nobody wants this alert", domain.TaskInProgress)
	_, err := f.store.SetNotificationPreferences(context.Background(), f.owner, []domain.NotificationType{domain.NotificationTaskStuck})
	require.NoError(t, err)
	time.Sleep(stuckSleep)

	filed, err := ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Zero(t, filed)
	require.Empty(t, f.notifications(t))
}

func TestStuckScannerIgnoresNonInProgressTasks(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-other")
	f.createTask(t, "todo stays quiet", domain.TaskTodo)
	f.createTask(t, "done stays quiet", domain.TaskDone)
	time.Sleep(stuckSleep)

	filed, err := ScanStuckTasksOnce(context.Background(), f.store, stuckThreshold)
	require.NoError(t, err)
	require.Zero(t, filed)
}

func TestRunStuckTaskScannerSweepsAndStops(t *testing.T) {
	t.Parallel()
	f := newStuckFixture(t, "s197-loop")
	f.createTask(t, "looped", domain.TaskInProgress)
	time.Sleep(stuckSleep)

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunStuckTaskScanner(runCtx, f.store, 10*time.Millisecond, stuckThreshold)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(f.notifications(t)) > 0 {
			break
		}
		require.True(t, time.Now().Before(deadline), "scanner never filed a task_stuck notification")
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stuck scanner did not stop on context cancel")
	}
}

func TestFormatStuckThreshold(t *testing.T) {
	require.Equal(t, "24h", formatStuckThreshold(24*time.Hour))
	require.Equal(t, "90m", formatStuckThreshold(90*time.Minute))
	require.Equal(t, "100ms", formatStuckThreshold(100*time.Millisecond))
}
