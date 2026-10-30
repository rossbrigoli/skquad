package policyclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func jsonHandler(t *testing.T, body string, hits *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.URL.Path != "/internal/v1/policy" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("agent") == "unknown" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("agent") == "boom" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
}

const snapCovered = `{
  "agent_id": "agent-1",
  "credential_hash": "x",
  "generation": 3,
  "grants": [
    {"resource_id": "res-git", "resource_type": "git",
     "ceiling": {"repos_allow": ["rossbrigoli/*"], "allow_push": true}}
  ]
}`

const snapConstraintsNarrower = `{
  "agent_id": "agent-1",
  "grants": [
    {"resource_id": "res-git", "resource_type": "git",
     "constraints": {"repos_allow": ["rossbrigoli/skquad"]},
     "ceiling": {"repos_allow": ["rossbrigoli/*"]}}
  ]
}`

const snapNoGit = `{
  "agent_id": "agent-1",
  "grants": [
    {"resource_id": "res-rest", "resource_type": "rest", "ceiling": {}}
  ]
}`

func TestRepoAllowed(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, snapCovered, nil))
	defer srv.Close()
	c := New(srv.URL, time.Minute, 2*time.Second)
	if err := c.RepoAllowed(context.Background(), "agent-1", "rossbrigoli/skquad"); err != nil {
		t.Fatalf("expected allowed: %v", err)
	}
	if err := c.RepoAllowed(context.Background(), "agent-1", "evilcorp/skquad"); err == nil {
		t.Fatal("expected denied for out-of-scope repo")
	}
}

func TestConstraintsPreferred(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, snapConstraintsNarrower, nil))
	defer srv.Close()
	c := New(srv.URL, time.Minute, 2*time.Second)
	if err := c.RepoAllowed(context.Background(), "agent-1", "rossbrigoli/skquad"); err != nil {
		t.Fatalf("expected allowed by constraints: %v", err)
	}
	// Within ceiling but outside constraints → deny (narrower wins).
	if err := c.RepoAllowed(context.Background(), "agent-1", "rossbrigoli/other"); err == nil {
		t.Fatal("expected deny: constraints narrower than ceiling")
	}
}

func TestNoGitGrantDenies(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, snapNoGit, nil))
	defer srv.Close()
	c := New(srv.URL, time.Minute, 2*time.Second)
	if err := c.RepoAllowed(context.Background(), "agent-1", "rossbrigoli/skquad"); err == nil {
		t.Fatal("expected deny with no git grant")
	}
}

func TestFailClosedOnErrors(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, "", nil))
	defer srv.Close()
	c := New(srv.URL, time.Minute, 2*time.Second)
	if err := c.RepoAllowed(context.Background(), "unknown", "rossbrigoli/skquad"); err == nil {
		t.Fatal("404 must fail closed")
	}
	if err := c.RepoAllowed(context.Background(), "boom", "rossbrigoli/skquad"); err == nil {
		t.Fatal("500 must fail closed")
	}
}

func TestAgentIDMismatchDenied(t *testing.T) {
	// CP returns a snapshot whose agent_id does not match the request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"agent_id":"someone-else","grants":[{"resource_id":"g","resource_type":"git","ceiling":{"repos_allow":["rossbrigoli/*"]}}]}`)
	}))
	defer srv.Close()
	c := New(srv.URL, time.Minute, 2*time.Second)
	if err := c.RepoAllowed(context.Background(), "agent-1", "rossbrigoli/skquad"); err == nil {
		t.Fatal("agent_id mismatch must deny")
	}
}

func TestCacheBoundsFetches(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(jsonHandler(t, snapCovered, &hits))
	defer srv.Close()
	c := New(srv.URL, time.Hour, 2*time.Second)
	for i := 0; i < 5; i++ {
		if err := c.RepoAllowed(context.Background(), "agent-1", "rossbrigoli/skquad"); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 CP fetch with TTL cache, got %d", hits.Load())
	}
}

func TestCacheExpiryRefetches(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(jsonHandler(t, snapCovered, &hits))
	defer srv.Close()
	c := New(srv.URL, 50*time.Millisecond, 2*time.Second)
	if err := c.RepoAllowed(context.Background(), "agent-1", "rossbrigoli/skquad"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := c.RepoAllowed(context.Background(), "agent-1", "rossbrigoli/skquad"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected refetch after TTL, got %d", hits.Load())
	}
}
