package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// S-189 coverage: CRWriter delete paths (404-tolerant, error-propagating).

func newTestCRWriter(t *testing.T, h http.HandlerFunc) *CRWriter {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return &CRWriter{
		baseURL:      server.URL,
		namespace:    testNamespace,
		groupVersion: testAPIVersion,
		agentImage:   runtimeImageRef,
		token:        testToken,
		client:       server.Client(),
	}
}

func TestDeleteSquadTargetsCRPath(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAuth string
	w := newTestCRWriter(t, func(wr http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		wr.WriteHeader(http.StatusOK)
	})

	if err := w.DeleteSquad(context.Background(), &domain.Squad{ID: "sq-1"}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotPath != "/apis/skquad.io/v1/namespaces/skquad-system/squads/squad-sq-1" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("auth = %q", gotAuth)
	}
}

func TestDeleteAgentTargetsCRPath(t *testing.T) {
	t.Parallel()

	var gotPath string
	w := newTestCRWriter(t, func(wr http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		wr.WriteHeader(http.StatusOK)
	})

	if err := w.DeleteAgent(context.Background(), &domain.Agent{ID: "ag-9"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/apis/skquad.io/v1/namespaces/skquad-system/agents/agent-ag-9" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestDeleteTreats404AsNoOp(t *testing.T) {
	t.Parallel()

	w := newTestCRWriter(t, func(wr http.ResponseWriter, _ *http.Request) {
		wr.WriteHeader(http.StatusNotFound)
	})
	if err := w.DeleteSquad(context.Background(), &domain.Squad{ID: "gone"}); err != nil {
		t.Fatalf("404 delete must be tolerated: %v", err)
	}
	if err := w.DeleteAgent(context.Background(), &domain.Agent{ID: "gone"}); err != nil {
		t.Fatalf("404 delete must be tolerated: %v", err)
	}
}

func TestDeletePropagatesServerErrors(t *testing.T) {
	t.Parallel()

	w := newTestCRWriter(t, func(wr http.ResponseWriter, _ *http.Request) {
		wr.WriteHeader(http.StatusInternalServerError)
		_, _ = wr.Write([]byte("boom"))
	})
	err := w.DeleteSquad(context.Background(), &domain.Squad{ID: "sq-1"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want 500 propagated", err)
	}
}

func TestDeleteAgentCredential(t *testing.T) {
	t.Parallel()

	t.Run("valid ref deletes core secret", func(t *testing.T) {
		var gotPath string
		w := newTestCRWriter(t, func(wr http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.RequestURI()
			wr.WriteHeader(http.StatusOK)
		})
		err := w.DeleteAgentCredential(context.Background(), "k8s://squad-test/agent-agent-1-credential-abcd")
		if err != nil {
			t.Fatal(err)
		}
		if gotPath != "/api/v1/namespaces/squad-test/secrets/agent-agent-1-credential-abcd" {
			t.Fatalf("path = %q", gotPath)
		}
	})

	t.Run("invalid ref is a silent no-op", func(t *testing.T) {
		called := false
		w := newTestCRWriter(t, func(wr http.ResponseWriter, _ *http.Request) {
			called = true
			wr.WriteHeader(http.StatusOK)
		})
		if err := w.DeleteAgentCredential(context.Background(), "not-a-k8s-ref"); err != nil {
			t.Fatalf("invalid ref must not error: %v", err)
		}
		if called {
			t.Fatal("invalid ref must not hit the API")
		}
	})

	t.Run("server error propagates", func(t *testing.T) {
		w := newTestCRWriter(t, func(wr http.ResponseWriter, _ *http.Request) {
			wr.WriteHeader(http.StatusForbidden)
		})
		if err := w.DeleteAgentCredential(context.Background(), "k8s://ns/secret"); err == nil {
			t.Fatal("403 must error")
		}
	})
}

func TestDeleteCoreTreats404AsNoOp(t *testing.T) {
	t.Parallel()

	var gotPath string
	w := newTestCRWriter(t, func(wr http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		wr.WriteHeader(http.StatusNotFound)
	})
	if err := w.deleteCore(context.Background(), "secrets", "ns-x", "sec-y"); err != nil {
		t.Fatalf("404 on deleteCore must be tolerated: %v", err)
	}
	if gotPath != "/api/v1/namespaces/ns-x/secrets/sec-y" {
		t.Fatalf("path = %q", gotPath)
	}
}
