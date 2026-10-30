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
