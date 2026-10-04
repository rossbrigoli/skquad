package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

// S-189 coverage: provider API-key Secret store (S-155).

func TestProviderSecretNameSanitizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain uuid", "6f1e2a3b-4c5d-6e7f-8a9b-0c1d2e3f4a5b", "skquad-provider-key-6f1e2a3b-4c5d-6e7f-8a9b-0c1d2e3f4a5b"},
		{"uppercase lowered", "ABC-123", "skquad-provider-key-abc-123"},
		{"collapses double dashes", "a--b", "skquad-provider-key-a-b"},
		{"strips illegal chars", "ab!cd@#$ef", "skquad-provider-key-abcdef"},
		{"trims surrounding spaces and trailing dash", "  abc-  ", "skquad-provider-key-abc"},
		{"empty id yields bare prefix", "", "skquad-provider-key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderSecretName(tc.input); got != tc.want {
				t.Fatalf("ProviderSecretName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}

	// Long ids are truncated to the DNS-1123 limit.
	long := ProviderSecretName(strings.Repeat("a", 400))
	if len(long) > 253 {
		t.Fatalf("long name length = %d, want <= 253", len(long))
	}
}

func TestIsManagedRef(t *testing.T) {
	t.Parallel()

	if !IsManagedRef("k8s://skquad-system/skquad-provider-key-x") {
		t.Fatal("k8s:// ref must be managed")
	}
	if IsManagedRef("env://LITELLM_KEY") {
		t.Fatal("non-k8s ref must not be managed")
	}
	if IsManagedRef("") {
		t.Fatal("empty ref must not be managed")
	}
}

func newTestSecretStore(t *testing.T, h http.HandlerFunc) (*SecretStore, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	s := &SecretStore{
		baseURL:   server.URL,
		namespace: testNamespace,
		token:     testToken,
		client:    server.Client(),
	}
	return s, server
}

func TestSecretStoreRefFor(t *testing.T) {
	t.Parallel()

	s := &SecretStore{namespace: "skquad-system"}
	if got := s.RefFor("skquad-provider-key-abc"); got != "k8s://skquad-system/skquad-provider-key-abc" {
		t.Fatalf("RefFor = %q", got)
	}
}

func TestSecretStoreEnsureCreatesWhenMissing(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	s, _ := newTestSecretStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","code":404}`))
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	})

	err := s.EnsureProviderKey(context.Background(), "skquad-provider-key-abc", "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/v1/namespaces/skquad-system/secrets" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBody["kind"] != "Secret" || gotBody["type"] != "Opaque" {
		t.Fatalf("body kind/type = %v/%v", gotBody["kind"], gotBody["type"])
	}
	sd, _ := gotBody["stringData"].(map[string]any)
	if sd[ProviderSecretKey] != "sk-secret" {
		t.Fatalf("stringData = %v", sd)
	}
	labels, _ := gotBody["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels[managedByLabel] != controlPlaneName {
		t.Fatalf("managed-by label = %v", labels)
	}
}

func TestSecretStoreEnsureUpdatesWithResourceVersion(t *testing.T) {
	t.Parallel()

	var gotMethod string
	var gotBody map[string]any
	s, _ := newTestSecretStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"kind":     "Secret",
				"metadata": map[string]any{"name": "skquad-provider-key-abc", "resourceVersion": "4242"},
			})
			return
		}
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	})

	if err := s.EnsureProviderKey(context.Background(), "skquad-provider-key-abc", "sk-new"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %s, want PUT", gotMethod)
	}
	meta, _ := gotBody["metadata"].(map[string]any)
	if meta["resourceVersion"] != "4242" {
		t.Fatalf("resourceVersion must be carried on update: %v", meta)
	}
}

func TestSecretStoreEnsurePropagatesGetError(t *testing.T) {
	t.Parallel()

	s, _ := newTestSecretStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if err := s.EnsureProviderKey(context.Background(), "x", "k"); err == nil {
		t.Fatal("500 on get must error")
	}
}

func TestSecretStoreEnsureSendFailure(t *testing.T) {
	t.Parallel()

	s, _ := newTestSecretStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","code":404}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("nope"))
	})
	err := s.EnsureProviderKey(context.Background(), "x", "k")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want HTTP 403", err)
	}
}

func TestSecretStoreGetProviderKey(t *testing.T) {
	t.Parallel()

	t.Run("found", func(t *testing.T) {
		s, _ := newTestSecretStore(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]string{ProviderSecretKey: base64.StdEncoding.EncodeToString([]byte("sk-live"))},
			})
		})
		key, err := s.GetProviderKey(context.Background(), "x")
		if err != nil || key != "sk-live" {
			t.Fatalf("key=%q err=%v", key, err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		s, _ := newTestSecretStore(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","code":404}`))
		})
		if _, err := s.GetProviderKey(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("undecodable data", func(t *testing.T) {
		s, _ := newTestSecretStore(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]string{ProviderSecretKey: "!!!not-base64!!!"},
			})
		})
		if _, err := s.GetProviderKey(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unparsable body", func(t *testing.T) {
		s, _ := newTestSecretStore(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{{{invalid"))
		})
		if _, err := s.GetProviderKey(context.Background(), "x"); err == nil {
			t.Fatal("bad JSON must error")
		}
	})
}

func TestSecretStoreDeleteProviderKey(t *testing.T) {
	t.Parallel()

	t.Run("ok", func(t *testing.T) {
		var gotPath string
		s, _ := newTestSecretStore(t, func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.RequestURI()
			w.WriteHeader(http.StatusOK)
		})
		if err := s.DeleteProviderKey(context.Background(), "k1"); err != nil {
			t.Fatal(err)
		}
		if gotPath != "/api/v1/namespaces/skquad-system/secrets/k1" {
			t.Fatalf("path = %q", gotPath)
		}
	})

	t.Run("missing is not an error", func(t *testing.T) {
		s, _ := newTestSecretStore(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		if err := s.DeleteProviderKey(context.Background(), "gone"); err != nil {
			t.Fatalf("404 delete must be a no-op: %v", err)
		}
	})

	t.Run("server error propagates", func(t *testing.T) {
		s, _ := newTestSecretStore(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		if err := s.DeleteProviderKey(context.Background(), "x"); err == nil {
			t.Fatal("503 must error")
		}
	})
}

func TestNewSecretStoreReadsTokenAndConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte("  projected-token \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		K8sAPIBase:   "https://api.test/",
		K8sNamespace: "skquad-system",
		K8sTokenFile: tokFile,
		K8sInsecure:  true,
	}
	s, err := NewSecretStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.token != "projected-token" {
		t.Fatalf("token = %q (must be trimmed)", s.token)
	}
	if s.baseURL != "https://api.test" {
		t.Fatalf("baseURL trailing slash must be trimmed: %q", s.baseURL)
	}
	if s.RefFor("n") != "k8s://skquad-system/n" {
		t.Fatalf("RefFor = %q", s.RefFor("n"))
	}

	if _, err := NewSecretStore(&config.Config{K8sTokenFile: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("missing token file must error")
	}
}
