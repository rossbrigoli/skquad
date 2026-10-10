// TG-4 (S-250) tests: BYO credential custody + the internal
// credentials read API. Fake token values only; nothing here may
// contain a real-looking secret.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

const byoFakeToken = "test-token-DO-NOT-USE"

// fakeResourceSecretStore is an in-memory stand-in for kube.SecretStore.
type fakeResourceSecretStore struct {
	mu         sync.Mutex
	secrets    map[string]map[string]string
	failEnsure bool
	failSweep  bool
	deletes    []string
}

func newFakeResourceSecretStore() *fakeResourceSecretStore {
	return &fakeResourceSecretStore{secrets: map[string]map[string]string{}}
}

func (f *fakeResourceSecretStore) EnsureResourceSecret(_ context.Context, name string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEnsure {
		return errors.New("fake: ensure failed")
	}
	cp := make(map[string]string, len(fields))
	for k, v := range fields {
		cp[k] = v
	}
	f.secrets[name] = cp
	return nil
}

func (f *fakeResourceSecretStore) GetResourceSecret(_ context.Context, name string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.secrets[name]
	if !ok {
		return nil, errors.New("fake: not found")
	}
	return v, nil
}

func (f *fakeResourceSecretStore) DeleteResourceSecret(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, name)
	f.deletes = append(f.deletes, name)
	return nil
}

func (f *fakeResourceSecretStore) RefFor(secretName string) string {
	return "k8s://skquad-system/" + secretName
}

// DeleteSecretsByPrefix mirrors kube.SecretStore's S-260 sweep: a name
// matches when it equals prefix or starts with prefix+"-". Matches are
// removed and recorded in deleted order.
func (f *fakeResourceSecretStore) DeleteSecretsByPrefix(_ context.Context, prefix string) (int, error) {
	if f.failSweep {
		return 0, errors.New("fake: sweep failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	dash := prefix + "-"
	var matched []string
	for name := range f.secrets {
		if name == prefix || strings.HasPrefix(name, dash) {
			matched = append(matched, name)
		}
	}
	slices.Sort(matched) // deterministic delete order for assertions
	for _, name := range matched {
		delete(f.secrets, name)
		f.deletes = append(f.deletes, name)
	}
	return len(matched), nil
}

func (f *fakeResourceSecretStore) set(name string, fields map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]string, len(fields))
	for k, v := range fields {
		cp[k] = v
	}
	f.secrets[name] = cp
}

func (f *fakeResourceSecretStore) get(name string) (map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.secrets[name]
	return v, ok
}

func (f *fakeResourceSecretStore) deletedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.deletes...)
}

func byoBearerBody(name string) map[string]any {
	return map[string]any{
		"name":            name,
		"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "bearer"},
		"policy_ceiling":  map[string]any{"methods": []string{"GET"}, "path_allow": []string{"/issues/**"}},
		"auth":            map[string]string{"token": byoFakeToken},
	}
}

func createBYORestResource(t *testing.T, handler http.Handler, name string) domain.RegistryResource {
	t.Helper()
	var res domain.RegistryResource
	doJSON(t, handler, http.MethodPost, registryBase+"rest", byoBearerBody(name), http.StatusCreated, &res)
	require.NotEmpty(t, res.ID)
	return res
}

// ── Internal credentials endpoint ────────────────────────────────────────

func TestInternalCredentialsServesBYOSecret(t *testing.T) {
	store := storage.NewMemoryStore()
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "byo-1")

	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource="+res.ID, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var out struct {
		ResourceID string            `json:"resource_id"`
		Kind       string            `json:"kind"`
		Fields     map[string]string `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, res.ID, out.ResourceID)
	require.Equal(t, "bearer", out.Kind)
	require.Equal(t, byoFakeToken, out.Fields["token"])
}

func TestInternalCredentialsMissingParam(t *testing.T) {
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInternalCredentialsUnknownResource(t *testing.T) {
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource=00000000-0000-0000-0000-000000000000", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestInternalCredentialsWrongTypeDenied(t *testing.T) {
	store := storage.NewMemoryStore()
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	var web domain.RegistryResource
	doJSON(t, handler, http.MethodPost, registryBase+"web",
		map[string]any{"name": "web-byo", "auth_ref": "k8s://skquad-system/somewhere"},
		http.StatusCreated, &web)
	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource="+web.ID, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "non-rest resources must never serve credentials")
}

func TestInternalCredentialsAuthNoneDenied(t *testing.T) {
	store := storage.NewMemoryStore()
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	var res domain.RegistryResource
	doJSON(t, handler, http.MethodPost, registryBase+"rest",
		map[string]any{
			"name":            "open-api",
			"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "none"},
		},
		http.StatusCreated, &res)
	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource="+res.ID, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestInternalCredentialsMissingSecretUnavailable(t *testing.T) {
	store := storage.NewMemoryStore()
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: secrets})
	// Operator-style resource: auth_ref set by hand, no managed secret.
	var res domain.RegistryResource
	doJSON(t, handler, http.MethodPost, registryBase+"rest",
		map[string]any{
			"name":            "ghost-api",
			"auth_ref":        "k8s://skquad-system/never-created",
			"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "bearer"},
		},
		http.StatusCreated, &res)
	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource="+res.ID, "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotContains(t, rec.Body.String(), byoFakeToken)
}

func TestInternalCredentialsNoStoreConfigured(t *testing.T) {
	handler := New(testConfig(), storage.NewMemoryStore())
	var res domain.RegistryResource
	doJSON(t, handler, http.MethodPost, registryBase+"rest",
		map[string]any{
			"name":            "nostore-api",
			"auth_ref":        "k8s://skquad-system/x",
			"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "bearer"},
		},
		http.StatusCreated, &res)
	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource="+res.ID, "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestInternalCredentialsAuthClassParityWithPolicy(t *testing.T) {
	// Same trust class: neither endpoint applies app-layer auth —
	// reachability is the network's job (TG-1 contract).
	store := storage.NewMemoryStore()
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: newFakeResourceSecretStore()})
	res := createBYORestResource(t, handler, "parity-api")

	pol := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy?agent=00000000-0000-0000-0000-000000000000", "")
	cred := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource="+res.ID, "")
	for _, rec := range []*httptest.ResponseRecorder{pol, cred} {
		require.NotEqual(t, http.StatusUnauthorized, rec.Code, "internal endpoints must not add app-layer auth")
		require.NotEqual(t, http.StatusForbidden, rec.Code, "internal endpoints must not add app-layer auth")
	}
	require.Equal(t, http.StatusNotFound, pol.Code) // unknown agent
	require.Equal(t, http.StatusOK, cred.Code)
}

// ── TG-4c (S-259): per-agent resolution ────────────────────────────────

func resolveCreds(t *testing.T, handler http.Handler, resourceID, agentID string) (int, struct {
	ResourceID string            `json:"resource_id"`
	Kind       string            `json:"kind"`
	Fields     map[string]string `json:"fields"`
	Scope      string            `json:"scope"`
}) {
	t.Helper()
	url := "/internal/v1/credentials?resource=" + resourceID
	if agentID != "" {
		url += "&agent=" + agentID
	}
	rec := doRawNoAuth(t, handler, http.MethodGet, url, "")
	var out struct {
		ResourceID string            `json:"resource_id"`
		Kind       string            `json:"kind"`
		Fields     map[string]string `json:"fields"`
		Scope      string            `json:"scope"`
	}
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec.Code, out
}

func TestInternalCredentialsPerAgentWinsOverDefault(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "pa-win-api")
	agentA := "11111111-1111-4111-8111-111111111111"
	secrets.set(kube.ResourceAgentSecretName(res.ID, agentA), map[string]string{"token": "fake-pat-agentA-DO-NOT-USE"})

	code, out := resolveCreds(t, handler, res.ID, agentA)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "agent", out.Scope)
	require.Equal(t, "fake-pat-agentA-DO-NOT-USE", out.Fields["token"])
	require.NotContains(t, out.Fields["token"], byoFakeToken)

	// Resource-only call keeps serving the default (backwards compat).
	code, out = resolveCreds(t, handler, res.ID, "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "resource", out.Scope)
	require.Equal(t, byoFakeToken, out.Fields["token"])
}

func TestInternalCredentialsAgentFallsBackToDefault(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "pa-fallback-api")
	unknownAgent := "22222222-2222-4222-8222-222222222222"

	code, out := resolveCreds(t, handler, res.ID, unknownAgent)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "resource", out.Scope, "agent without its own credential must get the resource default")
	require.Equal(t, byoFakeToken, out.Fields["token"])
}

func TestInternalCredentialsAgentWithoutDefaultStillServesOwn(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})
	// Resource with NO resource-level secret (no auth payload, no auth_ref).
	var res domain.RegistryResource
	doJSON(t, handler, http.MethodPost, registryBase+"rest",
		map[string]any{
			"name":            "no-default-api",
			"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "bearer"},
		}, http.StatusCreated, &res)
	agent := "33333333-3333-4333-8333-333333333333"
	secrets.set(kube.ResourceAgentSecretName(res.ID, agent), map[string]string{"token": "fake-pat-onlyagent-DO-NOT-USE"})

	code, out := resolveCreds(t, handler, res.ID, agent)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "agent", out.Scope)

	// Same resource, different agent, no default → 404 (neither exists).
	code, _ = resolveCreds(t, handler, res.ID, "44444444-4444-4444-8444-444444444444")
	require.Equal(t, http.StatusNotFound, code)
	// Resource-only call on the ref-less resource also 404s (unchanged).
	code, _ = resolveCreds(t, handler, res.ID, "")
	require.Equal(t, http.StatusNotFound, code)
}

func TestInternalCredentialsTwoAgentIsolation(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "isolation-api")
	agentA := "aaaaaaa1-0000-4000-8000-000000000001"
	agentB := "aaaaaaa2-0000-4000-8000-000000000002"
	secrets.set(kube.ResourceAgentSecretName(res.ID, agentA), map[string]string{"token": "fake-token-A-DO-NOT-USE"})
	secrets.set(kube.ResourceAgentSecretName(res.ID, agentB), map[string]string{"token": "fake-token-B-DO-NOT-USE"})

	_, a := resolveCreds(t, handler, res.ID, agentA)
	_, b := resolveCreds(t, handler, res.ID, agentB)
	require.Equal(t, "fake-token-A-DO-NOT-USE", a.Fields["token"], "agent A must resolve its OWN credential")
	require.Equal(t, "fake-token-B-DO-NOT-USE", b.Fields["token"], "agent B must resolve its OWN credential")
	require.NotContains(t, a.Fields["token"], "B")
	require.NotContains(t, b.Fields["token"], "A")
}

func TestInternalCredentialsPerAgentAuditCarriesAgentNoValues(t *testing.T) {
	store := storage.NewMemoryStore()
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "audit-pa-api")
	agent := "55555555-5555-4555-8555-555555555555"
	secretVal := "fake-audit-agent-DO-NOT-USE"
	secrets.set(kube.ResourceAgentSecretName(res.ID, agent), map[string]string{"token": secretVal})

	resolveCreds(t, handler, res.ID, agent)
	resolveCreds(t, handler, res.ID, "66666666-6666-4666-8666-666666666666") // falls back to default

	entries, err := store.ListAudit(context.Background(), "", 200)
	require.NoError(t, err)
	var servedAgent, servedResource int
	for _, e := range entries {
		if e.Action != "credentials.access" {
			continue
		}
		raw, _ := json.Marshal(e)
		require.NotContains(t, string(raw), secretVal, "credentials.access audit leaked a secret value")
		require.NotContains(t, string(raw), byoFakeToken)
		var md map[string]string
		require.NoError(t, json.Unmarshal(e.Metadata, &md))
		if md["outcome"] != "served" {
			continue
		}
		if md["agent"] == agent {
			servedAgent++
		} else {
			servedResource++
		}
	}
	require.Equal(t, 1, servedAgent)
	require.Equal(t, 1, servedResource, "fallback serve must be audited with the requesting agent")
}

// ── Registration custody ─────────────────────────────────────────────────

func TestBYORegistrationStoresSecretAndRedactsEveryRead(t *testing.T) {
	store := storage.NewMemoryStore()
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "redact-api")

	require.True(t, strings.HasPrefix(res.AuthRef, "k8s://"), "auth_ref must point at the managed Secret")
	require.NotContains(t, res.AuthRef, byoFakeToken)

	stored, ok := secrets.get("skquad-rest-" + res.ID)
	require.True(t, ok, "managed secret must exist under the resource-id-derived name")
	require.Equal(t, byoFakeToken, stored["token"])

	// Admin GET of the resource: no secret value anywhere in the body.
	var got domain.RegistryResource
	doJSON(t, handler, http.MethodGet, registryBase+"rest/"+res.ID, nil, http.StatusOK, &got)
	require.NotContains(t, got.AuthRef, byoFakeToken)

	// Policy snapshot (gateway-facing): the denylist strips auth_ref and
	// anything secret-looking; the value must be absent regardless.
	pol := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/policy?agent=00000000-0000-0000-0000-000000000000", "")
	require.NotContains(t, pol.Body.String(), byoFakeToken)

	// Audit log: registration audits must not carry the value either.
	entries, err := store.ListAudit(context.Background(), "", 200)
	require.NoError(t, err)
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		require.NotContains(t, string(raw), byoFakeToken, "audit entry %s leaked the secret", e.Action)
	}
}

func TestBYORegistrationValidation(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})

	t.Run("bearer missing token", func(t *testing.T) {
		body := byoBearerBody("v-1")
		body["auth"] = map[string]string{"token": ""}
		doJSONNoBody(t, handler, http.MethodPost, registryBase+"rest", body, http.StatusBadRequest)
	})
	t.Run("unknown auth field", func(t *testing.T) {
		body := byoBearerBody("v-2")
		body["auth"] = map[string]string{"token": byoFakeToken, "evil": "x"}
		doJSONNoBody(t, handler, http.MethodPost, registryBase+"rest", body, http.StatusBadRequest)
	})
	t.Run("auth with auth_kind none", func(t *testing.T) {
		body := byoBearerBody("v-3")
		body["endpoint_config"] = map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "none"}
		doJSONNoBody(t, handler, http.MethodPost, registryBase+"rest", body, http.StatusBadRequest)
	})
	t.Run("oauth2 bad token_url", func(t *testing.T) {
		body := map[string]any{
			"name":            "v-4",
			"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "oauth2_client_credentials"},
			"auth":            map[string]string{"client_id": "cid", "client_secret": "csecret", "token_url": "ftp://nope"},
		}
		doJSONNoBody(t, handler, http.MethodPost, registryBase+"rest", body, http.StatusBadRequest)
	})
	t.Run("auth on non-rest resource", func(t *testing.T) {
		body := map[string]any{"name": "v-5", "auth": map[string]string{"token": byoFakeToken}}
		doJSONNoBody(t, handler, http.MethodPost, registryBase+"web", body, http.StatusBadRequest)
	})
	t.Run("basic missing password", func(t *testing.T) {
		body := map[string]any{
			"name":            "v-6",
			"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "basic"},
			"auth":            map[string]string{"username": "u"},
		}
		doJSONNoBody(t, handler, http.MethodPost, registryBase+"rest", body, http.StatusBadRequest)
	})
}

func TestBYOSecretRotation(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "rotate-api")
	secretName := "skquad-rest-" + res.ID

	rotated := "rotated-token-DO-NOT-USE"
	var updated domain.RegistryResource
	doJSON(t, handler, http.MethodPatch, registryBase+"rest/"+res.ID,
		map[string]any{"auth": map[string]string{"token": rotated}}, http.StatusOK, &updated)

	stored, ok := secrets.get(secretName)
	require.True(t, ok)
	require.Equal(t, rotated, stored["token"])

	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource="+res.ID, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), rotated)
	require.NotContains(t, rec.Body.String(), byoFakeToken, "old secret must be gone after rotation")
}

func TestRotateToNoneClearsManagedSecret(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "none-api")

	var updated domain.RegistryResource
	doJSON(t, handler, http.MethodPatch, registryBase+"rest/"+res.ID, map[string]any{
		"endpoint_config": map[string]any{"base_url": "https://api.example.com/v2", "auth_kind": "none"},
		"auth":            map[string]any{},
	}, http.StatusOK, &updated)
	require.Empty(t, updated.AuthRef)
	require.Contains(t, secrets.deletedNames(), "skquad-rest-"+res.ID)
}

func TestBYOSecretStoreFailureRollsBackCreate(t *testing.T) {
	store := storage.NewMemoryStore()
	secrets := newFakeResourceSecretStore()
	secrets.failEnsure = true
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: secrets})
	doJSONNoBody(t, handler, http.MethodPost, registryBase+"rest", byoBearerBody("rollback-api"), http.StatusBadGateway)

	// The resource row must not survive a failed secret write.
	resources, err := store.ListResources(context.Background(), domain.ResRest)
	require.NoError(t, err)
	for _, r := range resources {
		require.NotEqual(t, "rollback-api", r.Name)
	}
}

func TestDeleteResourceRemovesManagedSecret(t *testing.T) {
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), storage.NewMemoryStore(), serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "delete-api")
	require.NotEmpty(t, res.AuthRef)

	req := httptest.NewRequest(http.MethodDelete, registryBase+"rest/"+res.ID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Contains(t, secrets.deletedNames(), "skquad-rest-"+res.ID)
}

// ── Audit hygiene ────────────────────────────────────────────────────────

func TestCredentialAccessAuditHasNoValues(t *testing.T) {
	store := storage.NewMemoryStore()
	secrets := newFakeResourceSecretStore()
	handler := newServer(testConfig(), store, serverDeps{resourceSecrets: secrets})
	res := createBYORestResource(t, handler, "audit-api")

	rec := doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource="+res.ID, "")
	require.Equal(t, http.StatusOK, rec.Code)
	// A denied access too (unknown resource).
	doRawNoAuth(t, handler, http.MethodGet, "/internal/v1/credentials?resource=00000000-0000-0000-0000-000000000001", "")

	entries, err := store.ListAudit(context.Background(), "", 200)
	require.NoError(t, err)
	var served, denied int
	for _, e := range entries {
		if e.Action != "credentials.access" {
			continue
		}
		raw, _ := json.Marshal(e)
		require.NotContains(t, string(raw), byoFakeToken, "credentials.access audit leaked a secret value")
		var md map[string]string
		require.NoError(t, json.Unmarshal(e.Metadata, &md))
		switch md["outcome"] {
		case "served":
			served++
			require.Equal(t, res.ID, e.ResourceID)
			require.Equal(t, "bearer", md["kind"])
		case "denied":
			denied++
		default:
			t.Fatalf("unexpected audit outcome %q", md["outcome"])
		}
	}
	require.Equal(t, 1, served)
	require.Equal(t, 1, denied)
}
