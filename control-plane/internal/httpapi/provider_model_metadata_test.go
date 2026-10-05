// S-208 tests: GET /registry/ai-providers/{id}/model-metadata — the
// admin passthrough powering the Model Edit dialog's "Fetch from
// provider" prefill. Covers OpenRouter-style OpenAI-compatible metadata,
// vanilla OpenAI "nothing available", ollama native /api/show parsing
// (model_info + num_ctx fallback), the anthropic no-metadata contract,
// and handler-level validation/gating.

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFetchModelMetadataOpenRouterStyle(t *testing.T) {
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		require.Equal(t, "/v1/models/gpt-4o", gotPath)
		_, _ = w.Write([]byte(`{"id":"gpt-4o","context_length":128000,
			"pricing":{"prompt":"0.0000025","completion":"0.00001","cache_read":"0.00000125"}}`))
	}))
	defer upstream.Close()

	meta, err := fetchProviderModelMetadata(t.Context(), upstream.Client(), upstream.URL+"/v1", "sk-k", "openai", "gpt-4o")
	require.NoError(t, err)
	require.True(t, meta.found())
	require.Equal(t, 128000, meta.ContextWindow)
	require.Equal(t, "Bearer sk-k", gotAuth)
	require.InDelta(t, 2.5, meta.Pricing["input_per_1m"], 1e-9)
	require.InDelta(t, 10.0, meta.Pricing["output_per_1m"], 1e-9)
	require.InDelta(t, 1.25, meta.Pricing["cached_input_per_1m"], 1e-9)
}

func TestFetchModelMetadataVanillaOpenAI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"gpt-4o","object":"model","owned_by":"openai"}`))
	}))
	defer upstream.Close()

	meta, err := fetchProviderModelMetadata(t.Context(), upstream.Client(), upstream.URL+"/v1", "sk-k", "openai", "gpt-4o")
	require.NoError(t, err)
	require.False(t, meta.found(), "vanilla OpenAI exposes nothing — never fabricate")
	require.Empty(t, meta.Pricing)
}

func TestFetchModelMetadataAnthropicNoMetadata(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer upstream.Close()

	meta, err := fetchProviderModelMetadata(t.Context(), upstream.Client(), upstream.URL+"/v1", "sk-k", "anthropic", "claude-x")
	require.NoError(t, err)
	require.False(t, meta.found())
	require.False(t, called, "anthropic has no per-model metadata; skip the upstream call")
}

func TestFetchOllamaModelMetadataStripsV1AndParsesContextLength(t *testing.T) {
	var gotPath, gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"model_info":{"llama.general.context_length":8192.0,
			"llama.architecture.name":"llama"},"parameters":"temperature 0.7"}`))
	}))
	defer upstream.Close()

	meta, err := fetchProviderModelMetadata(t.Context(), upstream.Client(), upstream.URL+"/v1", "", "ollama", "llama3")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/api/show", gotPath, "ollama native API must be reached at /api/show, not /v1/...")
	require.Equal(t, 8192, meta.ContextWindow)
}

func TestParseOllamaShowNumCtxFallback(t *testing.T) {
	meta, err := parseOllamaShowResponse([]byte(`{"parameters":"num_ctx 32768\ntemperature 0.8"}`))
	require.NoError(t, err)
	require.Equal(t, 32768, meta.ContextWindow)

	meta, err = parseOllamaShowResponse([]byte(`{"modelfile":"FROM x\nPARAMETER num_ctx 4096"}`))
	require.NoError(t, err)
	require.Equal(t, 4096, meta.ContextWindow)

	meta, err = parseOllamaShowResponse([]byte(`{"parameters":"temperature 0.8"}`))
	require.NoError(t, err)
	require.Equal(t, 0, meta.ContextWindow)
	require.False(t, meta.found())
}

func TestModelMetadataHandler(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"m1","context_length":200000}`))
	}))
	defer upstream.Close()

	handler, _ := newAIModelHarness(t)
	id := createProviderWithKind(t, handler, "meta-openai", "openai", upstream.URL+"/v1", "sk-metadata-test")

	var out struct {
		ProviderID    string `json:"provider_id"`
		Model         string `json:"model"`
		Found         bool   `json:"found"`
		ContextWindow int    `json:"context_window"`
	}
	doJSONAuth(t, handler, authAdmin, http.MethodGet,
		pathProvidersPrefix+id+"/model-metadata?model=m1", nil, http.StatusOK, &out)
	require.Equal(t, id, out.ProviderID)
	require.Equal(t, "m1", out.Model)
	require.True(t, out.Found)
	require.Equal(t, 200000, out.ContextWindow)
}

func TestModelMetadataHandlerValidationAndGating(t *testing.T) {
	handler, _ := newAIModelHarness(t)
	id := createProviderWithKind(t, handler, "meta-gate", "openai", "http://metadata.invalid/v1", "sk-metadata-test")

	rec := doRawAuth(t, handler, authAdmin, http.MethodGet, pathProvidersPrefix+id+"/model-metadata")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "model query parameter is required")

	rec = doRawAuth(t, handler, authAlice, http.MethodGet, pathProvidersPrefix+id+"/model-metadata?model=x")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestOpenaiPricingPerMillionIgnoresUnknownAndZero(t *testing.T) {
	var raw map[string]string64
	require.NoError(t, json.Unmarshal([]byte(`{"prompt":"0","completion":"0.00001","mystery":"1"}`), &raw))
	out := openaiPricingPerMillion(raw)
	require.NotContains(t, out, "input_per_1m", "zero rates are indistinguishable from unset — drop")
	require.Contains(t, out, "output_per_1m")
	require.NotContains(t, out, "mystery")
}
