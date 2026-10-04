// S-208 — provider model metadata fetch for the Model Edit dialog's
// "Fetch from provider" action. The control-plane proxies the provider
// server-side (same trust model as S-125's model listing: admin-only
// route, admin-registered base_url/credential) and returns NORMALISED
// metadata the dialog can pre-fill:
//
//	context_window  — tokens
//	pricing        — per-1M rates keyed like the AIModel pricing contract
//
// Coverage by provider kind:
//   - ollama: native POST /api/show exposes context_length (model_info
//     `*.general.context_length`, or `num_ctx` in parameters/modelfile).
//     No pricing is exposed.
//   - openai-compatible: GET {base}/models/{model}. Vanilla OpenAI only
//     returns id/owned_by (nothing to fill); OpenRouter-style responses
//     carry `context_length` and per-token `pricing.prompt/completion`
//     strings which we scale to per-1M.
//   - anthropic: the models API exposes no context/pricing — we return
//     found=false and the UI shows the "not available" hint.
//
// Nothing is ever fabricated: fields the provider doesn't expose stay
// absent so the dialog leaves them editable.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// providerMetadataTimeout bounds the upstream call like the S-125
// model-list contract: a slow provider is "unavailable", never a hang.
const providerMetadataTimeout = 10 * time.Second

var providerMetadataClient = &http.Client{Timeout: providerMetadataTimeout}

// providerModelMetadata is the normalised metadata extracted from a
// provider. Zero/absent fields mean "the provider did not expose this".
type providerModelMetadata struct {
	ContextWindow int                `json:"context_window,omitempty"`
	Pricing       map[string]float64 `json:"pricing,omitempty"`
	Source        string             `json:"source"`
}

// found reports whether ANY prefillable value came back.
func (m *providerModelMetadata) found() bool {
	return m != nil && (m.ContextWindow > 0 || len(m.Pricing) > 0)
}

// numCtxPattern matches `num_ctx 8192` in an ollama parameters string
// or modelfile (PARAMETER num_ctx 8192).
var numCtxPattern = regexp.MustCompile(`(?m)num_ctx\s+(\d+)`)

// fetchProviderModelMetadata dispatches on the provider kind and returns
// whatever normalised metadata the provider exposes. A nil error with a
// found()==false result is a normal outcome ("provider can't say"), not
// a failure.
func fetchProviderModelMetadata(ctx context.Context, client *http.Client, baseURL, apiKey, kind, model string) (*providerModelMetadata, error) {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch {
	case strings.Contains(k, "ollama"):
		return fetchOllamaModelMetadata(ctx, client, baseURL, model)
	case strings.Contains(k, "anthropic"):
		// Anthropic's /v1/models carries no context/pricing metadata.
		return &providerModelMetadata{Source: "anthropic models API exposes no context/pricing"}, nil
	default:
		return fetchOpenAIModelMetadata(ctx, client, baseURL, apiKey, model)
	}
}

// openaiProviderGet performs a kind-aware GET against an OpenAI-shaped
// endpoint (Bearer auth; Anthropic uses x-api-key + anthropic-version),
// reusing the S-125 status/error conventions. Returns the capped body.
func openaiProviderGet(ctx context.Context, client *http.Client, endpoint, apiKey, kind string) ([]byte, error) {
	// #nosec G704 -- endpoint is built from the admin-registered provider
	// base_url on a platform-admin-only route; not attacker-controlled.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("could not build provider model-metadata request")
	}
	if strings.EqualFold(strings.TrimSpace(kind), "anthropic") {
		if key := strings.TrimSpace(apiKey); key != "" {
			req.Header.Set("x-api-key", key)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if key := strings.TrimSpace(apiKey); key != "" {
		req.Header.Set(hdrAuthorization, bearerAuthPrefix+key)
	}
	req.Header.Set(hdrAccept, contentTypeJSON)
	// #nosec G704 -- see note above: admin-registered base_url only.
	resp, err := client.Do(req)
	if err != nil {
		return nil, mapProviderTransportError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("could not read provider model-metadata response")
	}
	if err := providerModelsStatusError(resp); err != nil {
		return nil, err
	}
	return body, nil
}

// fetchOpenAIModelMetadata calls GET {base}/models/{model}. Vanilla
// OpenAI returns nothing useful; OpenRouter-style providers return
// `context_length` plus per-token `pricing` strings.
func fetchOpenAIModelMetadata(ctx context.Context, client *http.Client, baseURL, apiKey, model string) (*providerModelMetadata, error) {
	base, err := providerBaseEndpoint(baseURL, "")
	if err != nil {
		return nil, err
	}
	endpoint := base + "/models/" + url.PathEscape(model)
	body, err := openaiProviderGet(ctx, client, endpoint, apiKey, "openai")
	if err != nil {
		return nil, err
	}
	meta := &providerModelMetadata{Source: "openai-compatible models API"}
	var parsed struct {
		ContextLength    *int                `json:"context_length"`
		MaxContextLength *int                `json:"max_context_length"`
		Pricing          map[string]string64 `json:"pricing"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("provider model-metadata response was not valid JSON")
	}
	if parsed.ContextLength != nil && *parsed.ContextLength > 0 {
		meta.ContextWindow = *parsed.ContextLength
	} else if parsed.MaxContextLength != nil && *parsed.MaxContextLength > 0 {
		meta.ContextWindow = *parsed.MaxContextLength
	}
	if pricing := openaiPricingPerMillion(parsed.Pricing); len(pricing) > 0 {
		meta.Pricing = pricing
	}
	return meta, nil
}

// string64 accepts JSON numbers or numeric strings for pricing fields
// (OpenRouter quotes per-token rates as strings).
type string64 float64

func (s *string64) UnmarshalJSON(data []byte) error {
	var num float64
	if err := json.Unmarshal(data, &num); err == nil {
		*s = string64(num)
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return errors.New("pricing value must be numeric")
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
	if err != nil {
		return errors.New("pricing value must be numeric")
	}
	*s = string64(parsed)
	return nil
}

// openaiPricingPerMillion maps per-TOKEN provider rates onto the
// per-1M keys the AIModel pricing contract uses. Unknown keys are
// ignored; non-positive values are dropped (0 pricing on a paid API is
// indistinguishable from "not set", and the dialog defaults to 0
// anyway).
func openaiPricingPerMillion(raw map[string]string64) map[string]float64 {
	mapping := map[string]string{
		"prompt":             "input_per_1m",
		"completion":         "output_per_1m",
		"cache_read":         "cached_input_per_1m",
		"prompt_cache_read":  "cached_input_per_1m",
		"cache_write":        "cache_write_per_1m",
		"prompt_cache_write": "cache_write_per_1m",
	}
	out := map[string]float64{}
	for src, dst := range mapping {
		value, ok := raw[src]
		if !ok {
			continue
		}
		perMillion := float64(value) * 1_000_000
		if perMillion <= 0 {
			continue
		}
		// Round to 6 decimal places to kill float dust (0.0000029999…).
		out[dst] = math.Round(perMillion*1e6) / 1e6
	}
	return out
}

// fetchOllamaModelMetadata calls the NATIVE ollama POST /api/show.
// Ollama's OpenAI-compatible base (…/v1) is stored for chat; the
// native API lives at the same host under /api, so strip a trailing
// /v1 before appending /api/show.
func fetchOllamaModelMetadata(ctx context.Context, client *http.Client, baseURL, model string) (*providerModelMetadata, error) {
	nativeBase, err := providerBaseEndpoint(baseURL, "/v1")
	if err != nil {
		return nil, err
	}
	endpoint := nativeBase + "/api/show"
	payload, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload)) // #nosec G704 -- endpoint built from admin-registered base_url + fixed path on an admin-only route; model name goes in the POST body, not the URL
	if err != nil {
		return nil, errors.New("could not build ollama model-metadata request")
	}
	req.Header.Set(hdrContentType, contentTypeJSON)
	req.Header.Set(hdrAccept, contentTypeJSON)
	// #nosec G704 -- admin-registered base_url, admin-only route.
	resp, err := client.Do(req)
	if err != nil {
		return nil, mapProviderTransportError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("could not read ollama model-metadata response")
	}
	if err := providerModelsStatusError(resp); err != nil {
		return nil, err
	}
	return parseOllamaShowResponse(body)
}

// parseOllamaShowResponse extracts the context window from ollama's
// /api/show JSON: prefer model_info `*.general.context_length`, fall
// back to `num_ctx` in the parameters string or modelfile.
func parseOllamaShowResponse(body []byte) (*providerModelMetadata, error) {
	var parsed struct {
		ModelInfo  map[string]any `json:"model_info"`
		Parameters string         `json:"parameters"`
		ModelFile  string         `json:"modelfile"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("ollama model-metadata response was not valid JSON")
	}
	meta := &providerModelMetadata{Source: "ollama api/show"}
	meta.ContextWindow = ollamaContextLengthFromModelInfo(parsed.ModelInfo)
	if meta.ContextWindow == 0 {
		meta.ContextWindow = ollamaContextLengthFromText(parsed.Parameters, parsed.ModelFile)
	}
	return meta, nil
}

// ollamaContextLengthFromModelInfo prefers the structured model_info
// `*.general.context_length` entry (S-189 split).
func ollamaContextLengthFromModelInfo(info map[string]any) int {
	for key, value := range info {
		if !strings.HasSuffix(key, ".general.context_length") {
			continue
		}
		if num, ok := value.(float64); ok && num > 0 {
			return int(num)
		}
	}
	return 0
}

// ollamaContextLengthFromText falls back to `num_ctx` found in the
// parameters string or modelfile (S-189 split).
func ollamaContextLengthFromText(sources ...string) int {
	for _, source := range sources {
		match := numCtxPattern.FindStringSubmatch(source)
		if match == nil {
			continue
		}
		if num, err := strconv.Atoi(match[1]); err == nil && num > 0 {
			return num
		}
	}
	return 0
}

// providerBaseEndpoint validates the admin-registered base_url and
// strips an optional trailing suffix (e.g. "/v1") so callers can
// append their own path. Mirrors the S-125 URL validation rules.
func providerBaseEndpoint(baseURL, stripSuffix string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return "", errors.New("provider has no base_url configured")
	}
	endpoint, err := url.Parse(trimmed)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return "", errors.New("provider base_url must be an absolute http/https URL")
	}
	if stripSuffix != "" && strings.HasSuffix(trimmed, stripSuffix) {
		trimmed = strings.TrimSuffix(trimmed, stripSuffix)
	}
	return trimmed, nil
}

// getLLMProviderModelMetadata handles
// GET /api/v1/registry/llm-providers/{providerID}/model-metadata?model=NAME.
// Admin-only (it exercises the provider credential). Error mapping and
// the "UI never blocked" contract match listLLMProviderModels.
func (s *Server) getLLMProviderModelMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	modelName := strings.TrimSpace(r.URL.Query().Get("model"))
	if modelName == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "model query parameter is required")
		return
	}
	provider, err := s.store.GetLLMProvider(r.Context(), chi.URLParam(r, "providerID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerMetadataTimeout)
	defer cancel()
	apiKey, err := s.resolveProviderKey(ctx, provider)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider_key_resolve_failed", "could not resolve the provider's stored API key")
		return
	}
	meta, err := fetchProviderModelMetadata(ctx, providerMetadataClient, provider.BaseURL, apiKey, provider.Kind, modelName)
	if err != nil {
		switch {
		case errors.Is(err, errProviderAuth):
			writeError(w, http.StatusBadGateway, "provider_auth", "provider rejected the registered credential — check the provider's API key")
		case errors.Is(err, context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, "provider_timeout", "provider model-metadata request timed out")
		default:
			writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider_id":    provider.ID,
		"model":          modelName,
		"found":          meta.found(),
		"context_window": meta.ContextWindow,
		"pricing":        meta.Pricing,
		"source":         meta.Source,
	})
}
