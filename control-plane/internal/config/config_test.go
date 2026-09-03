package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// clearEnv removes every SKQUAD_* variable so Load() starts from defaults.
// t.Setenv is used for the overrides so each test restores the environment.
func clearEnv(t *testing.T) {
	t.Helper()

	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "SKQUAD_") {
			t.Setenv(key, "")
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.AuthMode != AuthDev {
		t.Fatalf("AuthMode = %q, want %q", cfg.AuthMode, AuthDev)
	}
	if cfg.DevEmail != "dev@skquad.local" || cfg.DevName != "Dev Admin" {
		t.Fatalf("dev principal = %q/%q", cfg.DevEmail, cfg.DevName)
	}
	if cfg.DatabaseURL != "" {
		t.Fatalf("DatabaseURL = %q, want empty", cfg.DatabaseURL)
	}
	if cfg.K8sEnabled {
		t.Fatal("K8sEnabled default = true, want false")
	}
	if cfg.K8sAPIBase != "https://kubernetes.default.svc" || cfg.K8sNamespace != "skquad-system" || cfg.K8sGroupVersion != "skquad.io/v1" {
		t.Fatalf("k8s defaults = %q/%q/%q", cfg.K8sAPIBase, cfg.K8sNamespace, cfg.K8sGroupVersion)
	}
	if cfg.K8sInsecure {
		t.Fatal("K8sInsecure default = true, want false")
	}
	if cfg.AgentImage != "skquad/agent-runtime:0.1.0" {
		t.Fatalf("AgentImage = %q", cfg.AgentImage)
	}
	if cfg.DefaultIdleTimeout != 5*time.Minute {
		t.Fatalf("DefaultIdleTimeout = %s, want 5m", cfg.DefaultIdleTimeout)
	}
	if cfg.ReaperInterval != 30*time.Second {
		t.Fatalf("ReaperInterval = %s, want 30s", cfg.ReaperInterval)
	}
	if cfg.ReaperGrace != 120*time.Second {
		t.Fatalf("ReaperGrace = %s, want 120s", cfg.ReaperGrace)
	}
	if cfg.MemoryEmbeddingsEnabled {
		t.Fatal("MemoryEmbeddingsEnabled default = true, want false")
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)

	t.Setenv("SKQUAD_ADDR", ":9999")
	t.Setenv("SKQUAD_AUTH_MODE", "oidc")
	t.Setenv("SKQUAD_OIDC_ISSUER", "https://idp.example.test/realms/skquad")
	t.Setenv("SKQUAD_OIDC_AUDIENCE", "skquad-api")
	t.Setenv("SKQUAD_DATABASE_URL", "postgres://example/db")
	t.Setenv("SKQUAD_K8S_ENABLED", "true")
	t.Setenv("SKQUAD_K8S_INSECURE", "true")
	t.Setenv("SKQUAD_K8S_NAMESPACE", "skquad-dev")
	t.Setenv("SKQUAD_AGENT_IMAGE", "registry.example.test/agent:1.2.3")
	t.Setenv("SKQUAD_CONTROL_PLANE_URL", "https://api.skquad.test")
	t.Setenv("SKQUAD_LLM_GATEWAY_URL", "https://gateway.skquad.test")
	t.Setenv("SKQUAD_LITELLM_ADMIN_URL", "https://admin.skquad.test")
	t.Setenv("SKQUAD_LITELLM_MASTER_KEY", "master-key")
	t.Setenv("SKQUAD_GATEWAY_CALLBACK_TOKEN", "callback-token")
	t.Setenv("SKQUAD_MEMORY_EMBEDDINGS_ENABLED", "true")
	t.Setenv("SKQUAD_MEMORY_EMBEDDING_MODEL", "bge-m3")
	t.Setenv("SKQUAD_DEFAULT_IDLE_TIMEOUT", "90s")
	t.Setenv("SKQUAD_REAPER_INTERVAL_SECONDS", "45")
	t.Setenv("SKQUAD_REAPER_GRACE_SECONDS", "180")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9999" || cfg.AuthMode != AuthOIDC {
		t.Fatalf("addr/auth = %q/%q", cfg.Addr, cfg.AuthMode)
	}
	if cfg.IssuerURL != "https://idp.example.test/realms/skquad" || cfg.Audience != "skquad-api" {
		t.Fatalf("oidc settings = %q/%q", cfg.IssuerURL, cfg.Audience)
	}
	if cfg.DatabaseURL != "postgres://example/db" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if !cfg.K8sEnabled || !cfg.K8sInsecure || cfg.K8sNamespace != "skquad-dev" {
		t.Fatalf("k8s settings = %+v", cfg)
	}
	if cfg.AgentImage != "registry.example.test/agent:1.2.3" || cfg.ControlPlaneURL != "https://api.skquad.test" {
		t.Fatalf("agent/control-plane = %q/%q", cfg.AgentImage, cfg.ControlPlaneURL)
	}
	if cfg.LiteLLMAdminURL != "https://admin.skquad.test" {
		t.Fatalf("LiteLLMAdminURL = %q, want explicit admin URL", cfg.LiteLLMAdminURL)
	}
	if cfg.LiteLLMMasterKey != "master-key" || cfg.GatewayCallbackToken != "callback-token" {
		t.Fatalf("gateway credentials not loaded")
	}
	if !cfg.MemoryEmbeddingsEnabled || cfg.MemoryEmbeddingModel != "bge-m3" {
		t.Fatalf("memory settings = %v/%q", cfg.MemoryEmbeddingsEnabled, cfg.MemoryEmbeddingModel)
	}
	if cfg.DefaultIdleTimeout != 90*time.Second {
		t.Fatalf("DefaultIdleTimeout = %s, want 90s", cfg.DefaultIdleTimeout)
	}
	if cfg.ReaperInterval != 45*time.Second || cfg.ReaperGrace != 180*time.Second {
		t.Fatalf("reaper settings = %s/%s", cfg.ReaperInterval, cfg.ReaperGrace)
	}
}

func TestLoadLiteLLMAdminURLFallsBackToGateway(t *testing.T) {
	clearEnv(t)

	t.Setenv("SKQUAD_LLM_GATEWAY_URL", "https://gateway.skquad.test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LiteLLMAdminURL != "https://gateway.skquad.test" {
		t.Fatalf("LiteLLMAdminURL = %q, want gateway fallback", cfg.LiteLLMAdminURL)
	}
}

func TestLoadRejectsBadAuthMode(t *testing.T) {
	clearEnv(t)

	t.Setenv("SKQUAD_AUTH_MODE", "magic")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SKQUAD_AUTH_MODE") {
		t.Fatalf("Load error = %v, want SKQUAD_AUTH_MODE rejection", err)
	}
}

func TestLoadOIDCRequiresIssuerAndAudience(t *testing.T) {
	clearEnv(t)

	t.Setenv("SKQUAD_AUTH_MODE", "oidc")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SKQUAD_OIDC_ISSUER") {
		t.Fatalf("Load error = %v, want issuer requirement", err)
	}

	t.Setenv("SKQUAD_OIDC_ISSUER", "https://idp.example.test")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SKQUAD_OIDC_AUDIENCE") {
		t.Fatalf("Load error = %v, want audience requirement", err)
	}

	t.Setenv("SKQUAD_OIDC_AUDIENCE", "skquad-api")
	if _, err := Load(); err != nil {
		t.Fatalf("Load with complete oidc settings: %v", err)
	}
}

func TestEnvBool(t *testing.T) {
	clearEnv(t)

	// SKQUAD_K8S_ENABLED defaults to false, so empty and invalid values must
	// both resolve to false rather than turning the CR writer on by accident.
	cases := []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "true", want: true},
		{value: "TRUE", want: true},
		{value: "1", want: true},
		{value: "false", want: false},
		{value: "0", want: false},
		{value: "not-a-bool", want: false},
	}
	for _, tc := range cases {
		t.Setenv("SKQUAD_K8S_ENABLED", tc.value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(%q): %v", tc.value, err)
		}
		if cfg.K8sEnabled != tc.want {
			t.Fatalf("SKQUAD_K8S_ENABLED=%q → %v, want %v", tc.value, cfg.K8sEnabled, tc.want)
		}
	}
}

func TestEnvDurationAndSeconds(t *testing.T) {
	clearEnv(t)

	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "unset", value: "", want: 5 * time.Minute},
		{name: "parsed", value: "45s", want: 45 * time.Second},
		{name: "minutes", value: "2m", want: 2 * time.Minute},
		{name: "invalid", value: "soon", want: 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Setenv("SKQUAD_DEFAULT_IDLE_TIMEOUT", tc.value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("%s: Load: %v", tc.name, err)
		}
		if cfg.DefaultIdleTimeout != tc.want {
			t.Fatalf("%s: DefaultIdleTimeout = %s, want %s", tc.name, cfg.DefaultIdleTimeout, tc.want)
		}
	}

	seconds := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "unset", value: "", want: 30 * time.Second},
		{name: "parsed", value: "75", want: 75 * time.Second},
		{name: "zero is respected", value: "0", want: 0},
		{name: "negative falls back", value: "-5", want: 30 * time.Second},
		{name: "invalid falls back", value: "thirty", want: 30 * time.Second},
	}
	for _, tc := range seconds {
		t.Setenv("SKQUAD_REAPER_INTERVAL_SECONDS", tc.value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("%s: Load: %v", tc.name, err)
		}
		if cfg.ReaperInterval != tc.want {
			t.Fatalf("%s: ReaperInterval = %s, want %s", tc.name, cfg.ReaperInterval, tc.want)
		}
	}
}
