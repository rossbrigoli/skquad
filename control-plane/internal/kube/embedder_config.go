// S-212 (ADR-0013 §5): the control-plane writes the platform admin's
// embedder runtime choice into a well-known ConfigMap that the operator
// reads on every reconcile tick ("control-plane owns intent, operator
// reconciles"). Same raw-HTTP + projected-token pattern as SecretStore /
// CRWriter — the control-plane deliberately avoids client-go.
//
// The ConfigMap is created on first write and patched with a JSON merge
// patch afterwards, so concurrent admin changes converge on last-write-
// wins without resourceVersion conflicts (the value is a single scalar;
// the operator re-reads it live).
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
)

const (
	// EmbedderConfigMapName is the override ConfigMap the operator
	// watches (SKQUAD_EMBEDDER_RUNTIME_CONFIGMAP default).
	EmbedderConfigMapName = "skquad-embedder-config"
	// EmbedderRuntimeKey is the data key holding the runtime value
	// (SKQUAD_EMBEDDER_RUNTIME_KEY default).
	EmbedderRuntimeKey = "runtime"
)

// EmbedderConfigStore writes the embedder runtime override through the
// Kubernetes API in the control plane's release namespace.
type EmbedderConfigStore struct {
	baseURL   string
	namespace string
	token     string
	client    *http.Client
}

// NewEmbedderConfigStore builds the store from the same K8s connection
// config the CRWriter/SecretStore use.
func NewEmbedderConfigStore(cfg *config.Config) (*EmbedderConfigStore, error) {
	token, err := os.ReadFile(cfg.K8sTokenFile)
	if err != nil {
		return nil, fmt.Errorf("embedderconfig: read token: %w", err)
	}
	client, err := newK8sHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &EmbedderConfigStore{
		baseURL:   strings.TrimRight(cfg.K8sAPIBase, "/"),
		namespace: cfg.K8sNamespace,
		token:     strings.TrimSpace(string(token)),
		client:    client,
	}, nil
}

// SetEmbedderRuntime writes data.runtime into the override ConfigMap, creating
// the ConfigMap when it does not exist yet. Idempotent: re-setting the
// same value is a no-op patch.
func (s *EmbedderConfigStore) SetEmbedderRuntime(ctx context.Context, runtime string) error {
	runtime = strings.TrimSpace(runtime)
	if runtime == "" {
		return fmt.Errorf("embedderconfig: runtime value is empty")
	}
	path := "/api/v1/namespaces/" + s.namespace + "/configmaps/" + EmbedderConfigMapName

	// Does the ConfigMap already exist?
	getReq, err := s.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(getReq)
	if err != nil {
		return fmt.Errorf("embedderconfig: get %s: %w", EmbedderConfigMapName, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("embedderconfig: read %s: %w", EmbedderConfigMapName, err)
	}
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("embedderconfig: get %s: HTTP %d: %s", EmbedderConfigMapName, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if resp.StatusCode == http.StatusNotFound {
		create := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      EmbedderConfigMapName,
				"namespace": s.namespace,
				"labels": map[string]string{
					managedByLabel: controlPlaneName,
					"skquad.io/component": "embedder-runtime-override",
				},
			},
			"data": map[string]string{EmbedderRuntimeKey: runtime},
		}
		return s.send(ctx, http.MethodPost, "/api/v1/namespaces/"+s.namespace+"/configmaps", create, "create embedder config")
	}
	return s.send(ctx, http.MethodPatch, path, map[string]any{
		"data": map[string]string{EmbedderRuntimeKey: runtime},
	}, "patch embedder config")
}

func (s *EmbedderConfigStore) send(ctx context.Context, method, path string, payload map[string]any, op string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("embedderconfig: marshal %s: %w", op, err)
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
		return fmt.Errorf("embedderconfig: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("embedderconfig: %s: HTTP %d: %s", op, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (s *EmbedderConfigStore) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("embedderconfig: build request: %w", err)
	}
	req.Header.Set("Authorization", bearerPrefix+s.token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}
