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
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/stretchr/testify/require"
)

// S-287: agent resources override ConfigMap writer.

func agentResourcesConfigStore(t *testing.T, handler http.Handler) *AgentResourcesConfigStore {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	tok := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tok, []byte("test-token\n"), 0o600))
	store, err := NewAgentResourcesConfigStore(&config.Config{
		K8sAPIBase:   server.URL,
		K8sTokenFile: tok,
		K8sNamespace: "skquad-system",
	})
	require.NoError(t, err)
	return store
}

func fullSettings() domain.AgentResourceSettings {
	return domain.AgentResourceSettings{
		CPURequest:    "150m",
		CPULimit:      "750m",
		MemoryRequest: "192Mi",
		MemoryLimit:   "768Mi",
	}
}

func TestAgentResourcesConfigCreatesWhenAbsent(t *testing.T) {
	var created map[string]any
	var gotPath, gotAuth, gotCT string
	store := agentResourcesConfigStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	require.NoError(t, store.SetAgentResources(context.Background(), fullSettings()))
	require.Equal(t, "/api/v1/namespaces/skquad-system/configmaps", gotPath)
	require.Equal(t, "Bearer test-token", gotAuth)
	require.Equal(t, "application/json", gotCT)
	meta, _ := created["metadata"].(map[string]any)
	require.Equal(t, AgentResourcesConfigMapName, meta["name"])
	require.Equal(t, "skquad-system", meta["namespace"])
	data, _ := created["data"].(map[string]any)
	require.Equal(t, "150m", data[AgentResourcesKeyCPURequest])
	require.Equal(t, "750m", data[AgentResourcesKeyCPULimit])
	require.Equal(t, "192Mi", data[AgentResourcesKeyMemoryRequest])
	require.Equal(t, "768Mi", data[AgentResourcesKeyMemoryLimit])
}

func TestAgentResourcesConfigPatchesWhenPresent(t *testing.T) {
	var patchBody map[string]any
	var gotMethod, gotCT string
	store := agentResourcesConfigStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": AgentResourcesConfigMapName, "resourceVersion": "7"},
				"data":     map[string]string{AgentResourcesKeyCPURequest: "100m"},
			})
		default:
			gotMethod = r.Method
			gotCT = r.Header.Get("Content-Type")
			body, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(body, &patchBody))
			w.WriteHeader(http.StatusOK)
		}
	}))

	require.NoError(t, store.SetAgentResources(context.Background(), fullSettings()))
	require.Equal(t, http.MethodPatch, gotMethod)
	require.Equal(t, "application/merge-patch+json", gotCT)
	data, _ := patchBody["data"].(map[string]any)
	require.Len(t, data, 4)
	require.Equal(t, "768Mi", data[AgentResourcesKeyMemoryLimit])
}

func TestAgentResourcesConfigRejectsEmptyValues(t *testing.T) {
	store := agentResourcesConfigStore(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("no HTTP call expected for empty values")
	}))
	err := store.SetAgentResources(context.Background(), domain.AgentResourceSettings{
		CPURequest: "100m", CPULimit: "500m", MemoryRequest: "", MemoryLimit: "512Mi",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "memory_request is empty")
}

func TestAgentResourcesConfigGetErrorPropagates(t *testing.T) {
	store := agentResourcesConfigStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		t.Fatal("no write expected after failed get")
	}))
	err := store.SetAgentResources(context.Background(), fullSettings())
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 500")
}
