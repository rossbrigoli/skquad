package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

func TestSecretNameFromRef(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"k8s://squad-test/agent-credential":       "agent-credential",
		"k8s://squad-test/agent-virtual-key":      "agent-virtual-key",
		"plain-secret":                            "plain-secret",
		"llm-gateway://virtual-keys/legacy-agent": "",
		"squad-test/agent-credential":             "",
		"":                                        "",
	}
	for ref, want := range tests {
		if got := secretNameFromRef(ref); got != want {
			t.Fatalf("secretNameFromRef(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestSecretTargetFromRef(t *testing.T) {
	t.Parallel()

	namespace, name := secretTargetFromRef("k8s://squad-test/agent-credential")
	if namespace != "squad-test" || name != "agent-credential" {
		t.Fatalf("secret target = %q/%q, want squad-test/agent-credential", namespace, name)
	}

	namespace, name = secretTargetFromRef("llm-gateway://virtual-keys/agent")
	if namespace != "" || name != "" {
		t.Fatalf("non-Kubernetes ref target = %q/%q, want empty", namespace, name)
	}
}

func TestUpsertAgentMapsGeneratedSecretRefs(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	writer := &CRWriter{
		baseURL:      server.URL,
		namespace:    "skquad-system",
		groupVersion: "skquad.io/v1",
		agentImage:   "example.com/skquad/agent-runtime:test",
		token:        "test-token",
		client:       server.Client(),
	}
	agent := &domain.Agent{
		ID:              "agent-1",
		SquadID:         "squad-1",
		Role:            "coder",
		DefaultProvider: "11111111-1111-1111-1111-111111111111",
		DefaultModel:    "openai/gpt-4o-mini",
		IdleTimeoutSec:  300,
	}
	identity := &domain.AgentIdentity{
		AgentID:       agent.ID,
		CredentialRef: "k8s://squad-test/agent-agent-1-credential-abcd1234",
		VirtualKeyRef: "k8s://squad-test/agent-agent-1-virtual-key-efgh5678",
	}

	if err := writer.UpsertAgent(context.Background(), agent, identity); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/apis/skquad.io/v1/namespaces/skquad-system/agents/agent-agent-1?fieldManager=skquad-control-plane&force=true" {
		t.Fatalf("path = %q", gotPath)
	}
	spec := gotBody["spec"].(map[string]any)
	if got := spec["credentialSecret"]; got != "agent-agent-1-credential-abcd1234" {
		t.Fatalf("credentialSecret = %q", got)
	}
	if got := spec["virtualKeySecret"]; got != "agent-agent-1-virtual-key-efgh5678" {
		t.Fatalf("virtualKeySecret = %q", got)
	}
	if got := spec["defaultProviderId"]; got != agent.DefaultProvider {
		t.Fatalf("defaultProviderId = %q, want %q", got, agent.DefaultProvider)
	}
	if got := spec["defaultModel"]; got != agent.DefaultModel {
		t.Fatalf("defaultModel = %q, want %q", got, agent.DefaultModel)
	}
}

func TestUpsertAgentEmitsWorkspaceSecrets(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	writer := &CRWriter{
		baseURL:      server.URL,
		namespace:    "skquad-system",
		groupVersion: "skquad.io/v1",
		agentImage:   "example.com/skquad/agent-runtime:test",
		token:        "test-token",
		client:       server.Client(),
	}

	agent := &domain.Agent{ID: "agent-ws", SquadID: "squad-1", Role: "coder", IdleTimeoutSec: 300}
	if err := writer.UpsertAgent(context.Background(), agent, nil); err != nil {
		t.Fatal(err)
	}
	spec := gotBody["spec"].(map[string]any)
	if _, ok := spec["workspaceSecrets"]; ok {
		t.Fatalf("workspaceSecrets should be omitted when empty, got %v", spec["workspaceSecrets"])
	}

	agent.WorkspaceSecrets = []domain.WorkspaceSecret{
		{ResourceID: "ws-1", SecretName: "ws-repo-app-token"},
		{ResourceID: "ws-2", SecretName: "ws-repo-api-token"},
	}
	if err := writer.UpsertAgent(context.Background(), agent, nil); err != nil {
		t.Fatal(err)
	}
	spec = gotBody["spec"].(map[string]any)
	raw, ok := spec["workspaceSecrets"].([]any)
	if !ok || len(raw) != 2 {
		t.Fatalf("workspaceSecrets = %v, want 2 entries", spec["workspaceSecrets"])
	}
	first := raw[0].(map[string]any)
	if first["resourceId"] != "ws-1" || first["secretName"] != "ws-repo-app-token" {
		t.Fatalf("first entry = %v", first)
	}
}

func TestWriteAgentCredentialAppliesOpaqueSecret(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		if r.Method != http.MethodPatch {
			t.Fatalf("method = %s, want PATCH", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q, want bearer token", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	writer := &CRWriter{
		baseURL: server.URL,
		token:   "test-token",
		client:  server.Client(),
	}

	if err := writer.WriteAgentCredential(
		context.Background(),
		"k8s://squad-test/agent-credential",
		"agent-1",
		"runtime-token",
	); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/api/v1/namespaces/squad-test/secrets/agent-credential?fieldManager=skquad-control-plane&force=true" {
		t.Fatalf("path = %q", gotPath)
	}
	data := gotBody["data"].(map[string]any)
	if got := data["token"]; got != base64.StdEncoding.EncodeToString([]byte("runtime-token")) {
		t.Fatalf("encoded token = %q", got)
	}
}

func TestNewCRWriterTrustsProvidedCAFile(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(dir, "ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	writer, err := NewCRWriter(&config.Config{
		K8sAPIBase:      server.URL,
		K8sNamespace:    "skquad-system",
		K8sTokenFile:    tokenFile,
		K8sCAFile:       caFile,
		K8sGroupVersion: "skquad.io/v1",
	})
	if err != nil {
		t.Fatalf("NewCRWriter: %v", err)
	}
	// With the provided CA the TLS handshake must succeed against the
	// httptest server (whose CA is absent from the system trust store).
	if err := writer.UpsertSquad(context.Background(), &domain.Squad{ID: "squad-1", Namespace: "squad-test"}); err != nil {
		t.Fatalf("UpsertSquad over provided CA: %v", err)
	}
}

func TestNewCRWriterRejectsUnparsableCAFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewCRWriter(&config.Config{
		K8sAPIBase:      "https://kubernetes.default.svc",
		K8sNamespace:    "skquad-system",
		K8sTokenFile:    tokenFile,
		K8sCAFile:       caFile,
		K8sGroupVersion: "skquad.io/v1",
	})
	if err == nil {
		t.Fatal("expected NewCRWriter to reject a CA file with no certificates")
	}
}

func TestNewCRWriterMissingCAFileFallsBackToSystemPool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewCRWriter(&config.Config{
		K8sAPIBase:      "https://kubernetes.default.svc",
		K8sNamespace:    "skquad-system",
		K8sTokenFile:    tokenFile,
		K8sCAFile:       filepath.Join(dir, "absent-ca.crt"),
		K8sGroupVersion: "skquad.io/v1",
	}); err != nil {
		t.Fatalf("missing CA file should not fail construction: %v", err)
	}
}

// WP5 (ADR-0010 D4): the Agent CR must carry the model binding ids, and
// defaultModel must prefer the resolved bound AI Model name over the
// legacy free-text column.
func TestUpsertAgentEmitsModelBindingFields(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	writer := &CRWriter{
		baseURL:      server.URL,
		namespace:    "skquad-system",
		groupVersion: "skquad.io/v1",
		agentImage:   "example.com/skquad/agent-runtime:test",
		token:        "test-token",
		client:       server.Client(),
	}

	agent := &domain.Agent{
		ID:                  "agent-bind",
		SquadID:             "squad-1",
		Role:                "coder",
		DefaultProvider:     "legacy-provider-id",
		DefaultModel:        "legacy-model",
		AIModelID:           "ai-primary-uuid",
		FallbackAIModelID:   "ai-fallback-uuid",
		AIModelName:         "gpt-6-sol",
		FallbackAIModelName: "halogen/qwen3.8-flash-next",
		IdleTimeoutSec:      300,
	}
	if err := writer.UpsertAgent(context.Background(), agent, nil); err != nil {
		t.Fatal(err)
	}

	spec := gotBody["spec"].(map[string]any)
	if got := spec["aiModelId"]; got != "ai-primary-uuid" {
		t.Fatalf("aiModelId = %v, want ai-primary-uuid", got)
	}
	if got := spec["fallbackAiModelId"]; got != "ai-fallback-uuid" {
		t.Fatalf("fallbackAiModelId = %v, want ai-fallback-uuid", got)
	}
	// Resolved binding name wins over the legacy free-text default.
	if got := spec["defaultModel"]; got != "gpt-6-sol" {
		t.Fatalf("defaultModel = %v, want gpt-6-sol", got)
	}
}

func TestUpsertAgentDefaultModelFallsBackToLegacyWhenBindingUnresolved(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	writer := &CRWriter{
		baseURL:      server.URL,
		namespace:    "skquad-system",
		groupVersion: "skquad.io/v1",
		agentImage:   "example.com/skquad/agent-runtime:test",
		token:        "test-token",
		client:       server.Client(),
	}

	agent := &domain.Agent{
		ID:             "agent-legacy",
		SquadID:        "squad-1",
		Role:           "coder",
		DefaultModel:   "legacy-model",
		AIModelID:      "ai-unresolved-uuid",
		IdleTimeoutSec: 300,
	}
	if err := writer.UpsertAgent(context.Background(), agent, nil); err != nil {
		t.Fatal(err)
	}

	spec := gotBody["spec"].(map[string]any)
	if got := spec["defaultModel"]; got != "legacy-model" {
		t.Fatalf("defaultModel = %v, want legacy-model (unresolved binding must not blank the field)", got)
	}
	if got := spec["aiModelId"]; got != "ai-unresolved-uuid" {
		t.Fatalf("aiModelId = %v, want ai-unresolved-uuid", got)
	}
}
