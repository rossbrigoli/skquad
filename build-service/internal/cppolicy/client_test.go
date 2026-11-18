package cppolicy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 30*time.Second, 2*time.Second)
}

func snapshotBody(agentID string, grants []Grant) map[string]any {
	return map[string]any{"agent_id": agentID, "grants": grants}
}

func TestGitReposAllowCeiling(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/policy" || r.URL.Query().Get("agent") != "a1" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(snapshotBody("a1", []Grant{{
			ResourceID:   "git1",
			ResourceType: "git",
			Ceiling:      json.RawMessage(`{"repos_allow":["github.com/rossbrigoli/*"]}`),
		}}))
	})
	globs, err := c.GitReposAllow(context.Background(), "a1")
	if err != nil || len(globs) != 1 || globs[0] != "github.com/rossbrigoli/*" {
		t.Fatalf("got %v, %v", globs, err)
	}
}

func TestConstraintsOverrideCeiling(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snapshotBody("a1", []Grant{{
			ResourceID:   "git1",
			ResourceType: "git",
			Ceiling:      json.RawMessage(`{"repos_allow":["github.com/rossbrigoli/*"]}`),
			Constraints:  json.RawMessage(`{"repos_allow":["github.com/rossbrigoli/skquad"]}`),
		}}))
	})
	globs, err := c.GitReposAllow(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(globs) != 1 || globs[0] != "github.com/rossbrigoli/skquad" {
		t.Fatalf("constraints must narrow: %v", globs)
	}
}

func TestNoGitGrantsDeny(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snapshotBody("a1", []Grant{{ResourceID: "rest1", ResourceType: "rest"}}))
	})
	globs, err := c.GitReposAllow(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(globs) != 0 {
		t.Fatalf("no git grants must yield empty allow: %v", globs)
	}
}

func TestFailClosedOnServerError(t *testing.T) {
	var hits int32
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := c.GitReposAllow(context.Background(), "a1")
	if !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("server error must fail closed, got %v", err)
	}
}

func TestAgentIDSpoofFailsClosed(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snapshotBody("someone-else", []Grant{{
			ResourceType: "git", Ceiling: json.RawMessage(`{"repos_allow":["github.com/rossbrigoli/*"]}`),
		}}))
	})
	_, err := c.GitReposAllow(context.Background(), "a1")
	if !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("mismatched agent_id must fail closed, got %v", err)
	}
}

func TestTTLCachesFetches(t *testing.T) {
	var hits int32
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(snapshotBody("a1", []Grant{{
			ResourceType: "git", Ceiling: json.RawMessage(`{"repos_allow":["github.com/x/*"]}`),
		}}))
	})
	for i := 0; i < 3; i++ {
		if _, err := c.GitReposAllow(context.Background(), "a1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("TTL cache should collapse 3 lookups to 1 fetch, got %d", got)
	}
}

func TestHostQualificationOfHostlessGlobs(t *testing.T) {
	// Live CP convention: git grants carry host-less repos_allow
	// ("rossbrigoli/*") because the git resource is host-scoped via
	// config.base_url. GitReposAllow must qualify them so they match
	// the canonical "host/org/repo" form used by govern.
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snapshotBody("a1", []Grant{{
			ResourceID:   "git1",
			ResourceType: "git",
			Config:       json.RawMessage(`{"base_url":"https://github.com"}`),
			Constraints:  json.RawMessage(`{"allow_push":true,"repos_allow":["rossbrigoli/*","rossbrigoli/k3s-cluster"]}`),
		}}))
	})
	globs, err := c.GitReposAllow(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"github.com/rossbrigoli/*", "github.com/rossbrigoli/k3s-cluster"}
	if len(globs) != len(want) {
		t.Fatalf("got %v want %v", globs, want)
	}
	for i := range want {
		if globs[i] != want[i] {
			t.Fatalf("glob[%d]=%q want %q", i, globs[i], want[i])
		}
	}
}

func TestHostQualificationIdempotentAndNoHost(t *testing.T) {
	// Already-qualified globs must not be double-prefixed; a grant with
	// no usable config host leaves globs untouched (default-deny kept
	// against canonical host-prefixed candidates).
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snapshotBody("a1", []Grant{
			{
				ResourceID:   "git1",
				ResourceType: "git",
				Config:       json.RawMessage(`{"base_url":"https://github.com"}`),
				Constraints:  json.RawMessage(`{"repos_allow":["github.com/rossbrigoli/*"]}`),
			},
			{
				ResourceID:   "git2",
				ResourceType: "git",
				Constraints:  json.RawMessage(`{"repos_allow":["rossbrigoli/other"]}`),
			},
		}))
	})
	globs, err := c.GitReposAllow(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(globs) != 2 || globs[0] != "github.com/rossbrigoli/*" || globs[1] != "rossbrigoli/other" {
		t.Fatalf("got %v", globs)
	}
}
