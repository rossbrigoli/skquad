// S-180 — pre-save "Test" buttons for provider/model configuration.
//
// Two admin-only endpoints that exercise credentials WITHOUT persisting
// anything, so an admin can verify a token before saving:
//
//	POST /api/v1/registry/llm-providers/test  {base_url, api_key?, provider_id?}
//	POST /api/v1/ai-models/test               {provider_id, model_name}
//
// The provider test reuses the S-125 model-list call (GET {base}/models)
// — a cheap, provider-native request that verifies the credential without
// burning completion tokens. The model test sends a minimal chat round-trip
// ("Reply exactly with PONG", tiny max_tokens) and checks for PONG.
//
// Contract: both return 200 with {ok, reason, latency_ms, detail} once
// the test itself ran (ok=false carries why: auth_failed, timeout,
// unreachable, provider_error, pong_mismatch). Hard input problems are
// 400/404 as usual. The API key is NEVER echoed in a response, detail,
// or log — details carry only status codes, counts, and model text.
//
// SSRF note: these endpoints take arbitrary URLs by design (that IS the
// feature) and are therefore platform-admin-only, the same trust level as
// provider registration itself (see provider_models.go header).

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// providerTestTimeout bounds every S-180 upstream call so a hung
// provider cannot wedge the control-plane (task contract ~15s).
const providerTestTimeout = 15 * time.Second

// providerTestClient is the shared S-180 upstream client. Package-level
// so tests can shrink the timeout without waiting the full 15s.
var providerTestClient = &http.Client{Timeout: providerTestTimeout}

// pongProbe is the model-test prompt: cheap and unambiguous.
const pongProbe = "Reply exactly with PONG"

// Test-result reasons (stable wire values; the UI maps them to labels).
const (
	testReasonConnected    = "connected"
	testReasonAuthFailed   = "auth_failed"
	testReasonTimeout      = "timeout"
	testReasonUnreachable  = "unreachable"
	testReasonProviderErr  = "provider_error"
	testReasonPongMismatch = "pong_mismatch"
)

// testResult is the S-180 response envelope.
type testResult struct {
	OK        bool   `json:"ok"`
	Reason    string `json:"reason"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail,omitempty"`
}

func writeTestResult(w http.ResponseWriter, res testResult) {
	writeJSON(w, http.StatusOK, res)
}

// classifyProviderTestError maps a fetchProviderModels error onto an
// S-180 reason. Extracted for clarity (S3776).
func classifyProviderTestError(err error) (reason, detail string) {
	switch {
	case errors.Is(err, errProviderAuth):
		return testReasonAuthFailed, "provider rejected the credential (HTTP 401/403) — check the API key"
	case errors.Is(err, context.DeadlineExceeded):
		return testReasonTimeout, fmt.Sprintf("provider did not answer within %s", providerTestTimeout)
	default:
		return testReasonUnreachable, err.Error()
	}
}

// testProviderConnection handles POST /registry/llm-providers/test.
// Tests an UNSAVED provider: base_url + api_key come straight from the
// form. On the edit form an empty api_key with a provider_id falls back
// to the stored (Secret-backed) key so "leave blank to keep" is testable.
func (s *Server) testProviderConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req struct {
		BaseURL    string `json:"base_url"`
		APIKey     string `json:"api_key"`
		ProviderID string `json:"provider_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	apiKey := strings.TrimSpace(req.APIKey)
	// Edit-form fallback: use the stored provider's base URL / key for
	// whatever the form left blank.
	if providerID := strings.TrimSpace(req.ProviderID); providerID != "" && (baseURL == "" || apiKey == "") {
		provider, err := s.store.GetLLMProvider(r.Context(), providerID)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if baseURL == "" {
			baseURL = provider.BaseURL
		}
		if apiKey == "" {
			key, err := s.resolveProviderKey(r.Context(), provider)
			if err != nil {
				writeError(w, http.StatusBadGateway, "provider_key_resolve_failed", "could not resolve the provider's stored API key")
				return
			}
			apiKey = key
		}
	}
	if baseURL == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "base_url is required (or pass provider_id to test the stored value)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), providerTestTimeout)
	defer cancel()
	start := time.Now()
	models, err := fetchProviderModels(ctx, providerTestClient, baseURL, apiKey)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		reason, detail := classifyProviderTestError(err)
		writeTestResult(w, testResult{OK: false, Reason: reason, LatencyMS: latency, Detail: detail})
		return
	}
	writeTestResult(w, testResult{
		OK:        true,
		Reason:    testReasonConnected,
		LatencyMS: latency,
		Detail:    fmt.Sprintf("credential accepted — %d model(s) visible", len(models)),
	})
}

// providerChatEndpoint validates the base URL and appends the given
// OpenAI-compatible suffix, mirroring fetchProviderModels' URL rules.
func providerChatEndpoint(baseURL, suffix string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return "", errors.New("provider has no base_url configured")
	}
	endpoint, err := url.Parse(trimmed + suffix)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return "", errors.New("provider base_url must be an absolute http/https URL")
	}
	return endpoint.String(), nil
}

// buildModelTestRequest builds the minimal chat-completion request for a
// provider kind. Anthropic uses its native Messages API (x-api-key auth);
// every other kind goes through the OpenAI-compatible chat/completions
// shape (which openai, ollama_chat, azure and OpenAI-compatible gateways
// such as Gemini's /openai endpoint all accept).
func buildModelTestRequest(kind, baseURL, apiKey, model string) (*http.Request, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"messages":   []map[string]string{{"role": "user", "content": pongProbe}},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(kind), "anthropic") {
		// Anthropic's canonical path is /v1/messages; tolerate bases
		// that already end in /v1.
		suffix := "/messages"
		if !strings.HasSuffix(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "/v1") {
			suffix = "/v1/messages"
		}
		endpoint, err := providerChatEndpoint(baseURL, suffix)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
		return req, nil
	}
	endpoint, err := providerChatEndpoint(baseURL, "/chat/completions")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

// extractOpenAIChatText pulls choices[0].message.content out of an
// OpenAI-format completion response.
func extractOpenAIChatText(body []byte) (string, bool) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) == 0 {
		return "", false
	}
	return out.Choices[0].Message.Content, true
}

// extractAnthropicText joins the text blocks of an Anthropic Messages
// response.
func extractAnthropicText(body []byte) (string, bool) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Content) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(out.Content))
	for _, block := range out.Content {
		if block.Type == "text" || block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " "), true
}

// truncateDetail keeps upstream/model text out of unbounded responses.
// Model output is safe to show; raw upstream bodies are never shown,
// only status codes, so no credential echo is possible.
func truncateDetail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// testModelRoundTrip handles POST /ai-models/test — the pre-save model
// check. The model is NOT persisted: the request carries provider_id +
// model_name from the form; the provider's stored base URL and Secret
// key are resolved server-side (the key never travels to the browser and
// never comes back).
func (s *Server) testModelRoundTrip(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req struct {
		ProviderID string `json:"provider_id"`
		ModelName  string `json:"model_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	providerID := strings.TrimSpace(req.ProviderID)
	modelName := strings.TrimSpace(req.ModelName)
	if providerID == "" || modelName == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "provider_id and model_name are required")
		return
	}
	provider, err := s.store.GetLLMProvider(r.Context(), providerID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	apiKey, err := s.resolveProviderKey(r.Context(), provider)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider_key_resolve_failed", "could not resolve the provider's stored API key")
		return
	}
	upReq, err := buildModelTestRequest(provider.Kind, provider.BaseURL, apiKey, modelName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), providerTestTimeout)
	defer cancel()
	start := time.Now()
	resp, err := providerTestClient.Do(upReq.WithContext(ctx))
	latency := time.Since(start).Milliseconds()
	if err != nil {
		reason, detail := classifyProviderTestError(err)
		writeTestResult(w, testResult{OK: false, Reason: reason, LatencyMS: latency, Detail: detail})
		return
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		writeTestResult(w, testResult{OK: false, Reason: testReasonAuthFailed, LatencyMS: latency,
			Detail: "provider rejected the credential (HTTP 401/403) — check the provider's API key"})
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Deliberately NOT echoing the upstream body: it can contain
		// request echoes. Status code alone is enough to diagnose.
		writeTestResult(w, testResult{OK: false, Reason: testReasonProviderErr, LatencyMS: latency,
			Detail: fmt.Sprintf("provider returned HTTP %d for model %q", resp.StatusCode, modelName)})
		return
	}
	if readErr != nil {
		writeTestResult(w, testResult{OK: false, Reason: testReasonProviderErr, LatencyMS: latency,
			Detail: "could not read the provider completion response"})
		return
	}
	extract := extractOpenAIChatText
	if strings.EqualFold(strings.TrimSpace(provider.Kind), "anthropic") {
		extract = extractAnthropicText
	}
	text, ok := extract(body)
	if !ok {
		writeTestResult(w, testResult{OK: false, Reason: testReasonProviderErr, LatencyMS: latency,
			Detail: "provider answered but not in a parseable chat-completion shape"})
		return
	}
	if !strings.Contains(strings.ToUpper(text), "PONG") {
		writeTestResult(w, testResult{OK: false, Reason: testReasonPongMismatch, LatencyMS: latency,
			Detail: fmt.Sprintf("model replied %q instead of PONG", truncateDetail(text, 120))})
		return
	}
	writeTestResult(w, testResult{OK: true, Reason: testReasonConnected, LatencyMS: latency,
		Detail: fmt.Sprintf("model %q round-trip OK (PONG)", modelName)})
}
