// TG-8 slice C2 tests: gateway confirmation-gate enforcement against a
// fake CP implementing POST /internal/v1/confirmation/check and
// /consume, plus a counting driver proving whether execution happened.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/audit"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/boundary"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/confirmation"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
)

// ── fake CP ──────────────────────────────────────────────────────────────

type fakeConfRow struct {
	ID       string
	ArgsHash string
	State    string // pending | approved_once | approved_standing | denied
	Reason   string
	Consumed bool
}

type fakeConfCP struct {
	mu       sync.Mutex
	snapshot policy.Snapshot
	checks   int
	consumes int
	rows     map[string]*fakeConfRow
	seq      int
	// standingOnCheck: when non-empty, check answers auto with this
	// standing grant id (mirrors a live standing_grants match).
	standingOnCheck string
	// standingLive controls consume-time standing revalidation.
	standingLive bool
	down         bool
}

func newFakeConfCP(snap policy.Snapshot) *fakeConfCP {
	return &fakeConfCP{snapshot: snap, rows: map[string]*fakeConfRow{}, standingLive: true}
}

func (cp *fakeConfCP) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		if cp.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/internal/v1/policy":
			_ = json.NewEncoder(w).Encode(cp.snapshot)
		case "/internal/v1/confirmation/check":
			cp.checks++
			var req struct {
				ResourceID string `json:"resource_id"`
				AgentID    string `json:"agent_id"`
				Tool       string `json:"tool"`
				ArgsHash   string `json:"args_hash"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if cp.standingOnCheck != "" {
				_ = json.NewEncoder(w).Encode(map[string]string{
					"mode": "auto", "matched_standing_grant_id": cp.standingOnCheck,
				})
				return
			}
			cp.seq++
			id := fmt.Sprintf("conf-%d", cp.seq)
			cp.rows[id] = &fakeConfRow{ID: id, ArgsHash: req.ArgsHash, State: "pending"}
			_ = json.NewEncoder(w).Encode(map[string]string{"mode": "pending", "confirmation_id": id})
		case "/internal/v1/confirmation/consume":
			cp.consumes++
			var req struct {
				ID       string `json:"id"`
				ArgsHash string `json:"args_hash"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			row, ok := cp.rows[req.ID]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if row.ArgsHash != req.ArgsHash {
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": false, "reason": "args_hash_mismatch"})
				return
			}
			switch row.State {
			case "approved_once":
				if row.Consumed {
					_ = json.NewEncoder(w).Encode(map[string]any{"allowed": false, "reason": "denied_replayed"})
					return
				}
				row.Consumed = true
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "mode": "once"})
			case "approved_standing":
				if !cp.standingLive {
					_ = json.NewEncoder(w).Encode(map[string]any{"allowed": false, "reason": "standing_grant_not_live"})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"allowed": true, "mode": "standing", "matched_standing_grant_id": "sg-" + row.ID,
				})
			case "denied":
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": false, "reason": "denied_by_owner: " + row.Reason})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": false, "reason": "confirmation_pending"})
			}
		default:
			http.NotFound(w, r)
		}
	})
}

func (cp *fakeConfCP) decide(t *testing.T, n int, state, reason string) string {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	id := fmt.Sprintf("conf-%d", n)
	row, ok := cp.rows[id]
	if !ok {
		t.Fatalf("confirmation %s not found", id)
	}
	row.State = state
	row.Reason = reason
	return id
}

func (cp *fakeConfCP) counts() (checks, consumes int) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.checks, cp.consumes
}

// ── harness ──────────────────────────────────────────────────────────────

// gatedDriver records call count and whether the dispatch context carried the confirmation-satisfied marker.
type gatedDriver struct {
	calls     atomic.Int32
	satisfied atomic.Bool
}

func (d *gatedDriver) Name() string { return "gated" }
func (d *gatedDriver) Handle(ctx context.Context, _ *drivers.Request) (*drivers.Response, error) {
	d.calls.Add(1)
	d.satisfied.Store(drivers.ConfirmationSatisfied(ctx))
	return &drivers.Response{StatusCode: 200, Body: map[string]any{"ok": true}}, nil
}

const confTestToken = "conf-agent-token"

func newConfServer(t *testing.T, ceiling string, resourceType string) (*Server, *fakeConfCP, *gatedDriver, *bytes.Buffer) {
	t.Helper()
	cp := newFakeConfCP(policy.Snapshot{
		AgentID:        "agent-1",
		CredentialHash: auth.HashCredential(confTestToken),
		Grants: []policy.Grant{{
			ResourceID:   "res-1",
			ResourceType: resourceType,
			Ceiling:      json.RawMessage(ceiling),
		}},
	})
	srv := httptest.NewServer(cp.handler())
	t.Cleanup(srv.Close)
	pc := policy.NewClient(srv.URL, time.Hour, 2*time.Second)
	drv := &gatedDriver{}
	auditBuf := &bytes.Buffer{}
	gw := New(Deps{
		Policy:       pc,
		Boundary:     boundary.StaticVerifier{},
		Audit:        audit.NewStdoutEmitter(auditBuf),
		Confirmation: confirmation.NewClient(srv.URL, 2*time.Second),
		Drivers: map[string]drivers.Driver{
			"rest": drv, "mcp": drv, "web": drv,
		},
	})
	return gw, cp, drv, auditBuf
}

func doRest(t *testing.T, h http.Handler, body, confID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/rest/res-1", strings.NewReader(body))
	req.Header.Set("X-Skquad-Agent-ID", "agent-1")
	req.Header.Set("Authorization", "Bearer "+confTestToken)
	if confID != "" {
		req.Header.Set(ConfirmationIDHeader, confID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response not JSON error envelope: %s (%v)", rec.Body.String(), err)
	}
	return m
}

// auditHas asserts one audit event's gate detail JSON contains all want
// key/value pairs (detail is parsed, not substring-matched, because the
// detail is escaped inside the event JSON).
func auditHas(t *testing.T, buf *bytes.Buffer, want map[string]string) {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var ev audit.Event
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Detail == "" {
			continue
		}
		var d map[string]string
		if json.Unmarshal([]byte(ev.Detail), &d) != nil {
			continue
		}
		all := true
		for k, v := range want {
			if d[k] != v {
				all = false
				break
			}
		}
		if all {
			return
		}
	}
	t.Fatalf("no audit gate detail containing %v in:\n%s", want, buf.String())
}

// ── tests ────────────────────────────────────────────────────────────────

func TestGateAutoExecutesWithStandingID(t *testing.T) {
	gw, cp, drv, auditBuf := newConfServer(t, `{"require_confirmation": true}`, "rest")
	cp.mu.Lock()
	cp.standingOnCheck = "sg-42"
	cp.mu.Unlock()

	rec := doRest(t, gw, `{"a":1}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if drv.calls.Load() != 1 {
		t.Fatalf("driver calls = %d, want 1", drv.calls.Load())
	}
	if !drv.satisfied.Load() {
		t.Fatal("dispatch ctx missing confirmation-satisfied marker")
	}
	auditHas(t, auditBuf, map[string]string{"gate": "auto", "standing_grant_id": "sg-42"})
}

func TestGatePendingDoesNotExecute(t *testing.T) {
	gw, cp, drv, auditBuf := newConfServer(t, `{"require_confirmation": true}`, "rest")

	rec := doRest(t, gw, `{"a":1}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	m := decodeErr(t, rec)
	if m["error"] != "pending_confirmation" {
		t.Fatalf("error = %q, want pending_confirmation", m["error"])
	}
	if m["confirmation_id"] != "conf-1" {
		t.Fatalf("confirmation_id = %q, want conf-1", m["confirmation_id"])
	}
	if !strings.Contains(m["hint"], ConfirmationIDHeader) {
		t.Fatalf("hint must name the retry header, got %q", m["hint"])
	}
	if drv.calls.Load() != 0 {
		t.Fatal("driver must NOT run on pending")
	}
	auditHas(t, auditBuf, map[string]string{"gate": "pending", "confirmation_id": "conf-1"})
	if checks, _ := cp.counts(); checks != 1 {
		t.Fatalf("checks = %d, want 1", checks)
	}
}

func TestGateRetryOnceExecutesThenReplayDenied(t *testing.T) {
	gw, cp, drv, auditBuf := newConfServer(t, `{"require_confirmation": true}`, "rest")

	doRest(t, gw, `{"a":1}`, "") // pending → conf-1
	id := cp.decide(t, 1, "approved_once", "")

	rec := doRest(t, gw, `{"a":1}`, id)
	if rec.Code != http.StatusOK || drv.calls.Load() != 1 {
		t.Fatalf("approved retry: code %d calls %d, want 200/1: %s", rec.Code, drv.calls.Load(), rec.Body.String())
	}
	auditHas(t, auditBuf, map[string]string{"gate": "once"})

	// Replay: same id again → denied_replayed, driver stays at 1.
	rec = doRest(t, gw, `{"a":1}`, id)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("replay: want 403, got %d: %s", rec.Code, rec.Body.String())
	}
	m := decodeErr(t, rec)
	if m["error"] != "confirmation_denied" || m["reason"] != "denied_replayed" {
		t.Fatalf("replay envelope = %+v", m)
	}
	if drv.calls.Load() != 1 {
		t.Fatalf("replay must not execute; calls = %d", drv.calls.Load())
	}
	auditHas(t, auditBuf, map[string]string{"gate": "denied", "reason": "denied_replayed"})
}

func TestGateArgsMismatchOnRetry(t *testing.T) {
	gw, cp, drv, _ := newConfServer(t, `{"require_confirmation": true}`, "rest")

	doRest(t, gw, `{"a":1}`, "")
	id := cp.decide(t, 1, "approved_once", "")

	rec := doRest(t, gw, `{"a":2}`, id) // args changed after approval
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	m := decodeErr(t, rec)
	if m["reason"] != "args_hash_mismatch" || m["message"] != "args_hash_mismatch" {
		t.Fatalf("envelope = %+v, want args_hash_mismatch verbatim", m)
	}
	if drv.calls.Load() != 0 {
		t.Fatal("args mismatch must not execute")
	}
}

func TestGateStandingExecutesWithAudit(t *testing.T) {
	gw, cp, drv, auditBuf := newConfServer(t, `{"require_confirmation": true}`, "rest")

	doRest(t, gw, `{"a":1}`, "")
	id := cp.decide(t, 1, "approved_standing", "")

	rec := doRest(t, gw, `{"a":1}`, id)
	if rec.Code != http.StatusOK || drv.calls.Load() != 1 {
		t.Fatalf("standing retry: code %d calls %d: %s", rec.Code, drv.calls.Load(), rec.Body.String())
	}
	auditHas(t, auditBuf, map[string]string{"gate": "standing", "standing_grant_id": "sg-conf-1"})

	// Standing is multi-use: second call also executes.
	rec = doRest(t, gw, `{"a":1}`, id)
	if rec.Code != http.StatusOK || drv.calls.Load() != 2 {
		t.Fatalf("standing must be reusable: code %d calls %d", rec.Code, drv.calls.Load())
	}
}

func TestGateStandingNotLive(t *testing.T) {
	gw, cp, drv, _ := newConfServer(t, `{"require_confirmation": true}`, "rest")

	doRest(t, gw, `{"a":1}`, "")
	id := cp.decide(t, 1, "approved_standing", "")
	cp.mu.Lock()
	cp.standingLive = false
	cp.mu.Unlock()

	rec := doRest(t, gw, `{"a":1}`, id)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if m := decodeErr(t, rec); m["reason"] != "standing_grant_not_live" {
		t.Fatalf("reason = %q", m["reason"])
	}
	if drv.calls.Load() != 0 {
		t.Fatal("revoked standing must not execute")
	}
}

func TestGateDeniedByOwnerVerbatim(t *testing.T) {
	gw, cp, drv, _ := newConfServer(t, `{"require_confirmation": true}`, "rest")

	doRest(t, gw, `{"a":1}`, "")
	id := cp.decide(t, 1, "denied", "not today")

	rec := doRest(t, gw, `{"a":1}`, id)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	m := decodeErr(t, rec)
	if m["reason"] != "denied_by_owner" {
		t.Fatalf("reason = %q, want denied_by_owner", m["reason"])
	}
	if m["message"] != "denied_by_owner: not today" {
		t.Fatalf("message = %q, want CP reason verbatim", m["message"])
	}
	if drv.calls.Load() != 0 {
		t.Fatal("owner denial must not execute")
	}
}

func TestGateCPDownFailClosed(t *testing.T) {
	gw, cp, drv, auditBuf := newConfServer(t, `{"require_confirmation": true}`, "rest")
	// Warm the policy cache first so the failure under test is the
	// CONFIRMATION CP call, not policy resolution.
	doRest(t, gw, `{"a":1}`, "") // pending → conf-1
	cp.mu.Lock()
	cp.down = true
	cp.mu.Unlock()

	rec := doRest(t, gw, `{"a":2}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if m := decodeErr(t, rec); m["error"] != "confirmation_unavailable" {
		t.Fatalf("error = %q, want confirmation_unavailable", m["error"])
	}
	if drv.calls.Load() != 0 {
		t.Fatal("CP down must never execute the gated call")
	}
	auditHas(t, auditBuf, map[string]string{"gate": "unavailable"})

	// Retry-with-id path also fails closed while CP is down.
	rec = doRest(t, gw, `{"a":1}`, "conf-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("consume while CP down: want 503, got %d", rec.Code)
	}
}

func TestNonGatedGrantZeroConfirmationCalls(t *testing.T) {
	gw, cp, drv, _ := newConfServer(t, `{"max_bytes": 100}`, "rest")

	rec := doRest(t, gw, `{"a":1}`, "")
	if rec.Code != http.StatusOK || drv.calls.Load() != 1 {
		t.Fatalf("non-gated: code %d calls %d", rec.Code, drv.calls.Load())
	}
	checks, consumes := cp.counts()
	if checks != 0 || consumes != 0 {
		t.Fatalf("non-gated grant hit CP confirmation %d checks / %d consumes", checks, consumes)
	}
}

func TestGatePerToolShapeMCP(t *testing.T) {
	// Reuses the pre-existing MCP per-tool confirmation shape.
	gw, cp, drv, _ := newConfServer(t, `{"per_tool":{"deploy":{"requires_confirmation":true}}}`, "mcp")
	body := `{"tool":"deploy","arguments":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/res-1/call", strings.NewReader(body))
	req.Header.Set("X-Skquad-Agent-ID", "agent-1")
	req.Header.Set("Authorization", "Bearer "+confTestToken)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("per-tool gated MCP: want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if drv.calls.Load() != 0 {
		t.Fatal("per-tool gated MCP must not execute")
	}
	_ = cp
}

func TestGateMissingClientFailsClosed(t *testing.T) {
	cp := newFakeConfCP(policy.Snapshot{
		AgentID:        "agent-1",
		CredentialHash: auth.HashCredential(confTestToken),
		Grants: []policy.Grant{{ResourceID: "res-1", ResourceType: "rest",
			Ceiling: json.RawMessage(`{"require_confirmation": true}`)}},
	})
	srv := httptest.NewServer(cp.handler())
	t.Cleanup(srv.Close)
	drv := &gatedDriver{}
	gw := New(Deps{
		Policy:   policy.NewClient(srv.URL, time.Hour, 2*time.Second),
		Boundary: boundary.StaticVerifier{},
		Drivers:  map[string]drivers.Driver{"rest": drv},
		// Confirmation intentionally nil.
	})
	rec := doRest(t, gw, `{"a":1}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("gated grant without client: want 503, got %d", rec.Code)
	}
	if drv.calls.Load() != 0 {
		t.Fatal("gated grant without client must not execute")
	}
}
