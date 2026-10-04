package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

// S-189 coverage: gateway rollout-restart helper (S-GWREG).

func newTestReloader(t *testing.T, h http.HandlerFunc) *GatewayReloader {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return &GatewayReloader{
		baseURL:    server.URL,
		namespace:  testNamespace,
		deployment: "skquad-llm-gateway",
		token:      testToken,
		client:     server.Client(),
	}
}

func TestReloadGatewayStampsRolloutAnnotation(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotContentType, gotAuth string
	var gotPatch map[string]any
	g := newTestReloader(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		gotContentType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotPatch)
		w.WriteHeader(http.StatusOK)
	})

	before := time.Now().UTC().Add(-time.Second)
	if err := g.ReloadGateway(context.Background()); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %s, want PATCH", gotMethod)
	}
	if gotPath != "/apis/apps/v1/namespaces/skquad-system/deployments/skquad-llm-gateway" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotContentType != "application/strategic-merge-patch+json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("auth = %q", gotAuth)
	}

	tmpl, _ := gotPatch["spec"].(map[string]any)["template"].(map[string]any)
	meta, _ := tmpl["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	stamp, _ := ann[GatewayReloadAnnotation].(string)
	if stamp == "" {
		t.Fatalf("reload annotation missing: %v", gotPatch)
	}
	stampedAt, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("stamp %q not RFC3339Nano: %v", stamp, err)
	}
	if stampedAt.Before(before) || stampedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("stamp %q not a fresh timestamp", stamp)
	}
}

func TestReloadGatewayPropagatesAPIError(t *testing.T) {
	t.Parallel()

	g := newTestReloader(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("deployments.apps is forbidden"))
	})
	err := g.ReloadGateway(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err = %v, want 403 with body snippet", err)
	}
}

func TestNewGatewayReloaderConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte("gw-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := NewGatewayReloader(&config.Config{
		K8sAPIBase:           "https://api.test//",
		K8sNamespace:         "skquad-system",
		K8sTokenFile:         tokFile,
		LLMGatewayDeployment: "custom-gateway",
		K8sInsecure:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.token != "gw-token" || g.deployment != "custom-gateway" || g.baseURL != "https://api.test" {
		t.Fatalf("reloader = %+v", g)
	}

	if _, err := NewGatewayReloader(&config.Config{K8sTokenFile: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("missing token file must error")
	}
}
