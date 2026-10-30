package tools

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/k8s-mcp/internal/config"
	"github.com/rossbrigoli/skquad/k8s-mcp/internal/k8s"
)

// fakeAPI records requests and serves canned Kubernetes API responses.
type fakeAPI struct {
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func (f *fakeAPI) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, r.ContentLength)
	if r.ContentLength > 0 {
		_, _ = r.Body.Read(body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)})
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/v1/namespaces/skquad-system/pods" && r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"api-1"},"spec":{"nodeName":"n1"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0}]}}]}`))
	case r.URL.Path == "/api/v1/namespaces/kube-system/pods":
		_, _ = w.Write([]byte(`{"items":[]}`))
	case r.URL.Path == "/api/v1/namespaces/skquad-system/services":
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"api"},"spec":{"type":"ClusterIP","clusterIP":"10.0.0.1","ports":[{"port":80}]}}]}`))
	case r.URL.Path == "/api/v1/namespaces/skquad-system/events":
		_, _ = w.Write([]byte(`{"items":[{"type":"Warning","reason":"BackOff","message":"back pulling image","count":3,"lastTimestamp":"2026-10-30T10:00:00Z","involvedObject":{"kind":"Pod","name":"api-1"}}]}`))
	case r.URL.Path == "/api/v1/namespaces/skquad-system/pods/api-1" && r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`{"metadata":{"name":"api-1","namespace":"skquad-system","labels":{"app":"api"}},"spec":{"nodeName":"n1","containers":[{"name":"api","image":"skquad/api:v1"}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"api","ready":true,"restartCount":0}]}}`))
	case r.URL.Path == "/api/v1/namespaces/skquad-system/pods/api-1/log":
		_, _ = w.Write([]byte("log line 1\nlog line 2\n"))
	case r.URL.Path == "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/skquad" && r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`{"metadata":{"name":"skquad"},"status":{"sync":{"status":"Synced","revision":"abc123"},"health":{"status":"Healthy"},"resources":[{"kind":"Deployment"}]}}`))
	case r.URL.Path == "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/skquad" && r.Method == http.MethodPatch:
		_, _ = w.Write([]byte(`{"metadata":{"name":"skquad"}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}
}

func newTestRegistry(t *testing.T, argoApps []string) (*Registry, *fakeAPI, *httptest.Server) {
	t.Helper()
	fake := &fakeAPI{}
	srv := httptest.NewTLSServer(fake)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	tokPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokPath, []byte("sa-token"), 0o600))
	client, err := k8s.New(srv.URL, caPath, tokPath, 5*time.Second)
	require.NoError(t, err)
	cfg := &config.Config{
		AllowedNamespaces: []string{"skquad-system"},
		ArgoNamespace:     "argocd",
		ArgoAllowedApps:   argoApps,
		MaxLogLines:       500,
	}
	return NewRegistry(cfg, client), fake, srv
}

func call(t *testing.T, r *Registry, name string, args map[string]any) (Result, bool) {
	t.Helper()
	return r.Call(context.Background(), name, args)
}

func TestCatalogShape(t *testing.T) {
	reg, _, srv := newTestRegistry(t, []string{"skquad"})
	defer srv.Close()
	var names []string
	for _, tl := range reg.Tools() {
		names = append(names, tl.Name)
		require.NotEmpty(t, tl.Description)
		require.NotEmpty(t, tl.InputSchema)
	}
	assert.Equal(t, []string{
		"k8s_pods_list",
		"k8s_services_list",
		"k8s_events_list",
		"k8s_pod_describe",
		"k8s_pod_logs",
		"argocd_app_status",
		"argocd_app_refresh",
		"argocd_app_sync",
	}, names)
	// No escape-hatch tools exist under any plausible name.
	for _, forbidden := range []string{"kubectl", "exec", "shell", "raw_request", "api_call", "secrets_get", "configmap_get"} {
		_, ok := reg.Call(context.Background(), forbidden, nil)
		assert.False(t, ok, "tool %q must not exist", forbidden)
	}
}

func TestPodsListAllowedNamespace(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, nil)
	defer srv.Close()
	res, ok := call(t, reg, "k8s_pods_list", map[string]any{"namespace": "skquad-system"})
	require.True(t, ok)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Text, "api-1")
	assert.Len(t, fake.recorded(), 1)
}

func TestPodsListDeniedNamespaceNoNetworkCall(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, nil)
	defer srv.Close()
	res, ok := call(t, reg, "k8s_pods_list", map[string]any{"namespace": "kube-system"})
	require.True(t, ok)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Text, "denied")
	assert.Len(t, fake.recorded(), 0, "denied namespace must not hit the API")
}

func TestPodsListMissingNamespaceDenied(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, nil)
	defer srv.Close()
	res, _ := call(t, reg, "k8s_pods_list", map[string]any{})
	assert.True(t, res.IsError)
	assert.Contains(t, res.Text, "namespace is required")
	assert.Len(t, fake.recorded(), 0)
}

func TestPodsListInvalidLabelSelectorDenied(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, nil)
	defer srv.Close()
	res, _ := call(t, reg, "k8s_pods_list", map[string]any{"namespace": "skquad-system", "label_selector": "app; rm -rf"})
	assert.True(t, res.IsError)
	assert.Contains(t, res.Text, "denied")
	assert.Len(t, fake.recorded(), 0)
}

func TestPodLogsTailCapEnforced(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, nil)
	defer srv.Close()
	res, _ := call(t, reg, "k8s_pod_logs", map[string]any{"namespace": "skquad-system", "pod": "api-1", "tail_lines": 99999})
	require.False(t, res.IsError)
	rec := fake.recorded()
	require.Len(t, rec, 1)
	assert.Contains(t, rec[0].Query, "tailLines=500", "requested tail must be clamped to the configured max")
}

func TestPodLogsDeniedNamespace(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, nil)
	defer srv.Close()
	res, _ := call(t, reg, "k8s_pod_logs", map[string]any{"namespace": "default", "pod": "x"})
	assert.True(t, res.IsError)
	assert.Contains(t, res.Text, "denied")
	assert.Len(t, fake.recorded(), 0)
}

func TestPodDescribeIncludesEventsAndStatus(t *testing.T) {
	reg, _, srv := newTestRegistry(t, nil)
	defer srv.Close()
	res, _ := call(t, reg, "k8s_pod_describe", map[string]any{"namespace": "skquad-system", "pod": "api-1"})
	require.False(t, res.IsError)
	assert.Contains(t, res.Text, "skquad/api:v1")
	assert.Contains(t, res.Text, "BackOff")
}

func TestArgoStatusDeniedWhenAppNotAllowlisted(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, []string{"other-app"})
	defer srv.Close()
	res, _ := call(t, reg, "argocd_app_status", map[string]any{"app": "skquad"})
	assert.True(t, res.IsError)
	assert.Contains(t, res.Text, "not in the allowlist")
	assert.Len(t, fake.recorded(), 0)
}

func TestArgoStatusDeniedWhenNoAppsConfigured(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, nil)
	defer srv.Close()
	res, _ := call(t, reg, "argocd_app_sync", map[string]any{"app": "skquad"})
	assert.True(t, res.IsError)
	assert.Contains(t, res.Text, "fail-closed")
	assert.Len(t, fake.recorded(), 0)
}

func TestArgoStatusHappyPath(t *testing.T) {
	reg, _, srv := newTestRegistry(t, []string{"skquad"})
	defer srv.Close()
	res, _ := call(t, reg, "argocd_app_status", map[string]any{"app": "skquad"})
	require.False(t, res.IsError)
	assert.Contains(t, res.Text, "Synced")
	assert.Contains(t, res.Text, "Healthy")
}

func TestArgoRefreshAllowlisted(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, []string{"skquad"})
	defer srv.Close()
	res, _ := call(t, reg, "argocd_app_refresh", map[string]any{"app": "skquad"})
	require.False(t, res.IsError)
	rec := fake.recorded()
	require.Len(t, rec, 1)
	assert.Equal(t, http.MethodPatch, rec[0].Method)
	assert.Contains(t, rec[0].Body, `"refresh":"hard"`)
}

func TestArgoSyncAllowlistedPruneFalse(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, []string{"skquad"})
	defer srv.Close()
	res, _ := call(t, reg, "argocd_app_sync", map[string]any{"app": "skquad"})
	require.False(t, res.IsError)
	rec := fake.recorded()
	require.Len(t, rec, 1)
	assert.Equal(t, http.MethodPatch, rec[0].Method)
	var patch struct {
		Operation struct {
			Sync struct {
				Prune bool `json:"prune"`
			} `json:"sync"`
		} `json:"operation"`
	}
	require.NoError(t, json.Unmarshal([]byte(rec[0].Body), &patch))
	assert.False(t, patch.Operation.Sync.Prune)
}

func TestArgoSyncPruneTrueDenied(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, []string{"skquad"})
	defer srv.Close()
	res, _ := call(t, reg, "argocd_app_sync", map[string]any{"app": "skquad", "prune": true})
	assert.True(t, res.IsError)
	assert.Contains(t, res.Text, "prune=true is not permitted")
	assert.Len(t, fake.recorded(), 0)
}

func TestArgoSyncInvalidRevisionDenied(t *testing.T) {
	reg, fake, srv := newTestRegistry(t, []string{"skquad"})
	defer srv.Close()
	res, _ := call(t, reg, "argocd_app_sync", map[string]any{"app": "skquad", "revision": "evil/../x"})
	assert.True(t, res.IsError)
	assert.Contains(t, res.Text, "denied")
	assert.Len(t, fake.recorded(), 0)
}

func TestUnknownToolReturnsNotKnown(t *testing.T) {
	reg, _, srv := newTestRegistry(t, nil)
	defer srv.Close()
	_, ok := call(t, reg, "k8s_secret_get", nil)
	assert.False(t, ok)
}
