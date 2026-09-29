package httpapi

// S-174: agent inbox tab + dead-letter replay (owner) and the admin
// dead-letter screen (replay/prune).
// S-173: consult timeout/SLA — deadline stamping and the sweeper that
// notifies the asker exactly once when a consult goes unanswered.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const (
	pathInboxSuffix      = "/inbox"
	pathReplay           = "/replay"
	pathAdminDeadLetters = "/api/v1/admin/dead-letters/"
)

type s174Fixture struct {
	handler    http.Handler
	store      *storage.MemoryStore
	squadID    string
	senderID   string
	workerID   string
	senderCred string
	workerCred string
}

func newS174Fixture(t *testing.T) *s174Fixture {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.OIDCAdminGroups = []string{platformAdminGroup}
	crWriter := &fakeCRWriter{}
	store := storage.NewMemoryStore()
	handler := NewWithDependencies(cfg, store, headerOIDC{
		authOwner: {Issuer: testIssuer, Subject: "own-1", Email: "owner@example.com", EmailVerified: true, Name: "Owner"},
		authAdmin: {Issuer: testIssuer, Subject: "adm-1", Email: adminEmail, EmailVerified: true, Name: "Admin", Groups: []string{platformAdminGroup}},
		authAlice: {Issuer: testIssuer, Subject: "alice-1", Email: aliceEmail, EmailVerified: true, Name: "Alice"},
	}, crWriter, nil)

	var squad domain.Squad
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquads, map[string]any{"name": "Inbox Squad"}, http.StatusCreated, &squad)
	var sender domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Sender"}, http.StatusCreated, &sender)
	var worker domain.Agent
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathSquadsPrefix+squad.ID+pathAgents, map[string]any{"name": "Worker"}, http.StatusCreated, &worker)
	var senderID domain.AgentIdentity
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathAgentsPrefix+sender.ID+pathIdentity, nil, http.StatusCreated, &senderID)
	var workerID domain.AgentIdentity
	doJSONAuth(t, handler, authOwner, http.MethodPost, pathAgentsPrefix+worker.ID+pathIdentity, nil, http.StatusCreated, &workerID)

	return &s174Fixture{
		handler:    handler,
		store:      store,
		squadID:    squad.ID,
		senderID:   sender.ID,
		workerID:   worker.ID,
		senderCred: crWriter.credentialTokens[senderID.CredentialRef],
		workerCred: crWriter.credentialTokens[workerID.CredentialRef],
	}
}

// deadConsultOnWorker sends a one-attempt consult from sender to worker
// and fails it on the worker, returning the dead row.
func (f *s174Fixture) deadConsultOnWorker(t *testing.T, reason string) domain.Message {
	t.Helper()
	var sent domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "consult", "message": "die trying", "max_attempts": 1,
	}, http.StatusCreated, &sent)
	var dead domain.Message
	doAgentJSON(t, f.handler, f.workerID, f.workerCred, http.MethodPost, pathMyMessagesPrefix+sent.ID+"/fail", map[string]any{"reason": reason}, http.StatusOK, &dead)
	require.Equal(t, domain.MessageDead, dead.Status)
	return dead
}

func (f *s174Fixture) inbox(t *testing.T, bearer, agentID string) domain.AgentInboxSnapshot {
	t.Helper()
	var snap domain.AgentInboxSnapshot
	doJSONAuth(t, f.handler, bearer, http.MethodGet, pathAgentsPrefix+agentID+pathInboxSuffix, nil, http.StatusOK, &snap)
	return snap
}

func TestS174InboxSectionsAndCounts(t *testing.T) {
	f := newS174Fixture(t)

	// One pending message (never attempted).
	var pending domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "consult", "message": "waiting",
	}, http.StatusCreated, &pending)

	snap := f.inbox(t, authOwner, f.workerID)
	require.Equal(t, 1, snap.PendingCount)
	require.NotNil(t, snap.OldestPendingAt)
	require.Equal(t, 0, snap.RetryingCount)
	require.Empty(t, snap.Dead)

	// Deliver it.
	doAgentJSONNoBody(t, f.handler, f.workerID, f.workerCred, http.MethodPost, pathMyMessagesPrefix+pending.ID+"/ack", nil, http.StatusOK)
	snap = f.inbox(t, authOwner, f.workerID)
	require.Equal(t, 0, snap.PendingCount)
	require.Equal(t, 1, snap.DeliveredCount)
	require.Equal(t, pending.ID, snap.Delivered[0].ID)

	// Dead letter with its terminal reason visible.
	dead := f.deadConsultOnWorker(t, "runtime exploded")
	snap = f.inbox(t, authOwner, f.workerID)
	require.Equal(t, 1, snap.DeadCount)
	require.Equal(t, dead.ID, snap.Dead[0].ID)
	require.Equal(t, "runtime exploded", snap.Dead[0].TerminalReason)

	// Admin sees it too; a stranger does not.
	adminSnap := f.inbox(t, authAdmin, f.workerID)
	require.Equal(t, 1, adminSnap.DeadCount)
	var stranger struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSONAuth(t, f.handler, authAlice, http.MethodGet, pathAgentsPrefix+f.workerID+pathInboxSuffix, nil, http.StatusForbidden, &stranger)
	require.Equal(t, "forbidden", stranger.Error.Code)
}

func TestS174OwnerReplayPutsDeadLetterBack(t *testing.T) {
	f := newS174Fixture(t)
	dead := f.deadConsultOnWorker(t, "boom")

	var replayed domain.Message
	doJSONAuth(t, f.handler, authOwner, http.MethodPost,
		pathAgentsPrefix+f.workerID+"/messages/"+dead.ID+pathReplay, nil, http.StatusOK, &replayed)
	require.Equal(t, domain.MessagePending, replayed.Status)
	require.Equal(t, 0, replayed.Attempts)
	require.Empty(t, replayed.TerminalReason)
	require.False(t, replayed.ExpiresAt.Before(time.Now().UTC()))

	// Audit event names the replay.
	audits, err := f.store.ListAudit(context.Background(), f.squadID, 50)
	require.NoError(t, err)
	var found bool
	for _, entry := range audits {
		if entry.Action == auditDeadLetterReplayed && entry.ResourceID == dead.ID {
			found = true
		}
	}
	require.True(t, found, "dead_letter.replayed audit must be recorded")

	// Replaying a live (pending) message is a conflict.
	var conflict struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSONAuth(t, f.handler, authOwner, http.MethodPost,
		pathAgentsPrefix+f.workerID+"/messages/"+dead.ID+pathReplay, nil, http.StatusConflict, &conflict)
	require.Equal(t, "not_dead", conflict.Error.Code)
}

func TestS174ReplayRejectsWrongAgentAndOutsider(t *testing.T) {
	f := newS174Fixture(t)
	dead := f.deadConsultOnWorker(t, "boom")

	// Message is addressed to the worker; asking under the sender's agent id 404s.
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSONAuth(t, f.handler, authOwner, http.MethodPost,
		pathAgentsPrefix+f.senderID+"/messages/"+dead.ID+pathReplay, nil, http.StatusNotFound, &errBody)

	// Non-owner, non-admin cannot replay.
	doJSONAuth(t, f.handler, authAlice, http.MethodPost,
		pathAgentsPrefix+f.workerID+"/messages/"+dead.ID+pathReplay, nil, http.StatusForbidden, &errBody)
}

func TestS174AdminDeadLetterScreen(t *testing.T) {
	f := newS174Fixture(t)
	deadA := f.deadConsultOnWorker(t, "connection refused")
	deadB := f.deadConsultOnWorker(t, "timeout on delivery")

	// List all dead letters (admin only).
	var all struct {
		DeadLetters []domain.Message `json:"dead_letters"`
	}
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters", nil, http.StatusOK, &all)
	require.Len(t, all.DeadLetters, 2)

	// Filter by agent → only the worker's.
	var byAgent struct {
		DeadLetters []domain.Message `json:"dead_letters"`
	}
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters?agent="+f.workerID, nil, http.StatusOK, &byAgent)
	require.Len(t, byAgent.DeadLetters, 2)
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters?agent="+f.senderID, nil, http.StatusOK, &byAgent)
	require.Empty(t, byAgent.DeadLetters)

	// Filter by reason substring.
	var byReason struct {
		DeadLetters []domain.Message `json:"dead_letters"`
	}
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters?reason=connection", nil, http.StatusOK, &byReason)
	require.Len(t, byReason.DeadLetters, 1)
	require.Equal(t, deadA.ID, byReason.DeadLetters[0].ID)
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters?reason=timeout", nil, http.StatusOK, &byReason)
	require.Len(t, byReason.DeadLetters, 1)
	require.Equal(t, deadB.ID, byReason.DeadLetters[0].ID)

	// Filter by type.
	var byType struct {
		DeadLetters []domain.Message `json:"dead_letters"`
	}
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters?type=consult", nil, http.StatusOK, &byType)
	require.Len(t, byType.DeadLetters, 2)
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters?type=ping", nil, http.StatusOK, &byType)
	require.Empty(t, byType.DeadLetters)

	// Squad owner is not the platform admin: 403.
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSONAuth(t, f.handler, authOwner, http.MethodGet, "/api/v1/admin/dead-letters", nil, http.StatusForbidden, &errBody)

	// Malformed filters are 400, not a database error.
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters?squad=not-a-uuid", nil, http.StatusBadRequest, &errBody)
	doJSONAuth(t, f.handler, authAdmin, http.MethodGet, "/api/v1/admin/dead-letters?since=yesterday", nil, http.StatusBadRequest, &errBody)
}

func TestS174AdminReplayAndPrune(t *testing.T) {
	f := newS174Fixture(t)
	dead := f.deadConsultOnWorker(t, "boom")
	live := f.pendingOnWorker(t)

	// Admin replay works across the agent boundary.
	var replayed domain.Message
	doJSONAuth(t, f.handler, authAdmin, http.MethodPost, pathAdminDeadLetters+dead.ID+pathReplay, nil, http.StatusOK, &replayed)
	require.Equal(t, domain.MessagePending, replayed.Status)

	// Prune is dead-only.
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	doJSONAuth(t, f.handler, authAdmin, http.MethodPost, pathAdminDeadLetters+live.ID+pathReplay, nil, http.StatusConflict, &errBody)
	doJSONAuth(t, f.handler, authAdmin, http.MethodDelete, pathAdminDeadLetters+live.ID, nil, http.StatusConflict, &errBody)

	// Prune the dead row: gone, and audited.
	deadAgain := f.deadConsultOnWorker(t, "prune me")
	doDelete(t, f.handler, authAdmin, pathAdminDeadLetters+deadAgain.ID, http.StatusNoContent)
	_, err := f.store.GetMessage(context.Background(), deadAgain.ID)
	require.ErrorIs(t, err, storage.ErrNotFound)

	audits, err := f.store.ListAudit(context.Background(), f.squadID, 50)
	require.NoError(t, err)
	var found bool
	for _, entry := range audits {
		if entry.Action == auditDeadLetterPruned && entry.ResourceID == deadAgain.ID {
			found = true
		}
	}
	require.True(t, found, "dead_letter.pruned audit must be recorded")

	// Owner cannot prune (admin-only door).
	third := f.deadConsultOnWorker(t, "not for owners")
	doJSONAuth(t, f.handler, authOwner, http.MethodDelete, pathAdminDeadLetters+third.ID, nil, http.StatusForbidden, &errBody)
}

// pendingOnWorker leaves a live pending message on the worker for
// replay/prune rejection tests.
func (f *s174Fixture) pendingOnWorker(t *testing.T) domain.Message {
	t.Helper()
	var sent domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "consult", "message": "still alive",
	}, http.StatusCreated, &sent)
	return sent
}

func doDelete(t *testing.T, handler http.Handler, authorization, path string, wantStatus int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
}

// --- S-173: consult timeout/SLA ---

func TestS173ConsultDeadlineStamped(t *testing.T) {
	f := newS174Fixture(t)
	before := time.Now().UTC()

	var consult domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "consult", "message": "answer me",
	}, http.StatusCreated, &consult)
	require.False(t, consult.TimeoutAt.Before(before.Add(14*time.Minute)), "default deadline ~15m out")
	require.True(t, consult.TimeoutAt.Before(before.Add(16*time.Minute)))

	// Per-send override.
	var tuned domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "consult", "message": "quick answer", "consult_timeout_seconds": 60,
	}, http.StatusCreated, &tuned)
	require.True(t, tuned.TimeoutAt.Before(before.Add(2*time.Minute)))

	// Non-consult types carry no deadline.
	var ping domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "ping", "message": "awake?",
	}, http.StatusCreated, &ping)
	require.True(t, ping.TimeoutAt.IsZero(), "ping must not carry a consult deadline")

	var delegate domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "delegate", "message": "do the thing", "title": "the thing",
	}, http.StatusCreated, &delegate)
	require.True(t, delegate.TimeoutAt.IsZero(), "delegate is the task loop, not a consult")
}

func TestS173SweeperNotifiesAskerExactlyOnce(t *testing.T) {
	f := newS174Fixture(t)
	var consult domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "consult", "message": "unanswered question",
	}, http.StatusCreated, &consult)

	// Not due yet.
	posted, err := f.store.SweepConsultTimeouts(context.Background(), time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, 0, posted, "no notification before the deadline")

	// Past the deadline: the asker receives a synthetic reply on the
	// consult's own thread (correlation defaults to the consult id).
	posted, err = f.store.SweepConsultTimeouts(context.Background(), time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, posted)

	snap := f.inbox(t, authOwner, f.senderID)
	require.Equal(t, 1, snap.PendingCount)
	notice := snap.Pending[0]
	require.Equal(t, domain.MessageReply, notice.Type)
	require.Equal(t, consult.ID, notice.CorrelationID)
	require.Equal(t, "consult_timeout", notice.TerminalReason)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(notice.Payload, &payload))
	require.Equal(t, true, payload["consult_timeout"])
	require.Equal(t, true, payload["system_generated"])
	require.Equal(t, consult.ID, payload["timed_out_consult_id"])

	// Exactly once: a second sweep posts nothing new.
	posted, err = f.store.SweepConsultTimeouts(context.Background(), time.Now().UTC().Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, posted, "a consult must never time out twice")
}

func TestS173SweeperSkipsAnsweredAndDeletedAskers(t *testing.T) {
	f := newS174Fixture(t)

	// Answered consult: no timeout notice.
	var consult domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "consult", "message": "answered question",
	}, http.StatusCreated, &consult)
	var answer domain.Message
	doAgentJSON(t, f.handler, f.workerID, f.workerCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.senderID, "type": "reply", "message": "here you go", "correlation_id": consult.ID,
	}, http.StatusCreated, &answer)

	posted, err := f.store.SweepConsultTimeouts(context.Background(), time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, posted, "an answered consult never times out")

	// Deleted asker: the consult is stamped (never re-scanned) but posts
	// nothing — there is nobody left to notify.
	var orphan domain.Message
	doAgentJSON(t, f.handler, f.senderID, f.senderCred, http.MethodPost, pathMyMessages, map[string]any{
		"to_agent_id": f.workerID, "type": "consult", "message": "orphan question",
	}, http.StatusCreated, &orphan)
	require.NoError(t, f.store.DeleteAgent(context.Background(), f.senderID))

	posted, err = f.store.SweepConsultTimeouts(context.Background(), time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, posted, "nothing is deliverable to a deleted asker")

	// And the stamped consult stays stamped even across another sweep.
	posted, err = f.store.SweepConsultTimeouts(context.Background(), time.Now().UTC().Add(3*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, posted)
}
