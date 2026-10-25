// S-287: the control-plane writes the platform admin's agent pod
// resource choices into a well-known ConfigMap that the operator reads
// on every reconcile ("control-plane owns intent, operator
// reconciles"). Same raw-HTTP + projected-token pattern as
// EmbedderConfigStore (S-212) — the control-plane deliberately avoids
// client-go.
//
// The ConfigMap is created on first write and patched with a JSON merge
// patch afterwards, so concurrent admin changes converge on
// last-write-wins without resourceVersion conflicts (the operator
// re-reads all four keys live).
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

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

const (
	// AgentResourcesConfigMapName is the override ConfigMap the
	// operator watches (SKQUAD_AGENT_RESOURCES_CONFIGMAP default).
	AgentResourcesConfigMapName = "skquad-agent-resources"
	// Data keys holding the four resource quantities. These match the
	// operator's reader (controller.AgentResourceConfigMapKey*).
	AgentResourcesKeyCPURequest    = "cpu_request"
	AgentResourcesKeyCPULimit      = "cpu_limit"
	AgentResourcesKeyMemoryRequest = "memory_request"
	AgentResourcesKeyMemoryLimit   = "memory_limit"
)

// AgentResourcesConfigStore writes the agent resource override
// ConfigMap through the Kubernetes API in the control plane's release
// namespace.
type AgentResourcesConfigStore struct {
	baseURL   string
	namespace string
	token     string
	client    *http.Client
}

// NewAgentResourcesConfigStore builds the store from the same K8s
// connection config the CRWriter/SecretStore use.
func NewAgentResourcesConfigStore(cfg *config.Config) (*AgentResourcesConfigStore, error) {
	token, err := os.ReadFile(cfg.K8sTokenFile)
	if err != nil {
		return nil, fmt.Errorf("agentresourcesconfig: read token: %w", err)
	}
	client, err := newK8sHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &AgentResourcesConfigStore{
		baseURL:   strings.TrimRight(cfg.K8sAPIBase, "/"),
		namespace: cfg.K8sNamespace,
		token:     strings.TrimSpace(string(token)),
		client:    client,
	}, nil
}

// SetAgentResources writes the four resource quantities into the
// override ConfigMap, creating the ConfigMap when it does not exist
// yet. Idempotent: re-setting the same values is a no-op patch.
func (s *AgentResourcesConfigStore) SetAgentResources(ctx context.Context, res domain.AgentResourceSettings) error {
	data := map[string]string{
		AgentResourcesKeyCPURequest:    res.CPURequest,
		AgentResourcesKeyCPULimit:      res.CPULimit,
		AgentResourcesKeyMemoryRequest: res.MemoryRequest,
		AgentResourcesKeyMemoryLimit:   res.MemoryLimit,
	}
	for k, v := range data {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("agentresourcesconfig: %s is empty", k)
		}
	}
	path := "/api/v1/namespaces/" + s.namespace + "/configmaps/" + AgentResourcesConfigMapName

	getReq, err := s.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(getReq)
	if err != nil {
		return fmt.Errorf("agentresourcesconfig: get %s: %w", AgentResourcesConfigMapName, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("agentresourcesconfig: read %s: %w", AgentResourcesConfigMapName, err)
	}
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("agentresourcesconfig: get %s: HTTP %d: %s", AgentResourcesConfigMapName, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if resp.StatusCode == http.StatusNotFound {
		create := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      AgentResourcesConfigMapName,
				"namespace": s.namespace,
				"labels": map[string]string{
					managedByLabel:        controlPlaneName,
					"skquad.io/component": "agent-resources-override",
				},
			},
			"data": data,
		}
		return s.send(ctx, http.MethodPost, "/api/v1/namespaces/"+s.namespace+"/configmaps", create, "create agent resources config")
	}
	return s.send(ctx, http.MethodPatch, path, map[string]any{"data": data}, "patch agent resources config")
}

func (s *AgentResourcesConfigStore) send(ctx context.Context, method, path string, payload map[string]any, op string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("agentresourcesconfig: marshal %s: %w", op, err)
	}
	contentType := "application/json"
	if method == http.MethodPatch {
		contentType = "application/merge-patch+json"
	}
	req, err := s.request(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("agentresourcesconfig: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("agentresourcesconfig: %s: HTTP %d: %s", op, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (s *AgentResourcesConfigStore) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("agentresourcesconfig: build request: %w", err)
	}
	req.Header.Set("Authorization", bearerPrefix+s.token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}
