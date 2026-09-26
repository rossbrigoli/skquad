// Package config loads control-plane configuration from environment variables.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/breakglass"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
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
	// OIDCAdminGroups binds IdP group claims to platform_admin. Without it every
	// OIDC principal lands as RoleUser and the admin UI is unreachable.
	OIDCAdminGroups []string
	// BreakGlass* configure an OIDC-independent admin path (see
	// internal/breakglass). Everything here is ConfigMap-safe EXCEPT the two
	// secrets, which must come from a SealedSecret:
	// SKQUAD_BREAKGLASS_PASSWORD_HASH and SKQUAD_BREAKGLASS_JWT_KEY.
	// The on/off switch is deliberately a plain ConfigMap value so it can be
	// flipped without re-sealing anything.
	BreakGlassEnabled      bool     // SKQUAD_BREAKGLASS_ENABLED (default false)
	BreakGlassUsername     string  // SKQUAD_BREAKGLASS_USERNAME
	BreakGlassPasswordHash string  // SKQUAD_BREAKGLASS_PASSWORD_HASH (argon2id PHC) - SECRET
	BreakGlassJWTKey       string  // SKQUAD_BREAKGLASS_JWT_KEY - SECRET
	BreakGlassAllowedCIDRs []string // SKQUAD_BREAKGLASS_ALLOWED_CIDRS (comma-separated)
	BreakGlassTokenTTL     time.Duration // SKQUAD_BREAKGLASS_TOKEN_TTL (default 60m)
	BreakGlassMaxAttempts  int           // SKQUAD_BREAKGLASS_MAX_ATTEMPTS (default 5)
	BreakGlassWindow       time.Duration // SKQUAD_BREAKGLASS_WINDOW (default 15m)
	DevEmail        string // fixed principal email (AuthMode=dev)
	DevName         string // fixed principal name (AuthMode=dev)

	// Storage
	DatabaseURL string // Postgres DSN

	// Kubernetes (CR writer)
	K8sEnabled      bool
	K8sAPIBase      string // e.g. https://kubernetes.default.svc
	K8sNamespace    string // namespace where Squad/Agent CRs live
	K8sTokenFile    string // path to service-account token
	K8sCAFile       string // PEM of trusted CAs for the API (default: in-cluster SA CA)
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

	// Agent workspace storage (S-138). Platform-admin knobs only: squad
	// owners pick a size within [0, MaxAgentStorage]; the StorageClass is
	// never tenant-selectable (portability rule, mirrors S-135).
	DefaultAgentStorageSize string // SKQUAD_DEFAULT_AGENT_STORAGE_SIZE (default "2Gi")
	MaxAgentStorage         string // SKQUAD_MAX_AGENT_STORAGE (default "10Gi")
	StorageClass            string // SKQUAD_STORAGE_CLASS ("" = cluster default, omitted from PVC)
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Addr:                    envOr("SKQUAD_ADDR", ":8080"),
		AuthMode:                AuthMode(envOr("SKQUAD_AUTH_MODE", string(AuthDev))),
		IssuerURL:               os.Getenv("SKQUAD_OIDC_ISSUER"),
		Audience:                os.Getenv("SKQUAD_OIDC_AUDIENCE"),
		OIDCAdminGroups:         envList("SKQUAD_OIDC_ADMIN_GROUPS"),
		BreakGlassEnabled:     envBool("SKQUAD_BREAKGLASS_ENABLED", false),
		BreakGlassUsername:    strings.TrimSpace(os.Getenv("SKQUAD_BREAKGLASS_USERNAME")),
		BreakGlassPasswordHash: strings.TrimSpace(os.Getenv("SKQUAD_BREAKGLASS_PASSWORD_HASH")),
		BreakGlassJWTKey:      strings.TrimSpace(os.Getenv("SKQUAD_BREAKGLASS_JWT_KEY")),
		BreakGlassAllowedCIDRs: envList("SKQUAD_BREAKGLASS_ALLOWED_CIDRS"),
		BreakGlassTokenTTL:    envDuration("SKQUAD_BREAKGLASS_TOKEN_TTL", 60*time.Minute),
		BreakGlassMaxAttempts: envInt("SKQUAD_BREAKGLASS_MAX_ATTEMPTS", 5),
		BreakGlassWindow:      envDuration("SKQUAD_BREAKGLASS_WINDOW", 15*time.Minute),
		DevEmail:                envOr("SKQUAD_DEV_EMAIL", "dev@skquad.local"),
		DevName:                 envOr("SKQUAD_DEV_NAME", "Dev Admin"),
		DatabaseURL:             os.Getenv("SKQUAD_DATABASE_URL"),
		K8sEnabled:              envBool("SKQUAD_K8S_ENABLED", false),
		K8sAPIBase:              envOr("SKQUAD_K8S_API_BASE", "https://kubernetes.default.svc"),
		K8sNamespace:            envOr("SKQUAD_K8S_NAMESPACE", "skquad-system"),
		K8sTokenFile:            envOr("SKQUAD_K8S_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		K8sCAFile:               envOr("SKQUAD_K8S_CA_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"),
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
		DefaultAgentStorageSize: envOr("SKQUAD_DEFAULT_AGENT_STORAGE_SIZE", "2Gi"),
		MaxAgentStorage:         envOr("SKQUAD_MAX_AGENT_STORAGE", "10Gi"),
		StorageClass:            strings.TrimSpace(os.Getenv("SKQUAD_STORAGE_CLASS")),
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
	if _, err := domain.ParseStorageSize(c.DefaultAgentStorageSize); err != nil {
		return fmt.Errorf("config: SKQUAD_DEFAULT_AGENT_STORAGE_SIZE: %w", err)
	}
	if _, err := domain.ParseStorageSize(c.MaxAgentStorage); err != nil {
		return fmt.Errorf("config: SKQUAD_MAX_AGENT_STORAGE: %w", err)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envList parses a comma-separated env var into a trimmed, non-empty slice.
func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// AdminGroupMatched reports whether any of the caller's IdP groups is bound to
// platform_admin through SKQUAD_OIDC_ADMIN_GROUPS. Matching is case-insensitive
// on both sides.
func (c *Config) AdminGroupMatched(groups []string) bool {
	if len(c.OIDCAdminGroups) == 0 || len(groups) == 0 {
		return false
	}
	wanted := make(map[string]struct{}, len(c.OIDCAdminGroups))
	for _, g := range c.OIDCAdminGroups {
		if k := strings.ToLower(strings.TrimSpace(g)); k != "" {
			wanted[k] = struct{}{}
		}
	}
	for _, g := range groups {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(g))]; ok {
			return true
		}
	}
	return false
}

// BreakGlassConfig converts the flat env-derived settings into a breakglass.Config,
// parsing the CIDR allowlist. Returns an error rather than a half-built config so
// a typo in an allowlist entry fails loudly at startup instead of silently
// widening or closing access at request time.
func (c *Config) BreakGlassConfig() (*breakglass.Config, error) {
	bg := &breakglass.Config{
		Enabled:      c.BreakGlassEnabled,
		Username:     c.BreakGlassUsername,
		PasswordHash: c.BreakGlassPasswordHash,
		JWTKey:       []byte(c.BreakGlassJWTKey),
		TokenTTL:     c.BreakGlassTokenTTL,
		MaxAttempts:  c.BreakGlassMaxAttempts,
		Window:       c.BreakGlassWindow,
	}
	for _, raw := range c.BreakGlassAllowedCIDRs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "/") {
			// Bare IP: treat as a single-host prefix.
			if strings.Contains(raw, ":") {
				raw += "/128"
			} else {
				raw += "/32"
			}
		}
		_, netBlock, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("SKQUAD_BREAKGLASS_ALLOWED_CIDRS: invalid CIDR %q", raw)
		}
		bg.AllowedCIDRs = append(bg.AllowedCIDRs, netBlock)
	}
	return bg, nil
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
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
