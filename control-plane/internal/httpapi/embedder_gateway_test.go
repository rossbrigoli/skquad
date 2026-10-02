// S-212 tests — embedder gateway registration.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

func TestRegisterEmbedderOnceCreatesAndReloads(t *testing.T) {
	var created []map[string]any
	var updated []map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case "/model/new":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			created = append(created, b)
			json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"id": "dep-1"}})
		case "/model/update":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			updated = append(updated, b)
			json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"id": "dep-9"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer gw.Close()

	client, err := newLiteLLMGatewayClient(gw.URL, "master")
	require.NoError(t, err)
	reloader := &fakeGatewayReloader{}
	spec := GatewayModelSpec{ModelName: "qwen3-embed-0.6b", LitellmModel: "openai/qwen3-embed-0.6b", APIBase: "http://embedder:8080/v1", APIKey: "sk-skquad-internal"}

	createdFlag, err := registerEmbedderOnce(context.Background(), client, reloader, spec)
	require.NoError(t, err)
	require.True(t, createdFlag)
	require.Len(t, created, 1)
	require.Equal(t, "qwen3-embed-0.6b", created[0]["model_name"])
	require.Equal(t, 1, reloader.calls)
}

func TestRegisterEmbedderOnceIdempotentUpdate(t *testing.T) {
	var updated []map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model/info":
			json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"model_name": "qwen3-embed-0.6b", "model_info": map[string]any{"id": "dep-9"}},
			}})
		case "/model/update":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			updated = append(updated, b)
			json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"id": "dep-9"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer gw.Close()

	client, err := newLiteLLMGatewayClient(gw.URL, "master")
	require.NoError(t, err)
	reloader := &fakeGatewayReloader{}
	spec := GatewayModelSpec{ModelName: "qwen3-embed-0.6b", LitellmModel: "openai/qwen3-embed-0.6b", APIBase: "http://embedder:8080/v1", APIKey: "sk-skquad-internal"}

	createdFlag, err := registerEmbedderOnce(context.Background(), client, reloader, spec)
	require.NoError(t, err)
	require.False(t, createdFlag, "existing deployment must update in place, not create")
	require.Len(t, updated, 1)
	require.Equal(t, "dep-9", updated[0]["id"])
	require.Equal(t, 0, reloader.calls, "no reload when nothing was created")
}

func TestRegisterEmbedderDisabledWithoutFlags(t *testing.T) {
	// Must not panic or spawn goroutines when disabled.
	RegisterEmbedderGatewayModel(context.Background(), &config.Config{})
	RegisterEmbedderGatewayModel(context.Background(), &config.Config{MemoryEmbeddingsEnabled: true})
	RegisterEmbedderGatewayModel(context.Background(), &config.Config{MemoryEmbeddingsEnabled: true, MemoryEmbeddingModel: "m", LiteLLMAdminURL: "http://gw:4000"})
	// master key missing → disabled path, no panic.
}

func TestEmbedderServiceBaseURL(t *testing.T) {
	require.Equal(t, "http://skquad-embedder.skquad.svc.cluster.local:8080/v1", embedderServiceBaseURL(&config.Config{}))
	require.Equal(t, "http://custom:9/v1", embedderServiceBaseURL(&config.Config{MemoryEmbedderURL: "http://custom:9/v1"}))
}

type fakeGatewayReloader struct{ calls int }

func (f *fakeGatewayReloader) ReloadGateway(_ context.Context) error { f.calls++; return nil }
