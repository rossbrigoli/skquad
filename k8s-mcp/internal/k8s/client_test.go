package k8s

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsDeniedResource(t *testing.T) {
	for _, r := range []string{"secrets", "configmaps", "serviceaccounts", "roles", "rolebindings", "clusterroles", "clusterrolebindings", "nodes", "namespaces", "persistentvolumes", "SECRETS"} {
		assert.True(t, IsDeniedResource(r), "%s must be denied", r)
	}
	for _, r := range []string{"pods", "services", "events", "applications", "endpoints"} {
		assert.False(t, IsDeniedResource(r), "%s must be allowed", r)
	}
}

func TestGuardPathDeniesSecrets(t *testing.T) {
	err := guardPath("/api/v1/namespaces/skquad-system/secrets/my-secret")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied resource")
}

func TestGuardPathDeniesConfigMaps(t *testing.T) {
	err := guardPath("/api/v1/namespaces/default/configmaps/foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied resource")
}

func TestGuardPathDeniesExecSubresource(t *testing.T) {
	err := guardPath("/api/v1/namespaces/skquad-system/pods/mypod/exec")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied subresource")
}

func TestGuardPathDeniesAttachPortForward(t *testing.T) {
	require.Error(t, guardPath("/api/v1/namespaces/x/pods/y/attach"))
	require.Error(t, guardPath("/api/v1/namespaces/x/pods/y/portforward"))
}

func TestGuardPathAllowsScopedResources(t *testing.T) {
	require.NoError(t, guardPath("/api/v1/namespaces/skquad-system/pods"))
	require.NoError(t, guardPath("/api/v1/namespaces/skquad-system/pods/foo/log"))
	require.NoError(t, guardPath("/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/skquad"))
}

func TestGuardPathClusterScopedDeniesNodes(t *testing.T) {
	require.Error(t, guardPath("/api/v1/nodes"))
	require.Error(t, guardPath("/apis/rbac.authorization.k8s.io/v1/clusterroles"))
}

// newTestClient wires a Client against an httptest TLS server, trusting the
// test server's certificate via a temp CA file and a temp SA token file.
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	tokPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokPath, []byte("sa-token"), 0o600))
	c, err := New(srv.URL, caPath, tokPath, 5*time.Second)
	require.NoError(t, err)
	return c, srv
}

func TestClientSendsSATokenAndAuthorization(t *testing.T) {
	var gotAuth string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	_, err := c.GetPods(context.Background(), "skquad-system", "")
	require.NoError(t, err)
	assert.Equal(t, "Bearer sa-token", gotAuth)
}

func TestClientRefusesDeniedResourceWithoutNetwork(t *testing.T) {
	hit := false
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()
	_, err := c.do(context.Background(), http.MethodGet, "/api/v1/namespaces/x/secrets/y", nil, nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied resource")
	assert.False(t, hit, "denied request must never reach the network")
}

func TestClientSurfacesAPIError(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"pods 'nope' not found"}`))
	}))
	defer srv.Close()
	_, err := c.GetPod(context.Background(), "ns", "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestClientPatchArgoAppUsesMergePatch(t *testing.T) {
	var ct, body string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		body = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_, err := c.PatchArgoApp(context.Background(), "argocd", "skquad", []byte(`{"operation":{"refresh":"hard"}}`))
	require.NoError(t, err)
	assert.Equal(t, "application/merge-patch+json", ct)
	assert.Contains(t, body, "refresh")
}
