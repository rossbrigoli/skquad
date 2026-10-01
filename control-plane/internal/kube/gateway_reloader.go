// S-GWREG: rollout-restart helper for the LiteLLM gateway.
//
// The running litellm router does NOT pick up DB-persisted model
// deployment changes (store_model_in_db=true) until the gateway pod
// restarts (documented in docs/llm-gateway.md). Every control-plane
// mutation of a gateway model deployment must therefore trigger a
// rollout restart of the gateway Deployment. We do it the canonical
// kubectl way: a strategic-merge patch stamping a fresh timestamp onto
// the pod-template annotation, which changes the pod spec and makes the
// Deployment controller roll the pods.
//
// Same raw-HTTP + projected-token pattern as SecretStore/PodRestarter.

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

// GatewayReloadAnnotation is the pod-template annotation whose changing
// value triggers the rollout. Mirrors `kubectl rollout restart`.
const GatewayReloadAnnotation = "skquad.io/gateway-reload"

// GatewayReloader triggers rollout restarts of the LiteLLM gateway
// Deployment through the Kubernetes API.
type GatewayReloader struct {
	baseURL    string
	namespace  string
	deployment string
	token      string
	client     *http.Client
}

// NewGatewayReloader builds a GatewayReloader from the same K8s
// connection config the SecretStore/PodRestarter use. The deployment
// name comes from cfg.LLMGatewayDeployment (default skquad-llm-gateway).
func NewGatewayReloader(cfg *config.Config) (*GatewayReloader, error) {
	token, err := os.ReadFile(cfg.K8sTokenFile)
	if err != nil {
		return nil, fmt.Errorf("gatewayreloader: read token: %w", err)
	}
	client, err := newK8sHTTPClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("gatewayreloader: build client: %w", err)
	}
	return &GatewayReloader{
		baseURL:    strings.TrimRight(cfg.K8sAPIBase, "/"),
		namespace:  cfg.K8sNamespace,
		deployment: cfg.LLMGatewayDeployment,
		token:      strings.TrimSpace(string(token)),
		client:     client,
	}, nil
}

// ReloadGateway stamps the pod-template reload annotation with the
// current UTC timestamp, causing a rollout restart of the gateway
// Deployment. Requires RBAC: apps/deployments get+patch in the
// control-plane namespace (see charts api-server-rbac.yaml).
func (g *GatewayReloader) ReloadGateway(ctx context.Context) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						GatewayReloadAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("gatewayreloader: marshal patch: %w", err)
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", g.namespace, g.deployment)
	// #nosec G704 -- base URL and deployment name are admin config, not user input
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, g.baseURL+path, bytes.NewReader(patch))
	if err != nil {
		return fmt.Errorf("gatewayreloader: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("gatewayreloader: patch deployment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("gatewayreloader: patch deployment: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
