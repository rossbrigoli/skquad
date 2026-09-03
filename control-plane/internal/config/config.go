// Package config loads control-plane configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// AuthMode selects how the API authenticates human users.
type AuthMode string

const (
	// AuthDev disables authN and uses a fixed admin principal. Development only.
	AuthDev AuthMode = "dev"
	// AuthOIDC validates Bearer JWTs against the configured OIDC issuer.
	AuthOIDC AuthMode = "oidc"
)

// Config holds all control-plane settings.
type Config struct {
	// HTTP
	Addr string // listen address, e.g. ":8080"

	// AuthN
	AuthMode  AuthMode
	IssuerURL string // OIDC issuer (AuthMode=oidc)
	Audience  string // expected JWT audience
	DevEmail  string // fixed principal email (AuthMode=dev)
	DevName   string // fixed principal name (AuthMode=dev)

	// Storage
	DatabaseURL string // Postgres DSN

	// Kubernetes (CR writer)
	K8sEnabled      bool
	K8sAPIBase      string // e.g. https://kubernetes.default.svc
	K8sNamespace    string // namespace where Squad/Agent CRs live
	K8sTokenFile    string // path to service-account token
	K8sGroupVersion string // e.g. skquad.io/v1
	K8sInsecure     bool   // skip TLS verification (dev)
	AgentImage      string // image written into Agent CR specs
	ControlPlaneURL string // URL written into Agent CR specs for runtime callbacks
	LLMGatewayURL   string // URL written into Agent CR specs for LLM gateway calls

	// LiteLLM gateway management
	LiteLLMAdminURL      string // URL used by the API server for LiteLLM key management
	LiteLLMMasterKey     string // LiteLLM proxy admin key used for virtual-key provisioning
	GatewayCallbackToken string // internal bearer token for gateway callbacks into the API

	// Memory
	MemoryEmbeddingsEnabled bool   // semantic memory retrieval uses embeddings only when true
	MemoryEmbeddingModel    string // embedding model name for generated vectors

	// Behaviour
	DefaultIdleTimeout time.Duration
	ReaperInterval     time.Duration // how often the execution reaper runs
	ReaperGrace        time.Duration // extra time beyond the lease before an execution is declared dead
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Addr:                    envOr("SKQUAD_ADDR", ":8080"),
		AuthMode:                AuthMode(envOr("SKQUAD_AUTH_MODE", string(AuthDev))),
		IssuerURL:               os.Getenv("SKQUAD_OIDC_ISSUER"),
		Audience:                os.Getenv("SKQUAD_OIDC_AUDIENCE"),
		DevEmail:                envOr("SKQUAD_DEV_EMAIL", "dev@skquad.local"),
		DevName:                 envOr("SKQUAD_DEV_NAME", "Dev Admin"),
		DatabaseURL:             os.Getenv("SKQUAD_DATABASE_URL"),
		K8sEnabled:              envBool("SKQUAD_K8S_ENABLED", false),
		K8sAPIBase:              envOr("SKQUAD_K8S_API_BASE", "https://kubernetes.default.svc"),
		K8sNamespace:            envOr("SKQUAD_K8S_NAMESPACE", "skquad-system"),
		K8sTokenFile:            envOr("SKQUAD_K8S_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		K8sGroupVersion:         envOr("SKQUAD_K8S_GROUP_VERSION", "skquad.io/v1"),
		K8sInsecure:             envBool("SKQUAD_K8S_INSECURE", false),
		AgentImage:              envOr("SKQUAD_AGENT_IMAGE", "skquad/agent-runtime:0.1.0"),
		ControlPlaneURL:         os.Getenv("SKQUAD_CONTROL_PLANE_URL"),
		LLMGatewayURL:           os.Getenv("SKQUAD_LLM_GATEWAY_URL"),
		LiteLLMAdminURL:         os.Getenv("SKQUAD_LITELLM_ADMIN_URL"),
		LiteLLMMasterKey:        os.Getenv("SKQUAD_LITELLM_MASTER_KEY"),
		GatewayCallbackToken:    os.Getenv("SKQUAD_GATEWAY_CALLBACK_TOKEN"),
		MemoryEmbeddingsEnabled: envBool("SKQUAD_MEMORY_EMBEDDINGS_ENABLED", false),
		MemoryEmbeddingModel:    os.Getenv("SKQUAD_MEMORY_EMBEDDING_MODEL"),
		DefaultIdleTimeout:      envDuration("SKQUAD_DEFAULT_IDLE_TIMEOUT", 5*time.Minute),
		ReaperInterval:          envSeconds("SKQUAD_REAPER_INTERVAL_SECONDS", 30),
		ReaperGrace:             envSeconds("SKQUAD_REAPER_GRACE_SECONDS", 120),
	}
	if c.LiteLLMAdminURL == "" {
		c.LiteLLMAdminURL = c.LLMGatewayURL
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	switch c.AuthMode {
	case AuthDev, AuthOIDC:
	default:
		return fmt.Errorf("config: unknown SKQUAD_AUTH_MODE %q (want %q or %q)",
			c.AuthMode, AuthDev, AuthOIDC)
	}
	if c.AuthMode == AuthOIDC && c.IssuerURL == "" {
		return fmt.Errorf("config: SKQUAD_OIDC_ISSUER is required when SKQUAD_AUTH_MODE=oidc")
	}
	if c.AuthMode == AuthOIDC && c.Audience == "" {
		return fmt.Errorf("config: SKQUAD_OIDC_AUDIENCE is required when SKQUAD_AUTH_MODE=oidc")
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envSeconds(key string, def int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(def) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return time.Duration(def) * time.Second
	}
	return time.Duration(n) * time.Second
}
