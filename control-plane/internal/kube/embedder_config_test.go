package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/stretchr/testify/require"
)

// S-212 (ADR-0013 §5): embedder runtime override ConfigMap writer.

func embedderConfigStore(t *testing.T, handler http.Handler) (*EmbedderConfigStore, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	tok := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tok, []byte("test-token\n"), 0o600))
	store, err := NewEmbedderConfigStore(&config.Config{
		K8sAPIBase:   server.URL,
		K8sTokenFile: tok,
		K8sNamespace: "skquad-system",
	})
	require.NoError(t, err)
	return store, server
}

func TestEmbedderConfigSetCreatesWhenAbsent(t *testing.T) {
	var created map[string]any
	var gotPath, gotAuth, gotCT string
	store, _ := embedderConfigStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &created))
		w.WriteHeader(http.StatusCreated)
	}))

	require.NoError(t, store.SetEmbedderRuntime(context.Background(), "cuda"))
	require.Equal(t, "/api/v1/namespaces/skquad-system/configmaps", gotPath)
	require.Equal(t, "Bearer test-token", gotAuth)
	require.Equal(t, "application/json", gotCT)
	meta, _ := created["metadata"].(map[string]any)
	require.Equal(t, EmbedderConfigMapName, meta["name"])
	require.Equal(t, "skquad-system", meta["namespace"])
	data, _ := created["data"].(map[string]any)
	require.Equal(t, "cuda", data[EmbedderRuntimeKey])
}

func TestEmbedderConfigSetPatchesWhenPresent(t *testing.T) {
	var patchBody map[string]any
	var gotPath, gotMethod, gotCT string
	store, _ := embedderConfigStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": EmbedderConfigMapName, "resourceVersion": "42"},
				"data":       map[string]string{EmbedderRuntimeKey: "auto"},
			})
		case http.MethodPatch:
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotCT = r.Header.Get("Content-Type")
			body, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(body, &patchBody))
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))

	require.NoError(t, store.SetEmbedderRuntime(context.Background(), "vulkan"))
	require.Equal(t, http.MethodPatch, gotMethod)
	require.Equal(t, "/api/v1/namespaces/skquad-system/configmaps/"+EmbedderConfigMapName, gotPath)
	require.Equal(t, "application/merge-patch+json", gotCT)
	data, _ := patchBody["data"].(map[string]any)
	require.Equal(t, "vulkan", data[EmbedderRuntimeKey])
}

func TestEmbedderConfigSetErrors(t *testing.T) {
	store, _ := embedderConfigStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	require.ErrorContains(t, store.SetEmbedderRuntime(context.Background(), "cpu"), "HTTP 500")
	require.ErrorContains(t, store.SetEmbedderRuntime(context.Background(), "  "), "empty")
}

func TestEmbedderConfigPatchFailureSurfaces(t *testing.T) {
	store, _ := embedderConfigStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": EmbedderConfigMapName}})
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	require.ErrorContains(t, store.SetEmbedderRuntime(context.Background(), "cuda"), "HTTP 403")
}
