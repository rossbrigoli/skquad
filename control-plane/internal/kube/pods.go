// Package kube: pod restart helper for S-162 (Restart Agent).
//
// The control-plane deletes the agent's pods directly through the
// Kubernetes API (same raw-HTTP + projected-token pattern as
// SecretStore/CRWriter). The owning Deployment recreates them, so a
// delete is a restart. Pods are found by the skquad.io/agent-id label
// the operator stamps on every agent workload.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

// LabelAgentID mirrors operator/api/v1.LabelAgentID. Kept as a literal
// here to avoid importing the operator module from the control-plane.
const LabelAgentID = "skquad.io/agent-id"

// PodRestarter deletes agent pods by label through the Kubernetes API.
type PodRestarter struct {
	baseURL   string
	namespace string
	token     string
	client    *http.Client
}

// NewPodRestarter builds a PodRestarter from the same K8s connection
// config the CRWriter/SecretStore use.
func NewPodRestarter(cfg *config.Config) (*PodRestarter, error) {
	token, err := os.ReadFile(cfg.K8sTokenFile)
	if err != nil {
		return nil, fmt.Errorf("podrestarter: read token: %w", err)
	}
	client, err := newK8sHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &PodRestarter{
		baseURL:   strings.TrimRight(cfg.K8sAPIBase, "/"),
		namespace: cfg.K8sNamespace,
		token:     strings.TrimSpace(string(token)),
		client:    client,
	}, nil
}

// RestartAgentPods deletes all pods labeled with the given agent id in
// the control-plane namespace and returns how many were deleted. A 404
// from the collection delete (no matching pods) is reported as 0, not
// an error.
func (p *PodRestarter) RestartAgentPods(ctx context.Context, agentID string) (int, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s%%3D%s",
		p.namespace, LabelAgentID, agentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("podrestarter: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("podrestarter: delete pods: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("podrestarter: delete pods: HTTP %d", resp.StatusCode)
	}
	var status struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return 0, fmt.Errorf("podrestarter: decode response: %w", err)
	}
	return len(status.Items), nil
}
