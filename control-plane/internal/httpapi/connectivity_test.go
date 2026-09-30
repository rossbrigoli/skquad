// S-180 tests: pre-save Test endpoints.
//   POST /api/v1/registry/llm-providers/test  (connection check via model list)
//   POST /api/v1/ai-models/test               (PONG round-trip)
// Covers success, upstream 401, timeout, unreachable, admin gating,
// edit-form stored-key fallback, the anthropic Messages path, PONG
// mismatch, and — critically — that the API key never appears in any
// response body.

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const pathProviderTest = "/api/v1/registry/llm-providers/test"
const pathAIModelTest = "/api/v1/ai-models/test"

// createProviderWithKind registers a provider with an explicit kind and
// returns its id (createProviderWithBase hardcodes kind=openai).
func createProviderWithKind(t *testing.T, handler http.Handler, name, kind, baseURL, apiKeyRef string) string {
	t.Helper()
	var provider struct {
		ID string `json:"id"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodPost, "/api/v1/registry/llm-providers", map[string]any{
		"name":        name,
		"kind":        kind,
		"base_url":    baseURL,
		"api_key_ref": apiKeyRef,
	}, http.StatusCreated, &provider)
	return provider.ID
}

func postTest(t *testing.T, handler http.Handler, path string, body map[string]any) testResult {
	t.Helper()
	var out testResult
	doJSONAuth(t, handler, authAdmin, http.MethodPost, path, body, http.StatusOK, &out)
	return out
}

// --- provider connection test -----------------------------------------

func TestS180ProviderTestSuccess(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	res := postTest(t, handler, pathProviderTest, map[string]any{
		"base_url": upstream.URL + "/v1",
		"api_key":  "sk-fresh-123",
	})
	require.True(t, res.OK)
	require.Equal(t, testReasonConnected, res.Reason)
	require.Contains(t, res.Detail, "2 model(s) visible")
	require.Equal(t, "Bearer sk-fresh-123", gotAuth)
}

func TestS180ProviderTestAuthFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	req := httptest.NewRequest(http.MethodPost, pathProviderTest, strings.NewReader(`{"base_url":"`+upstream.URL+`/v1","api_key":"sk-wrong-999"}`))
	req.Header.Set("Authorization", authAdmin)
	req.Header.Set("Content-Type", jsonContentType)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res testResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.False(t, res.OK)
	require.Equal(t, testReasonAuthFailed, res.Reason)
	require.NotContains(t, rec.Body.String(), "sk-wrong-999", "API key must never be echoed")
}

func TestS180ProviderTestTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	orig := providerTestClient
	providerTestClient = &http.Client{Timeout: 50 * time.Millisecond}
	defer func() { providerTestClient = orig }()

	handler, _ := newAIModelHarness(t)
	res := postTest(t, handler, pathProviderTest, map[string]any{
		"base_url": upstream.URL + "/v1",
		"api_key":  "sk-any",
	})
	require.False(t, res.OK)
	require.Equal(t, testReasonTimeout, res.Reason)
}

func TestS180ProviderTestUnreachable(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	res := postTest(t, handler, pathProviderTest, map[string]any{
		"base_url": "http://s180-definitely-invalid-host.invalid/v1",
		"api_key":  "sk-any",
	})
	require.False(t, res.OK)
	require.Equal(t, testReasonUnreachable, res.Reason)
}

func TestS180ProviderTestRequiresAdmin(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	rec := doRawAuth(t, handler, authAlice, http.MethodPost, pathProviderTest)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestS180ProviderTestMissingBaseURL(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	rec := doRawAuth(t, handler, authAdmin, http.MethodPost, pathProviderTest)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestS180ProviderTestEditUsesStoredKey(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "stored-key-prov", upstream.URL+"/v1", "sk-stored-555")
	// Edit form: base_url and api_key left blank → stored values used.
	res := postTest(t, handler, pathProviderTest, map[string]any{"provider_id": id})
	require.True(t, res.OK, res.Detail)
	require.Equal(t, "Bearer sk-stored-555", gotAuth)
}

func TestS180ProviderTestUnknownProvider(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	req := httptest.NewRequest(http.MethodPost, pathProviderTest, strings.NewReader(`{"provider_id":"nope"}`))
	req.Header.Set("Authorization", authAdmin)
	req.Header.Set("Content-Type", jsonContentType)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// --- model round-trip test --------------------------------------------

func TestS180ModelTestPongSuccess(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer sk-model-1", r.Header.Get("Authorization"))
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"PONG"}}]}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "pong-prov", upstream.URL+"/v1", "sk-model-1")
	res := postTest(t, handler, pathAIModelTest, map[string]any{"provider_id": id, "model_name": "gpt-test"})
	require.True(t, res.OK, res.Detail)
	require.Equal(t, testReasonConnected, res.Reason)
	require.Equal(t, "gpt-test", gotBody["model"])
	require.Equal(t, float64(16), gotBody["max_tokens"], "must cap tokens for a cheap probe")
}

func TestS180ModelTestPongMismatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Sure! How can I help?"}}]}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "mismatch-prov", upstream.URL+"/v1", "sk-any")
	res := postTest(t, handler, pathAIModelTest, map[string]any{"provider_id": id, "model_name": "chatty-model"})
	require.False(t, res.OK)
	require.Equal(t, testReasonPongMismatch, res.Reason)
	require.Contains(t, res.Detail, "Sure!")
}

func TestS180ModelTestPongCaseInsensitive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "lower-prov", upstream.URL+"/v1", "sk-any")
	res := postTest(t, handler, pathAIModelTest, map[string]any{"provider_id": id, "model_name": "lower-model"})
	require.True(t, res.OK)
}

func TestS180ModelTestAuthFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "403-prov", upstream.URL+"/v1", "sk-bad")
	res := postTest(t, handler, pathAIModelTest, map[string]any{"provider_id": id, "model_name": "any"})
	require.False(t, res.OK)
	require.Equal(t, testReasonAuthFailed, res.Reason)
}

func TestS180ModelTestUnknownModelDoesNotLeakKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		// Malicious-ish upstream body echoing the auth header: must NOT
		// reach the client through our detail.
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key sk-secret-777"}}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithBase(t, handler, "leak-prov", upstream.URL+"/v1", "sk-secret-777")
	var raw strings.Builder
	req := httptest.NewRequest(http.MethodPost, pathAIModelTest, strings.NewReader(`{"provider_id":"`+id+`","model_name":"ghost-model"}`))
	req.Header.Set("Authorization", authAdmin)
	req.Header.Set("Content-Type", jsonContentType)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	raw.WriteString(rec.Body.String())
	require.Equal(t, http.StatusOK, rec.Code, raw.String())
	var res testResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.False(t, res.OK)
	require.Equal(t, testReasonProviderErr, res.Reason)
	require.NotContains(t, raw.String(), "sk-secret-777", "upstream body must not be echoed")
	require.Contains(t, res.Detail, "404")
}

func TestS180ModelTestAnthropicNative(t *testing.T) {
	var gotKey, gotVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"PONG"}]}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithKind(t, handler, "claude-prov", "anthropic", upstream.URL+"/v1", "sk-ant-test")
	res := postTest(t, handler, pathAIModelTest, map[string]any{"provider_id": id, "model_name": "claude-x"})
	require.True(t, res.OK, res.Detail)
	require.Equal(t, "sk-ant-test", gotKey)
	require.Equal(t, "2023-06-01", gotVersion)
}

func TestS180ModelTestRequiresAdmin(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	rec := doRawAuth(t, handler, authAlice, http.MethodPost, pathAIModelTest)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestS180ModelTestMissingFields(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	rec := doRawAuth(t, handler, authAdmin, http.MethodPost, pathAIModelTest)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestS180ModelTestUnknownProvider(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	req := httptest.NewRequest(http.MethodPost, pathAIModelTest, strings.NewReader(`{"provider_id":"ghost","model_name":"m"}`))
	req.Header.Set("Authorization", authAdmin)
	req.Header.Set("Content-Type", jsonContentType)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
