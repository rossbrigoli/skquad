package httpapi

// S-269: gateway usage-reporting tests. The usage client posts to the
// CP's internal resource-usage ingest; the pipeline fires a report for
// every EXECUTED (governed-allowed) resource call and never for
// denials. Reporting is best-effort: a failing CP never breaks the
// governed path.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/tool-gateway/internal/auth"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/boundary"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/drivers"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/policy"
	"github.com/rossbrigoli/skquad/tool-gateway/internal/usage"
)

// recordingReporter captures usage reports for assertions.
type recordingReporter struct {
	mu     sync.Mutex
	reports []usage.Report
}

func (r *recordingReporter) Report(_ context.Context, rep usage.Report) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, rep)
	return nil
}

func (r *recordingReporter) wait(t *testing.T, want int) []usage.Report {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.reports)
		r.mu.Unlock()
		if n >= want {
			r.mu.Lock()
			defer r.mu.Unlock()
			out := append([]usage.Report(nil), r.reports...)
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d usage report(s)", want)
	return nil
}

func (r *recordingReporter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reports)
}

// stubWebDriver is a minimal `web` driver for pipeline tests.
type stubWebDriver struct{}

func (stubWebDriver) Name() string { return "web" }
func (stubWebDriver) Handle(context.Context, *drivers.Request) (*drivers.Response, error) {
	return &drivers.Response{StatusCode: 200, Body: map[string]any{"ok": true}}, nil
}

func newUsageTestServer(t *testing.T, rep usage.Reporter) (*Server, *recordingReporter) {
	t.Helper()
	st := newCPStub(t)
	st.set("agent-1", auth.HashCredential(testToken))
	pc := policy.NewClient(st.srv.URL, 30*time.Second, 2*time.Second)
	rec, ok := rep.(*recordingReporter)
	if !ok {
		t.Fatal("expected *recordingReporter")
	}
	srv := New(Deps{
		Policy:   pc,
		Boundary: boundary.StaticVerifier{},
		Enabled:  enabledTrue(),
		Drivers:  map[string]drivers.Driver{"web": stubWebDriver{}},
		Usage:    rep,
	})
	return srv, rec
}

func enabledTrue() *atomic.Bool {
	b := &atomic.Bool{}
	b.Store(true)
	return b
}

func TestUsageReportFiresOnAllowedWebCall(t *testing.T) {
	srv, rec := newUsageTestServer(t, &recordingReporter{})
	// The cpStub snapshot has no grants → denial path.
	rec2 := doWebFetch(t, srv, "agent-1", testToken, `{"url":"https://x.test"}`)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no grant)", rec2.Code)
	}
	if rec.count() != 0 {
		t.Fatalf("denied call must not report usage, got %d", rec.count())
	}
}

func TestUsageReportFieldsOnExecutedCall(t *testing.T) {
	// CP stub that serves a snapshot WITH a web grant.
	st := newCPStub(t)
	st.mu.Lock()
	st.snapshots["agent-1"] = policy.Snapshot{
		AgentID:        "agent-1",
		CredentialHash: auth.HashCredential(testToken),
		Grants: []policy.Grant{{
			ResourceID:   "web-res-1",
			ResourceType: "web",
		}},
	}
	st.mu.Unlock()

	rec := &recordingReporter{}
	pc := policy.NewClient(st.srv.URL, 30*time.Second, 2*time.Second)
	srv := New(Deps{
		Policy:   pc,
		Boundary: boundary.StaticVerifier{},
		Enabled:  enabledTrue(),
		Drivers:  map[string]drivers.Driver{"web": stubWebDriver{}},
		Usage:    rec,
	})

	rec2 := doWebFetch(t, srv, "agent-1", testToken, `{"url":"https://x.test"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
	}
	reports := rec.wait(t, 1)
	if reports[0].ResourceID != "web-res-1" || reports[0].ResourceType != "web" || reports[0].AgentID != "agent-1" {
		t.Fatalf("unexpected report: %+v", reports[0])
	}
	if reports[0].Count != 1 {
		t.Fatalf("count = %d, want 1", reports[0].Count)
	}
	if reports[0].Timestamp.IsZero() {
		t.Fatal("report timestamp must be set")
	}
}

func TestUsageClientPostsToCP(t *testing.T) {
	var got usage.Report
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/resource-usage" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer cp.Close()

	c := usage.NewClient(cp.URL, 2*time.Second)
	ts := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	if err := c.Report(context.Background(), usage.Report{
		Timestamp: ts, AgentID: "a1", ResourceID: "r1", ResourceType: "rest",
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if got.AgentID != "a1" || got.ResourceID != "r1" || got.ResourceType != "rest" || got.Count != 1 {
		t.Fatalf("CP received %+v", got)
	}

	// Validation and error paths.
	if err := c.Report(context.Background(), usage.Report{AgentID: "a1"}); err == nil {
		t.Fatal("missing resource_id must error")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if err := usage.NewClient(bad.URL, 2*time.Second).Report(context.Background(),
		usage.Report{AgentID: "a1", ResourceID: "r1"}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("non-2xx must error, got %v", err)
	}
}

// failingReporter always errors — the governed call must still succeed.
type failingReporter struct{}

func (failingReporter) Report(context.Context, usage.Report) error {
	return context.DeadlineExceeded
}

func TestUsageReportFailureDoesNotBreakCall(t *testing.T) {
	st := newCPStub(t)
	st.mu.Lock()
	st.snapshots["agent-1"] = policy.Snapshot{
		AgentID:        "agent-1",
		CredentialHash: auth.HashCredential(testToken),
		Grants:         []policy.Grant{{ResourceID: "web-res-1", ResourceType: "web"}},
	}
	st.mu.Unlock()
	pc := policy.NewClient(st.srv.URL, 30*time.Second, 2*time.Second)
	srv := New(Deps{
		Policy:   pc,
		Boundary: boundary.StaticVerifier{},
		Enabled:  enabledTrue(),
		Drivers:  map[string]drivers.Driver{"web": stubWebDriver{}},
		Usage:    failingReporter{},
	})
	rec := doWebFetch(t, srv, "agent-1", testToken, `{"url":"https://x.test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite failing usage sink", rec.Code)
	}
}
