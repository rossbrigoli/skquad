// S-155: provider API keys are stored as Kubernetes Secrets created by
// the control-plane, so platform admins paste the key in the UI instead
// of hand-creating a Secret. The providers table keeps only a
// k8s://<namespace>/<name> reference plus a display mask.

package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

// ProviderSecretKey is the data key holding the provider API key inside
// the managed Secret.
const ProviderSecretKey = "api-key"

// Core REST path fragments for the Secret API (S-189, sonar S1192).
const (
	apiPathNamespaces = "/api/v1/namespaces/"
	apiPathSecrets    = "/secrets/"
)

// SecretStore reads/writes provider API-key Secrets through the
// Kubernetes API (same raw-HTTP + projected-token pattern as CRWriter —
// the control-plane deliberately avoids client-go).
type SecretStore struct {
	baseURL   string
	namespace string
	token     string
	client    *http.Client
}

// NewSecretStore builds a SecretStore from the same K8s connection
// config the CRWriter uses.
func NewSecretStore(cfg *config.Config) (*SecretStore, error) {
	token, err := os.ReadFile(cfg.K8sTokenFile)
	if err != nil {
		return nil, fmt.Errorf("secretstore: read token: %w", err)
	}
	client, err := newK8sHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &SecretStore{
		baseURL:   strings.TrimRight(cfg.K8sAPIBase, "/"),
		namespace: cfg.K8sNamespace,
		token:     strings.TrimSpace(string(token)),
		client:    client,
	}, nil
}

// ProviderSecretName derives the managed Secret name for a provider id.
// Provider ids are UUIDs (DNS-1123 safe); we still sanitize defensively.
func ProviderSecretName(providerID string) string {
	return managedSecretName("skquad-provider-key-", providerID)
}

// ResourceSecretName derives the managed Secret name for a BYO REST
// resource id (TG-4). Same sanitization posture as provider keys.
func ResourceSecretName(resourceID string) string {
	return managedSecretName("skquad-rest-", resourceID)
}

// GitSecretName derives the managed Secret name for a BYO git
// resource id (TG-4b). The "skquad-git-" prefix keeps git custody
// Secrets disjoint from REST custody Secrets even if a resource id
// were ever reused across types.
func GitSecretName(resourceID string) string {
	return managedSecretName("skquad-git-", resourceID)
}

// MCPSecretName derives the managed Secret name for a BYO MCP resource
// id (TG-5 slice B2a). The "skquad-mcp-" prefix keeps MCP custody
// Secrets disjoint from the REST/git custody namespaces.
func MCPSecretName(resourceID string) string {
	return managedSecretName("skquad-mcp-", resourceID)
}

// agentSecretSuffix renders the per-agent marker shared by the REST
// and git per-(resource,agent) Secret names. Returns "" for agent
// ids that sanitize away to nothing.
func agentSecretSuffix(agentID string) string {
	suffix := strings.Trim(managedSecretName("agent-", agentID), "-")
	if suffix == "" || suffix == "agent" {
		return ""
	}
	return suffix
}

// ResourceAgentSecretName derives the managed Secret name for ONE
// agent's per-agent credential on a BYO REST resource (TG-4c, S-259).
//
// Design choice (S-259): per-(resource, agent) Secret rather than
// per-agent keys inside the resource Secret. The existing
// ResourceSecretStore interface (Ensure/Get/Delete by name) supports
// it unchanged, K8s resourceVersion conflict detection stays scoped to
// one credential (rotating agent A never contends with agent B), and
// deleting one agent's credential can't touch anyone else's material.
//
// The name is <resource-secret-name>-agent-<sanitized agent id>: the
// "-agent-" marker keeps per-agent names disjoint from any
// resource-level name (resource ids are UUIDs, so a resource can never
// itself be named "<uuid>-agent-<uuid>"). Returns "" when agentID
// sanitizes away to nothing — callers must treat that as invalid input
// rather than silently resolving to the resource-level Secret.
func ResourceAgentSecretName(resourceID, agentID string) string {
	suffix := agentSecretSuffix(agentID)
	if suffix == "" {
		return ""
	}
	name := ResourceSecretName(resourceID) + "-" + suffix
	if len(name) > 253 {
		// Defensive: real ids are UUIDs (~85 chars total). Truncation
		// can only collide for absurdly long resource ids.
		name = strings.TrimRight(name[:253], "-")
	}
	return name
}

// GitAgentSecretName derives the managed Secret name for ONE agent's
// per-agent credential on a BYO git resource (TG-4b). Same custody
// pattern as ResourceAgentSecretName (per-(resource,agent) Secret,
// disjoint from the resource-level Secret, conflict-scoped rotation)
// with the "skquad-git-" prefix. Returns "" when the agent id
// sanitizes away to nothing.
func GitAgentSecretName(resourceID, agentID string) string {
	suffix := agentSecretSuffix(agentID)
	if suffix == "" {
		return ""
	}
	name := GitSecretName(resourceID) + "-" + suffix
	if len(name) > 253 {
		name = strings.TrimRight(name[:253], "-")
	}
	return name
}

// MCPAgentSecretName derives the managed Secret name for ONE agent's
// per-agent credential on a BYO MCP resource (TG-5 slice B2a). Same
// per-(resource,agent) pattern as ResourceAgentSecretName / GitAgentSecretName,
// under the "skquad-mcp-" custody prefix.
func MCPAgentSecretName(resourceID, agentID string) string {
	suffix := agentSecretSuffix(agentID)
	if suffix == "" {
		return ""
	}
	name := MCPSecretName(resourceID) + "-" + suffix
	if len(name) > 253 {
		name = strings.TrimRight(name[:253], "-")
	}
	return name
}

func managedSecretName(prefix, id string) string {
	var b strings.Builder
	b.WriteString(prefix)
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(id)) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			continue
		}
		if r == '-' && lastDash {
			continue
		}
		b.WriteRune(r)
		lastDash = r == '-'
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

// RefFor builds the api_key_ref value stored for a managed secret.
func (s *SecretStore) RefFor(secretName string) string {
	return k8sRefPrefix + s.namespace + "/" + secretName
}

// IsManagedRef reports whether a ref points at a control-plane-managed
// provider key Secret.
func IsManagedRef(ref string) bool {
	return strings.HasPrefix(ref, k8sRefPrefix)
}

// EnsureProviderKey creates or replaces the provider key Secret. Update
// carries the live resourceVersion so concurrent writers conflict loudly
// instead of silently clobbering.
func (s *SecretStore) EnsureProviderKey(ctx context.Context, name, key string) error {
	existing, code, err := s.getRaw(ctx, name)
	if err != nil {
		return err
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": s.namespace,
			"labels":    map[string]string{managedByLabel: controlPlaneName},
		},
		"type":       "Opaque",
		"stringData": map[string]string{ProviderSecretKey: key},
	}
	if code == http.StatusNotFound {
		return s.send(ctx, http.MethodPost, apiPathNamespaces+s.namespace+"/secrets", secret, "create secret")
	}
	if meta, ok := existing["metadata"].(map[string]any); ok {
		if rv, ok := meta["resourceVersion"].(string); ok && rv != "" {
			secret["metadata"].(map[string]any)["resourceVersion"] = rv
		}
	}
	return s.send(ctx, http.MethodPut, apiPathNamespaces+s.namespace+apiPathSecrets+name, secret, "update secret")
}

// GetProviderKey returns the stored key for a managed Secret.
func (s *SecretStore) GetProviderKey(ctx context.Context, name string) (string, error) {
	secret, code, err := s.getRaw(ctx, name)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", fmt.Errorf("secretstore: secret %s not found", name)
	}
	data, _ := secret["data"].(map[string]any)
	raw, _ := data[ProviderSecretKey].(string)
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("secretstore: decode %s: %w", name, err)
	}
	return string(key), nil
}

// DeleteProviderKey removes the Secret; a missing Secret is not an error.
func (s *SecretStore) DeleteProviderKey(ctx context.Context, name string) error {
	return s.deleteSecret(ctx, name)
}

// EnsureResourceSecret creates or replaces a multi-field managed Secret
// for a BYO REST resource credential (TG-4). Fields are written as
// stringData; update carries the live resourceVersion so concurrent
// writers conflict loudly instead of silently clobbering.
func (s *SecretStore) EnsureResourceSecret(ctx context.Context, name string, fields map[string]string) error {
	existing, code, err := s.getRaw(ctx, name)
	if err != nil {
		return err
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": s.namespace,
			"labels":    map[string]string{managedByLabel: controlPlaneName},
		},
		"type":       "Opaque",
		"stringData": fields,
	}
	if code == http.StatusNotFound {
		return s.send(ctx, http.MethodPost, apiPathNamespaces+s.namespace+"/secrets", secret, "create resource secret")
	}
	if meta, ok := existing["metadata"].(map[string]any); ok {
		if rv, ok := meta["resourceVersion"].(string); ok && rv != "" {
			secret["metadata"].(map[string]any)["resourceVersion"] = rv
		}
	}
	return s.send(ctx, http.MethodPut, apiPathNamespaces+s.namespace+apiPathSecrets+name, secret, "update resource secret")
}

// GetResourceSecret returns every decoded string value of a managed
// resource Secret. Values are sensitive: callers must never log them.
func (s *SecretStore) GetResourceSecret(ctx context.Context, name string) (map[string]string, error) {
	secret, code, err := s.getRaw(ctx, name)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, fmt.Errorf("secretstore: secret %s not found", name)
	}
	data, _ := secret["data"].(map[string]any)
	out := make(map[string]string, len(data))
	for k, v := range data {
		raw, _ := v.(string)
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("secretstore: decode %s/%s: %w", name, k, err)
		}
		out[k] = string(decoded)
	}
	return out, nil
}

// DeleteResourceSecret removes a managed resource Secret; a missing
// Secret is not an error.
func (s *SecretStore) DeleteResourceSecret(ctx context.Context, name string) error {
	return s.deleteSecret(ctx, name)
}

func (s *SecretStore) deleteSecret(ctx context.Context, name string) error {
	req, err := s.request(ctx, http.MethodDelete, apiPathNamespaces+s.namespace+apiPathSecrets+name, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("secretstore: delete %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("secretstore: delete %s: HTTP %d", name, resp.StatusCode)
	}
	return nil
}

func (s *SecretStore) getRaw(ctx context.Context, name string) (map[string]any, int, error) {
	req, err := s.request(ctx, http.MethodGet, apiPathNamespaces+s.namespace+apiPathSecrets+name, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("secretstore: get %s: %w", name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("secretstore: read %s: %w", name, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return nil, resp.StatusCode, fmt.Errorf("secretstore: get %s: HTTP %d", name, resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("secretstore: parse %s: %w", name, err)
	}
	return out, resp.StatusCode, nil
}

func (s *SecretStore) send(ctx context.Context, method, path string, payload map[string]any, op string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("secretstore: marshal %s: %w", op, err)
	}
	req, err := s.request(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("secretstore: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("secretstore: %s: HTTP %d: %s", op, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (s *SecretStore) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("secretstore: build request: %w", err)
	}
	req.Header.Set("Authorization", bearerPrefix+s.token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}
