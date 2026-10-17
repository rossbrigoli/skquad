// Package usage reports governed resource-call usage to the control
// plane (S-269 per-resource metering).
//
// One report per executed (governed-allowed) resource call:
//
//	POST {CP}/internal/v1/resource-usage {resource_id, resource_type, agent_id, count, timestamp}
//
// Same trust class as the confirmation client: the internal network
// policy is the fence, no app-layer auth. Unlike confirmation — which
// is fail-CLOSED because it gates execution — usage reporting is
// best-effort telemetry: a report failure must never block or fail
// the governed request path. The server-side helper fires the POST on
// a detached goroutine and swallows errors (the CP-side audit trail
// records what was ingested; a missed report is visible as a gap, and
// the CP never guesses usage).
package usage

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

// Report is one per-resource usage report. Count defaults to 1 per
// executed call; ResourceType is the registry resource type (rest,
// mcp, web, git, ...).
type Report struct {
	Timestamp    time.Time `json:"timestamp,omitempty"`
	AgentID      string    `json:"agent_id"`
	ResourceID   string    `json:"resource_id"`
	ResourceType string    `json:"resource_type,omitempty"`
	Count        int       `json:"count,omitempty"`
}

// Reporter sinks usage reports. Implementations must be safe for
// concurrent use.
type Reporter interface {
	Report(ctx context.Context, rep Report) error
}

// NopReporter discards reports (tests and deployments without a CP
// usage channel).
type NopReporter struct{}

func (NopReporter) Report(context.Context, Report) error { return nil }

// Client posts reports to the control plane's internal usage ingest.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a usage client against the CP base URL with the
// given per-request timeout.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Report synchronously POSTs one usage report. Callers on the request
// path should use it from a detached goroutine (see
// httpapi.Server.reportUsage) so telemetry latency/errors never leak
// into the governed call.
func (c *Client) Report(ctx context.Context, rep Report) error {
	if rep.ResourceID == "" || rep.AgentID == "" {
		return fmt.Errorf("usage report requires resource_id and agent_id")
	}
	if rep.Count == 0 {
		rep.Count = 1
	}
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/internal/v1/resource-usage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("usage ingest returned %s", resp.Status)
	}
	return nil
}
