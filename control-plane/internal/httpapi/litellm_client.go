package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type liteLLMGatewayClient struct {
	baseURL   string
	masterKey string
	client    *http.Client
}

// newLiteLLMGatewayClient builds the admin client for the LiteLLM gateway.
// The base URL is server configuration (SKQUAD_LLM_GATEWAY_URL /
// SKQUAD_LITELLM_ADMIN_URL) supplied by the platform admin — never
// per-request user input — and it is validated here so a typo or a
// scheme-injection in config fails at startup instead of turning the
// control plane into an open proxy.
func newLiteLLMGatewayClient(baseURL, masterKey string) (*liteLLMGatewayClient, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("litellm: gateway admin URL %q must be an absolute http/https URL", baseURL)
	}
	return &liteLLMGatewayClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		masterKey: strings.TrimSpace(masterKey),
		client:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *liteLLMGatewayClient) ProvisionAgentKey(ctx context.Context, req GatewayKeyRequest) (string, string, error) {
	metadata := map[string]string{
		"skquad_agent_id": req.AgentID,
		"skquad_squad_id": req.SquadID,
	}
	if req.FallbackModel != "" {
		// Observability: which model this key is supposed to fail over to
		// (ADR-0010 D6). The router config below is what enforces it.
		metadata["skquad_fallback_model"] = req.FallbackModel
	}
	body := map[string]any{
		"models":    req.Models,
		"metadata":  metadata,
		"key_alias": fmt.Sprintf("skquad-agent-%s", req.AgentID),
	}
	if rs := d7RouterSettings(req.PrimaryModel, req.FallbackModel); rs != nil {
		body["router_settings"] = rs
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("litellm: marshal key request: %w", err)
	}
	// #nosec G704 -- c.baseURL is validated admin-supplied config, not user input
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/key/generate", bytes.NewReader(payload))
	if err != nil {
		return "", "", fmt.Errorf("litellm: build key request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.masterKey)
	httpReq.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- URL is the validated admin-configured gateway base + fixed
	// path; no user-controlled component reaches this request.
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("litellm: generate key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("litellm: generate key: %s: %s", resp.Status, gatewayResponseSnippet(resp.Body))
	}

	var out struct {
		Key   string `json:"key"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("litellm: decode key response: %w", err)
	}
	if strings.TrimSpace(out.Key) == "" {
		return "", "", fmt.Errorf("litellm: key response did not include key")
	}
	if strings.TrimSpace(out.Token) == "" {
		return "", "", fmt.Errorf("litellm: key response did not include token")
	}
	return out.Key, out.Token, nil
}

// FindKeyByAlias returns the token (sha256 hash) of the gateway virtual key
// carrying the given alias, or found=false when no such key exists.
//
// This exists so provisioning can be made idempotent against LiteLLM's
// unique-key-alias constraint (S-129): when the control plane's identity
// row has lost the token (status "none") but the gateway still holds the
// agent's key, the sync path adopts the existing key instead of failing
// /key/generate with "alias already exists".
func (c *liteLLMGatewayClient) FindKeyByAlias(ctx context.Context, alias string) (string, bool, error) {
	if strings.TrimSpace(alias) == "" {
		return "", false, nil
	}
	q := url.Values{}
	q.Set("key_alias", alias)
	q.Set("return_full_object", "true")
	// #nosec G704 -- c.baseURL is validated admin-supplied config, not user input
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/key/list?"+q.Encode(), nil)
	if err != nil {
		return "", false, fmt.Errorf("litellm: build key list request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.masterKey)

	// #nosec G704 -- URL is the validated admin-configured gateway base + fixed
	// path; no user-controlled component reaches this request.
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return "", false, fmt.Errorf("litellm: list keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("litellm: list keys: %s: %s", resp.Status, gatewayResponseSnippet(resp.Body))
	}
	var out struct {
		Keys []struct {
			Token    string `json:"token"`
			KeyAlias string `json:"key_alias"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, fmt.Errorf("litellm: decode key list: %w", err)
	}
	for _, k := range out.Keys {
		if k.KeyAlias == alias && strings.TrimSpace(k.Token) != "" {
			return k.Token, true, nil
		}
	}
	return "", false, nil
}

// UpdateAgentKey rotates the model allow-list and fallback/router
// configuration of an existing virtual key, identified by its token (the
// sha256 hash LiteLLM returned at generation).
func (c *liteLLMGatewayClient) UpdateAgentKey(ctx context.Context, token string, req GatewayKeyRequest) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("litellm: update key requires a token")
	}
	if req.Models == nil {
		req.Models = []string{}
	}
	body := map[string]any{
		"key":    token,
		"models": req.Models,
	}
	// Always send router_settings so a removed fallback clears the old
	// fallbacks mapping instead of leaving it live on the key.
	body["router_settings"] = d7RouterSettings(req.PrimaryModel, req.FallbackModel)
	return c.postKeyAdmin(ctx, "/key/update", body, "update key")
}

// d7RouterSettings compiles the ADR-0010 D7 failure-class policy into the
// per-key router_settings LiteLLM accepts on /key/generate and
// /key/update. Verified against litellm.types.router.UpdateRouterConfig
// (fallbacks, retry_policy) and the proxy's key→router settings
// precedence (key > team > global).
//
// Expressible:
//   - transport errors / 5xx / timeouts / 429-after-retries → generic
//     `fallbacks` fire after the retry_policy budget for that class is
//     exhausted, which is exactly "429 after retries + backoff".
//   - upstream 401/403 → AuthenticationErrorRetries=0 means no pointless
//     retry with the same dead credential; the generic fallback still fires
//     so the agent keeps working on the fallback model. The loud alert is
//     emitted by the gateway's metering callback (llm.upstream_auth_alert).
//
// NOT expressible in LiteLLM's API (accepted gap, see WP3 report):
//   - 400-class exclusion: generic `fallbacks` fire on every exception
//     class; there is no way to exclude BadRequestError/context-length
//     errors from the fallback attempt itself. The closest faithful
//     equivalent is BadRequestErrorRetries=0 — the bad request is not
//     retried, but the fallback deployment is still tried once. A smaller-
//     context fallback on a context-length error therefore still happens.
func d7RouterSettings(primary, fallback string) map[string]any {
	fallbacks := []map[string][]string{}
	if primary != "" && fallback != "" {
		fallbacks = append(fallbacks, map[string][]string{primary: {fallback}})
	}
	return map[string]any{
		// Always present so a removed fallback clears the old mapping
		// instead of leaving it live on the key.
		"fallbacks": fallbacks,
		"retry_policy": map[string]any{
			// D7: 400-class errors are not retryable — a different attempt
			// cannot fix a malformed/oversized prompt.
			"BadRequestErrorRetries": 0,
			// Upstream auth failure: retrying the same dead key is pointless.
			"AuthenticationErrorRetries": 0,
			// Transient classes: retry with backoff, then fall back.
			"RateLimitErrorRetries":          2,
			"TimeoutErrorRetries":            2,
			"InternalServerErrorRetries":     2,
			"ServiceUnavailableErrorRetries": 2,
		},
	}
}

// RevokeAgentKey deletes the virtual key identified by its token.
func (c *liteLLMGatewayClient) RevokeAgentKey(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("litellm: revoke key requires a token")
	}
	return c.postKeyAdmin(ctx, "/key/delete", map[string]any{"key": token}, "delete key")
}

func (c *liteLLMGatewayClient) postKeyAdmin(ctx context.Context, path string, body map[string]any, op string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("litellm: marshal %s request: %w", op, err)
	}
	// #nosec G704 -- c.baseURL is validated admin-supplied config, not user input
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("litellm: build %s request: %w", op, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.masterKey)
	httpReq.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- URL is the validated admin-configured gateway base + fixed
	// path; no user-controlled component reaches this request.
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("litellm: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("litellm: %s: %s: %s", op, resp.Status, gatewayResponseSnippet(resp.Body))
	}
	return nil
}

func gatewayResponseSnippet(r io.Reader) string {
	body, err := io.ReadAll(io.LimitReader(r, 2048))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}
