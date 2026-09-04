// Package kube mirrors control-plane domain state into Kubernetes custom
// resources consumed by the skquad operator.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// CRWriter writes Squad and Agent custom resources through the Kubernetes API.
type CRWriter struct {
	baseURL         string
	namespace       string
	groupVersion    string
	agentImage      string
	controlPlaneURL string
	llmGatewayURL   string
	token           string
	client          *http.Client
}

// NewCRWriter creates a Kubernetes REST writer from config.
func NewCRWriter(cfg *config.Config) (*CRWriter, error) {
	token, err := os.ReadFile(cfg.K8sTokenFile)
	if err != nil {
		return nil, fmt.Errorf("kube: read token: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.K8sInsecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit dev mode
	}
	return &CRWriter{
		baseURL:         strings.TrimRight(cfg.K8sAPIBase, "/"),
		namespace:       cfg.K8sNamespace,
		groupVersion:    cfg.K8sGroupVersion,
		agentImage:      cfg.AgentImage,
		controlPlaneURL: cfg.ControlPlaneURL,
		llmGatewayURL:   cfg.LLMGatewayURL,
		token:           strings.TrimSpace(string(token)),
		client:          &http.Client{Transport: transport},
	}, nil
}

func (w *CRWriter) UpsertSquad(ctx context.Context, squad *domain.Squad) error {
	body := map[string]any{
		"apiVersion": w.groupVersion,
		"kind":       "Squad",
		"metadata": map[string]any{
			"name":      squadCRName(squad.ID),
			"namespace": w.namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "skquad-control-plane",
				"skquad.io/squad-id":           squad.ID,
			},
		},
		"spec": map[string]any{
			"squadId":        squad.ID,
			"ownerRef":       squad.OwnerID,
			"namespace":      squad.Namespace,
			"mission":        squad.Mission,
			"operatingModel": rawJSON(squad.OperatingModel, map[string]any{}),
			"status":         squad.Status,
		},
	}
	return w.apply(ctx, "squads", squadCRName(squad.ID), body)
}

func (w *CRWriter) DeleteSquad(ctx context.Context, squad *domain.Squad) error {
	return w.delete(ctx, "squads", squadCRName(squad.ID))
}

func (w *CRWriter) UpsertAgent(ctx context.Context, agent *domain.Agent, identity *domain.AgentIdentity) error {
	spec := map[string]any{
		"agentId":           agent.ID,
		"squadId":           agent.SquadID,
		"role":              agent.Role,
		"systemPrompt":      agent.SystemPrompt,
		"defaultProviderId": agent.DefaultProvider,
		"defaultModel":      agent.DefaultModel,
		"image":             w.agentImage,
		"permissions":       rawJSON(agent.Permissions, []any{}),
		"idleTimeout":       fmt.Sprintf("%ds", agent.IdleTimeoutSec),
		"desiredActive":     agent.Status == domain.AgentBusy,
	}
	if w.controlPlaneURL != "" {
		spec["controlPlaneUrl"] = w.controlPlaneURL
	}
	if w.llmGatewayURL != "" {
		spec["llmGatewayUrl"] = w.llmGatewayURL
	}
	if identity != nil {
		if secretName := secretNameFromRef(identity.CredentialRef); secretName != "" {
			spec["credentialSecret"] = secretName
		}
		if secretName := secretNameFromRef(identity.VirtualKeyRef); secretName != "" {
			spec["virtualKeySecret"] = secretName
		}
	}
	body := map[string]any{
		"apiVersion": w.groupVersion,
		"kind":       "Agent",
		"metadata": map[string]any{
			"name":      agentCRName(agent.ID),
			"namespace": w.namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "skquad-control-plane",
				"skquad.io/agent-id":           agent.ID,
				"skquad.io/squad-id":           agent.SquadID,
			},
		},
		"spec": spec,
	}
	return w.apply(ctx, "agents", agentCRName(agent.ID), body)
}

func (w *CRWriter) DeleteAgent(ctx context.Context, agent *domain.Agent) error {
	return w.delete(ctx, "agents", agentCRName(agent.ID))
}

func (w *CRWriter) WriteAgentCredential(ctx context.Context, credentialRef string, agentID string, token string) error {
	namespace, name := secretTargetFromRef(credentialRef)
	if namespace == "" || name == "" {
		return fmt.Errorf("kube: invalid agent credential ref %q", credentialRef)
	}
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "skquad-control-plane",
				"skquad.io/agent-id":           agentID,
			},
		},
		"type": "Opaque",
		"data": map[string]string{
			"token": base64.StdEncoding.EncodeToString([]byte(token)),
		},
	}
	return w.applyCore(ctx, "secrets", namespace, name, body)
}

func (w *CRWriter) DeleteAgentCredential(ctx context.Context, credentialRef string) error {
	namespace, name := secretTargetFromRef(credentialRef)
	if namespace == "" || name == "" {
		return nil
	}
	return w.deleteCore(ctx, "secrets", namespace, name)
}

func (w *CRWriter) apply(ctx context.Context, plural, name string, body map[string]any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("kube: marshal %s/%s: %w", plural, name, err)
	}
	url := fmt.Sprintf("%s/apis/%s/namespaces/%s/%s/%s?fieldManager=skquad-control-plane&force=true",
		w.baseURL, w.groupVersion, w.namespace, plural, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("kube: build apply request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/apply-patch+yaml")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("kube: apply %s/%s: %w", plural, name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kube: apply %s/%s: %s: %s", plural, name, resp.Status, responseSnippet(resp.Body))
	}
	return nil
}

func (w *CRWriter) applyCore(ctx context.Context, plural, namespace, name string, body map[string]any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("kube: marshal %s/%s: %w", plural, name, err)
	}
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/%s/%s?fieldManager=skquad-control-plane&force=true",
		w.baseURL, namespace, plural, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("kube: build apply request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/apply-patch+yaml")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("kube: apply %s/%s: %w", plural, name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kube: apply %s/%s: %s: %s", plural, name, resp.Status, responseSnippet(resp.Body))
	}
	return nil
}

func (w *CRWriter) delete(ctx context.Context, plural, name string) error {
	url := fmt.Sprintf("%s/apis/%s/namespaces/%s/%s/%s", w.baseURL, w.groupVersion, w.namespace, plural, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("kube: build delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("kube: delete %s/%s: %w", plural, name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kube: delete %s/%s: %s: %s", plural, name, resp.Status, responseSnippet(resp.Body))
	}
	return nil
}

func (w *CRWriter) deleteCore(ctx context.Context, plural, namespace, name string) error {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/%s/%s", w.baseURL, namespace, plural, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("kube: build delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("kube: delete %s/%s: %w", plural, name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kube: delete %s/%s: %s: %s", plural, name, resp.Status, responseSnippet(resp.Body))
	}
	return nil
}

func rawJSON(raw json.RawMessage, fallback any) any {
	if len(raw) == 0 {
		return fallback
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return fallback
	}
	return out
}

func responseSnippet(r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, 2048))
	return strings.TrimSpace(string(body))
}

func squadCRName(id string) string {
	return "squad-" + id
}

func agentCRName(id string) string {
	return "agent-" + id
}

func secretNameFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "k8s://") {
		parts := strings.Split(strings.TrimPrefix(ref, "k8s://"), "/")
		if len(parts) >= 2 {
			return parts[len(parts)-1]
		}
		return ""
	}
	if strings.Contains(ref, "://") || strings.Contains(ref, "/") {
		return ""
	}
	return ref
}

func secretTargetFromRef(ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "k8s://") {
		return "", ""
	}
	parts := strings.Split(strings.TrimPrefix(ref, "k8s://"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}
