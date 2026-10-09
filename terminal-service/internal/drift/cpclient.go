package drift

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CPClient talks to the control-plane drift surface:
//
//	GET  /internal/v1/artifact-resources  → runner-facing resource list
//	POST /internal/v1/drift-reports       → store one drift report
//
// Auth is the direction-scoped bearer token (SKQUAD_DRIFT_INGEST_TOKEN).
// The token is never logged; error bodies are truncated before surfacing.
type CPClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client // injectable for tests; zero value gets a sane default
}

func (c *CPClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// ListArtifactResources fetches the artifact-enabled resource list.
func (c *CPClient) ListArtifactResources(ctx context.Context) ([]ArtifactResource, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/internal/v1/artifact-resources"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("drift: list artifact resources: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("drift: list artifact resources: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("drift: list artifact resources: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out struct {
		Resources []ArtifactResource `json:"resources"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("drift: list artifact resources: bad JSON: %w", err)
	}
	return out.Resources, nil
}

// PostDriftReport stores one report; returns the stored report id.
func (c *CPClient) PostDriftReport(ctx context.Context, r Report) (string, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/internal/v1/drift-reports"
	payload, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("drift: post report: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("drift: post report: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("drift: post report: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("drift: post report: bad JSON: %w", err)
	}
	return out.ID, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
