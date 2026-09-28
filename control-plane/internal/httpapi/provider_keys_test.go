// S-155 tests: pasted provider API keys land in a managed Kubernetes
// Secret; reads expose only api_key_masked + has_api_key and never the
// key or the ref. Covers create/patch/clear/replace, legacy api_key_ref
// aliasing, delete cleanup, and the startup migration.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"github.com/stretchr/testify/require"
)

// fakeKeyStore is an in-memory stand-in for kube.SecretStore.
type fakeKeyStore struct {
	secrets map[string]string
	deletes int
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{secrets: map[string]string{}}
}

func (f *fakeKeyStore) EnsureProviderKey(_ context.Context, name, key string) error {
	f.secrets[name] = key
	return nil
}

func (f *fakeKeyStore) GetProviderKey(_ context.Context, name string) (string, error) {
	v, ok := f.secrets[name]
	if !ok {
		return "", fmt.Errorf("fake: secret %s not found", name)
	}
	return v, nil
}

func (f *fakeKeyStore) DeleteProviderKey(_ context.Context, name string) error {
	f.deletes++
	delete(f.secrets, name)
	return nil
}

func (f *fakeKeyStore) RefFor(secretName string) string {
	return "k8s://skquad-system/" + secretName
}

// failStore rejects every EnsureProviderKey (rollback path).
type failStore struct{ fakeKeyStore }

func (f *failStore) EnsureProviderKey(_ context.Context, name, _ string) error {
	return fmt.Errorf("fake: always fails (%s)", name)
}

func rawProviderPost(t *testing.T, handler http.Handler, body map[string]any) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/registry/llm-providers", bytes.NewReader(payload))
	req.Header.Set(headerContentType, jsonContentType)
	req.Header.Set("Authorization", authAdmin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func rawProviderPatch(t *testing.T, handler http.Handler, id string, body map[string]any) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/registry/llm-providers/"+id, bytes.NewReader(payload))
	req.Header.Set(headerContentType, jsonContentType)
	req.Header.Set("Authorization", authAdmin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestProviderKeyCreateStoresSecretAndMasks(t *testing.T) {
	keys := newFakeKeyStore()
	handler := NewWithProviderKeyStore(testConfig(), storage.NewMemoryStore(), keys)

	code, p := rawProviderPost(t, handler, map[string]any{
		"name":     "openai",
		"kind":     "openai",
		"base_url": "https://api.openai.com/v1",
		"api_key":  "sk-" + "super" + "secret-xyz78",
	})
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "•••••xyz78", p["api_key_masked"])
	require.Equal(t, true, p["has_api_key"])
	_, hasRef := p["api_key_ref"]
	require.False(t, hasRef, "api_key_ref must never be serialized")
	require.NotContains(t, fmt.Sprint(p), "supersecret")

	secretName := providerSecretName(p["id"].(string))
	require.Equal(t, "sk-"+"super"+"secret-xyz78", keys.secrets[secretName])
}

func TestProviderKeyCreateWithoutKey(t *testing.T) {
	keys := newFakeKeyStore()
	handler := NewWithProviderKeyStore(testConfig(), storage.NewMemoryStore(), keys)
	code, p := rawProviderPost(t, handler, map[string]any{
		"name": "local", "kind": "ollama", "base_url": "http://localhost:11434/v1",
	})
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, false, p["has_api_key"])
	require.Equal(t, "", p["api_key_masked"])
	require.Empty(t, keys.secrets)
}

func TestProviderKeyLegacyRefTreatedAsPastedKey(t *testing.T) {
	keys := newFakeKeyStore()
	handler := NewWithProviderKeyStore(testConfig(), storage.NewMemoryStore(), keys)
	code, p := rawProviderPost(t, handler, map[string]any{
		"name": "legacy", "kind": "openai", "base_url": "https://x.test/v1",
		"api_key_ref": "sk-legacy-abcde",
	})
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, true, p["has_api_key"])
	require.Equal(t, "•••••abcde", p["api_key_masked"])
	require.Contains(t, keys.secrets, providerSecretName(p["id"].(string)))
}

func TestProviderKeyPatchKeepReplaceClear(t *testing.T) {
	keys := newFakeKeyStore()
	handler := NewWithProviderKeyStore(testConfig(), storage.NewMemoryStore(), keys)
	_, p := rawProviderPost(t, handler, map[string]any{
		"name": "p", "kind": "openai", "base_url": "https://x.test/v1", "api_key": "sk-11111" + "2345",
	})
	id := p["id"].(string)
	secretName := providerSecretName(id)

	// Omitted → keep.
	code, kept := rawProviderPatch(t, handler, id, map[string]any{"name": "p2"})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "•••••12345", kept["api_key_masked"])
	require.Equal(t, "sk-111112345", keys.secrets[secretName])

	// Replace.
	code, rep := rawProviderPatch(t, handler, id, map[string]any{"api_key": "sk-99999" + "67890"})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "•••••67890", rep["api_key_masked"])
	require.Equal(t, "sk-9999967890", keys.secrets[secretName])

	// Clear.
	code, cleared := rawProviderPatch(t, handler, id, map[string]any{"api_key": ""})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, false, cleared["has_api_key"])
	require.NotContains(t, keys.secrets, secretName)
}

func TestProviderKeyListNeverLeaks(t *testing.T) {
	keys := newFakeKeyStore()
	handler := NewWithProviderKeyStore(testConfig(), storage.NewMemoryStore(), keys)
	rawProviderPost(t, handler, map[string]any{
		"name": "s", "kind": "openai", "base_url": "https://x.test/v1", "api_key": "sk-" + "top" + "secret",
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/registry/llm-providers", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "topsecret")
	require.NotContains(t, rec.Body.String(), "api_key_ref")
	require.Contains(t, rec.Body.String(), "•••••ecret")
}

func TestProviderKeyDeleteRemovesSecret(t *testing.T) {
	keys := newFakeKeyStore()
	handler := NewWithProviderKeyStore(testConfig(), storage.NewMemoryStore(), keys)
	_, p := rawProviderPost(t, handler, map[string]any{
		"name": "d", "kind": "openai", "base_url": "https://x.test/v1", "api_key": "sk-" + "del" + "ete-12345",
	})
	id := p["id"].(string)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/registry/llm-providers/"+id, nil)
	req.Header.Set("Authorization", authAdmin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotContains(t, keys.secrets, providerSecretName(id))
	require.Equal(t, 1, keys.deletes)
}

func TestProviderKeyCreateRollbackOnSecretFailure(t *testing.T) {
	handler := NewWithProviderKeyStore(testConfig(), storage.NewMemoryStore(), &failStore{})
	code, _ := rawProviderPost(t, handler, map[string]any{
		"name": "boom", "kind": "openai", "base_url": "https://x.test/v1", "api_key": "sk-" + "boom" + "-12345",
	})
	require.Equal(t, http.StatusBadGateway, code)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/registry/llm-providers", nil))
	require.NotContains(t, rec.Body.String(), "boom")
}

func TestMigrateLegacyProviderKeys(t *testing.T) {
	store := storage.NewMemoryStore()
	keys := newFakeKeyStore()
	ctx := context.Background()

	lit, err := store.CreateLLMProvider(ctx, &domain.LLMProvider{
		Name: "lit", Kind: "openai", BaseURL: "https://x.test/v1", APIKeyRef: "sk-literal-abcdef",
	})
	require.NoError(t, err)
	_, err = store.CreateLLMProvider(ctx, &domain.LLMProvider{
		Name: "keyless", Kind: "ollama", BaseURL: "http://x.test/v1",
	})
	require.NoError(t, err)

	wrapped, err := MigrateLegacyProviderKeys(ctx, store, keys)
	require.NoError(t, err)
	require.Equal(t, 1, wrapped)

	got, err := store.GetLLMProvider(ctx, lit.ID)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got.APIKeyRef, "k8s://"))
	require.Equal(t, "•••••bcdef", got.APIKeyMask)
	require.Equal(t, "sk-literal-abcdef", keys.secrets[providerSecretName(lit.ID)])

	// Idempotent: second run wraps nothing.
	wrapped2, err := MigrateLegacyProviderKeys(ctx, store, keys)
	require.NoError(t, err)
	require.Equal(t, 0, wrapped2)
}

func TestMaskProviderKey(t *testing.T) {
	require.Equal(t, "", maskProviderKey(""))
	require.Equal(t, "•••", maskProviderKey("abc"))
	require.Equal(t, "•••••", maskProviderKey("abcde"))
	require.Equal(t, "•••••bcdef", maskProviderKey("abcdef"))
}
