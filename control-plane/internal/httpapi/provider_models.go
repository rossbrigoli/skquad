// S-125 — live provider model listing for the AI Model registration
// dropdown. The control-plane proxies the provider's OpenAI-compatible
// GET {base_url}/models server-side so the registered credential never
// reaches the browser; the UI only sees the sorted model names.
//
// Trust model: base_url and api_key_ref are admin-registered provider
// fields (same trust level as the LiteLLM admin client) and this endpoint
// is platform-admin-only, so the outbound request is not user-controlled.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// providerModelsTimeout bounds the upstream call per the S-125 contract
// (~10s). Provider /models is cheap; a slower answer is treated as
// unavailable so the dialog never hangs.
const providerModelsTimeout = 10 * time.Second

// providerModelsClient is the shared upstream client. Package-level so
// tests can shrink the timeout without waiting ten seconds.
var providerModelsClient = &http.Client{Timeout: providerModelsTimeout}

// errProviderAuth marks an upstream credential rejection (401/403). The
// handler maps it to a distinct error code so the UI can hint "check the
// provider's API key" rather than a generic failure.
var errProviderAuth = errors.New("provider rejected the registered credential")

// fetchProviderModels calls GET {baseURL}/models (OpenAI-compatible)
// with the provider's registered key and returns the sorted, deduped
// model ids. Empty api_keyRef omits the Authorization header (local
// providers such as ollama often need no credential).
func fetchProviderModels(ctx context.Context, client *http.Client, baseURL, apiKeyRef string) ([]string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, errors.New("provider has no base_url configured")
	}
	endpoint, err := url.Parse(trimmed + "/models")
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, errors.New("provider base_url must be an absolute http/https URL")
	}
	// #nosec G704 -- endpoint is built from the admin-registered provider
	// base_url on a platform-admin-only route; not attacker-controlled.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("could not build provider model-list request")
	}
	if key := strings.TrimSpace(apiKeyRef); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, context.DeadlineExceeded
		}
		return nil, errors.New("provider is unreachable")
	}
	defer resp.Body.Close()
	// 1 MiB cap: a model list is small; anything larger is junk and must
	// not balloon the control-plane's memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("could not read provider model-list response")
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, errProviderAuth
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, errors.New("provider returned " + resp.Status)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("provider model-list response was not valid OpenAI-format JSON")
	}
	seen := make(map[string]bool, len(out.Data))
	models := make([]string, 0, len(out.Data))
	for _, entry := range out.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}

// listLLMProviderModels handles GET /api/v1/registry/llm-providers/{providerID}/models.
// Admin-only (it exercises the provider credential). Maps upstream
// failures onto clear error codes: provider_auth (401/403 upstream),
// provider_timeout, provider_error. The UI falls back to free-text input
// on any failure so registration is never blocked.
func (s *Server) listLLMProviderModels(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	provider, err := s.store.GetLLMProvider(r.Context(), chi.URLParam(r, "providerID"))
	if err != nil {
		writeStorageError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerModelsTimeout)
	defer cancel()
	models, err := fetchProviderModels(ctx, providerModelsClient, provider.BaseURL, provider.APIKeyRef)
	if err != nil {
		switch {
		case errors.Is(err, errProviderAuth):
			writeError(w, http.StatusBadGateway, "provider_auth", "provider rejected the registered credential — check the provider's API key")
		case errors.Is(err, context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, "provider_timeout", "provider model list request timed out")
		default:
			writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider_id": provider.ID,
		"models":      models,
	})
}
