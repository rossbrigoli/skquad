// Package embeddings provides the control-plane client for generating
// text embeddings through the LiteLLM gateway (S-212).
//
// Single embedding path: every write-time and query-time embedding is
// produced by the gateway's OpenAI-compatible /v1/embeddings endpoint
// with the registered embedder model, so auth, routing and metering
// stay unified. The gateway in turn fronts the skquad-embedder pod
// (llama.cpp + Qwen3-Embedding-0.6B, 1024-dim, last-token pooling).
package embeddings

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

// Client calls the gateway /v1/embeddings endpoint for one configured
// embedding model.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// DefaultTimeout bounds a single /v1/embeddings call. The previous 30s
// default was too short for large memories on CPU embedders (~17k-token
// rows measured >90s CPU-only; S-212). The CUDA GPU path finishes well
// inside this bound; the timeout only guards against a wedged gateway.
const DefaultTimeout = 180 * time.Second

// NewClient validates the gateway base URL (admin-supplied config,
// never per-request input — same contract as the LiteLLM admin client)
// and returns a client bound to the given embedding model name.
func NewClient(baseURL, apiKey, model string) (*Client, error) {
	return NewClientWithTimeout(baseURL, apiKey, model, DefaultTimeout)
}

// NewClientWithTimeout is NewClient with an explicit per-call timeout.
func NewClientWithTimeout(baseURL, apiKey, model string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("embeddings: gateway URL %q must be an absolute http/https URL", baseURL)
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("embeddings: model name is required")
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  strings.TrimSpace(apiKey),
		model:   strings.TrimSpace(model),
		client:  &http.Client{Timeout: timeout},
	}, nil
}

// Model returns the configured embedding model name.
func (c *Client) Model() string { return c.model }

// Embed returns the embedding vector for text. A non-2xx response or a
// malformed payload is returned as an error; callers decide whether the
// operation is fatal (query time) or best-effort (write time).
func (c *Client) Embed(ctx context.Context, text string) ([]float64, error) {
	payload, err := json.Marshal(map[string]any{
		"model": c.model,
		"input": text,
	})
	if err != nil {
		return nil, fmt.Errorf("embeddings: marshal request: %w", err)
	}
	// #nosec G704 -- c.baseURL is validated admin-supplied config, not user input
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("embeddings: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	// #nosec G704 -- URL is the validated admin-configured gateway base + fixed
	// path; no user-controlled component reaches this request.
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embeddings: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embeddings: gateway returned %s: %s", resp.Status, snippet(resp.Body))
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embeddings: decode response: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embeddings: gateway returned no embedding vector")
	}
	return out.Data[0].Embedding, nil
}

func snippet(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 512))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
