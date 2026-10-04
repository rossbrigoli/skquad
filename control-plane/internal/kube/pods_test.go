package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

// S-189 coverage: pod restart helper (S-162).

func newTestRestarter(t *testing.T, h http.HandlerFunc) *PodRestarter {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return &PodRestarter{
		baseURL:   server.URL,
		namespace: testNamespace,
		token:     testToken,
		client:    server.Client(),
	}
}

func TestRestartAgentPodsCountsDeleted(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAuth string
	p := newTestRestarter(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"PodList","items":[{"metadata":{"name":"agent-a-xyz"}},{"metadata":{"name":"agent-a-abc"}}]}`))
	})

	n, err := p.RestartAgentPods(context.Background(), "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted = %d, want 2", n)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("method = %s", gotMethod)
	}
	want := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s%%3D%s", testNamespace, LabelAgentID, "agent-a")
	if gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("auth = %q", gotAuth)
	}
}

func TestRestartAgentPodsEmptyAndErrors(t *testing.T) {
	t.Parallel()

	t.Run("no matching pods is zero not error", func(t *testing.T) {
		p := newTestRestarter(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		n, err := p.RestartAgentPods(context.Background(), "ghost")
		if err != nil || n != 0 {
			t.Fatalf("n=%d err=%v, want 0/<nil>", n, err)
		}
	})

	t.Run("empty item list", func(t *testing.T) {
		p := newTestRestarter(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"items":[]}`))
		})
		n, err := p.RestartAgentPods(context.Background(), "agent-b")
		if err != nil || n != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})

	t.Run("non-200 propagates", func(t *testing.T) {
		p := newTestRestarter(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		if _, err := p.RestartAgentPods(context.Background(), "agent-c"); err == nil {
			t.Fatal("401 must error")
		}
	})

	t.Run("unparsable response propagates", func(t *testing.T) {
		p := newTestRestarter(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		})
		if _, err := p.RestartAgentPods(context.Background(), "agent-d"); err == nil {
			t.Fatal("bad JSON must error")
		}
	})
}

func TestNewPodRestarterConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte(" pod-token\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewPodRestarter(&config.Config{
		K8sAPIBase:   "https://api.test/",
		K8sNamespace: "skquad-system",
		K8sTokenFile: tokFile,
		K8sInsecure:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.token != "pod-token" {
		t.Fatalf("token = %q (must be trimmed)", p.token)
	}
	if p.baseURL != "https://api.test" {
		t.Fatalf("baseURL = %q", p.baseURL)
	}

	if _, err := NewPodRestarter(&config.Config{K8sTokenFile: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("missing token file must error")
	}
}
