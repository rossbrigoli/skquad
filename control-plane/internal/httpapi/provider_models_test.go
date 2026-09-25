// S-125 tests: GET /registry/llm-providers/{id}/models — the admin
// passthrough that powers the register-model dropdown. Covers success,
// upstream auth rejection, upstream timeout, non-admin rejection, and
// unknown-provider 404.

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func createProviderWithBase(t *testing.T, handler http.Handler, name, baseURL, apiKeyRef string) string {
	t.Helper()
	var provider struct {
		ID string `json:"id"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":        name,
		"kind":        "openai",
		"base_url":    baseURL,
		"api_key_ref": apiKeyRef,
	}, http.StatusCreated, &provider)
	return provider.ID
}

// doRawAuth performs an authenticated request and returns the recorder
// so tests can assert on non-2xx status codes and bodies.
func doRawAuth(t *testing.T, handler http.Handler, authorization, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", authorization)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestListProviderModelsSuccess(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		// Unsorted + duplicate on purpose: the handler must sort and dedupe.
		_, _ = w.Write([]byte(`{"data":[{"id":"zeta-model"},{"id":"alpha-model"},{"id":"alpha-model"},{"id":"  "}]}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "upstream-ok", upstream.URL+"/v1", "sk-test-123")

	var out struct {
		ProviderID string   `json:"provider_id"`
		Models     []string `json:"models"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodGet, pathProvidersPrefix+id+pathModels, nil, http.StatusOK, &out)
	require.Equal(t, id, out.ProviderID)
	require.Equal(t, []string{"alpha-model", "zeta-model"}, out.Models)
	require.Equal(t, "Bearer sk-test-123", gotAuth, "credential must be sent upstream")
}

func TestListProviderModelsAuthError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "upstream-401", upstream.URL+"/v1", "bad-key")

	rec := doRawAuth(t, handler, authAdmin, http.MethodGet, pathProvidersPrefix+id+pathModels)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"provider_auth"`)
}

func TestListProviderModelsTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	// Shrink the shared client timeout for the test window so we exercise
	// the real timeout path without waiting 10s.
	orig := providerModelsClient
	providerModelsClient = &http.Client{Timeout: 50 * time.Millisecond}
	defer func() { providerModelsClient = orig }()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "upstream-slow", upstream.URL+"/v1", "key")

	rec := doRawAuth(t, handler, authAdmin, http.MethodGet, pathProvidersPrefix+id+pathModels)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"provider_timeout"`)
}

func TestListProviderModelsRequiresAdmin(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "upstream-admin", "http://models.invalid/v1", "key")

	rec := doRawAuth(t, handler, authAlice, http.MethodGet, pathProvidersPrefix+id+pathModels)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestListProviderModelsUnknownProvider(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	rec := doRawAuth(t, handler, authAdmin, http.MethodGet, "/api/v1/registry/llm-providers/does-not-exist/models")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestFetchProviderModelsRejectsBadBaseURL(t *testing.T) {
	_, err := fetchProviderModels(t.Context(), http.DefaultClient, "not a url", "k")
	require.Error(t, err)
	_, err = fetchProviderModels(t.Context(), http.DefaultClient, "ftp://x/v1", "k")
	require.Error(t, err)
}
