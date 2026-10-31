// Package config loads and validates the k8s-mcp server configuration from
// the environment (MIG-7 / S-313).
//
// Fail-closed posture: the server refuses to start without an auth token and
// without an explicit, non-empty list of allowed namespaces. Wildcards are
// never accepted — scoping must be an explicit enumeration so a missing
// value can never silently widen access.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// In-cluster defaults (Kubernetes service-account mount layout).
const (
	DefaultAddr          = ":8081"
	DefaultAPIURL        = "https://kubernetes.default.svc:443"
	DefaultCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	DefaultSATokenPath   = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultArgoNamespace = "argocd"
	DefaultMaxLogLines   = 500
	DefaultTimeout       = 30 * time.Second
)

// dnsLabel is the Kubernetes RFC-1123 label shape used for namespaces and
// object names. Deliberately excludes '*' and '.' so wildcards and path-ish
// values can never enter the allowlists.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Config is the fully-validated runtime configuration.
type Config struct {
	// Addr is the listen address for the MCP HTTP server.
	Addr string
	// AuthToken is the bearer token clients must present (fits the skquad
	// mcp resource auth_kind=bearer custody model).
	AuthToken string
	// APIURL is the Kubernetes API server base URL.
	APIURL string
	// CAPath is the PEM bundle used to verify the API server.
	CAPath string
	// SATokenPath is the ServiceAccount token file used against the API.
	SATokenPath string
	// AllowedNamespaces is the explicit set of namespaces every read tool
	// may touch. Anything outside is denied at call time.
	AllowedNamespaces []string
	// ArgoNamespace is where ArgoCD Application CRs live.
	ArgoNamespace string
	// ArgoAllowedApps is the explicit set of ArgoCD application names
	// that status/refresh/sync may target. Empty means gitops tools deny
	// everything (fail-closed).
	ArgoAllowedApps []string
	// MaxLogLines is the hard cap on pod-log tail length.
	MaxLogLines int
	// Timeout bounds every upstream Kubernetes API call.
	Timeout time.Duration
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		Addr:              envOr("K8SMCP_ADDR", DefaultAddr),
		APIURL:            envOr("K8SMCP_API_URL", DefaultAPIURL),
		CAPath:            envOr("K8SMCP_CA_PATH", DefaultCAPath),
		SATokenPath:       envOr("K8SMCP_SA_TOKEN_PATH", DefaultSATokenPath),
		ArgoNamespace:     envOr("K8SMCP_ARGOCD_NAMESPACE", DefaultArgoNamespace),
		MaxLogLines:       DefaultMaxLogLines,
		Timeout:           DefaultTimeout,
		AllowedNamespaces: splitList(os.Getenv("K8SMCP_ALLOWED_NAMESPACES")),
		ArgoAllowedApps:   splitList(os.Getenv("K8SMCP_ARGO_ALLOWED_APPS")),
	}

	token, err := loadToken()
	if err != nil {
		return nil, err
	}
	c.AuthToken = token

	if v := strings.TrimSpace(os.Getenv("K8SMCP_MAX_LOG_LINES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("config: K8SMCP_MAX_LOG_LINES must be a positive integer")
		}
		c.MaxLogLines = n
	}
	if v := strings.TrimSpace(os.Getenv("K8SMCP_TIMEOUT_SECONDS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("config: K8SMCP_TIMEOUT_SECONDS must be a positive integer")
		}
		c.Timeout = time.Duration(n) * time.Second
	}

	// Fail-closed: an empty namespace allowlist would mean "nothing is
	// scoped", which is the opposite of the design. Refuse to start.
	if len(c.AllowedNamespaces) == 0 {
		return nil, fmt.Errorf("config: K8SMCP_ALLOWED_NAMESPACES must list at least one namespace (fail-closed)")
	}
	for _, ns := range c.AllowedNamespaces {
		if !dnsLabel.MatchString(ns) {
			return nil, fmt.Errorf("config: invalid namespace %q in K8SMCP_ALLOWED_NAMESPACES (must be a RFC-1123 label; wildcards not allowed)", ns)
		}
	}
	if !dnsLabel.MatchString(c.ArgoNamespace) {
		return nil, fmt.Errorf("config: invalid K8SMCP_ARGOCD_NAMESPACE %q", c.ArgoNamespace)
	}
	for _, app := range c.ArgoAllowedApps {
		if !dnsLabel.MatchString(app) {
			return nil, fmt.Errorf("config: invalid app %q in K8SMCP_ARGO_ALLOWED_APPS (must be a RFC-1123 label; wildcards not allowed)", app)
		}
	}
	return c, nil
}

// loadToken resolves the MCP bearer token from K8SMCP_AUTH_TOKEN_FILE
// (preferred, mounted secret) or K8SMCP_AUTH_TOKEN. Exactly-one-of is not
// required; the file wins when both are set. Empty is a fatal error.
func loadToken() (string, error) {
	if path := strings.TrimSpace(os.Getenv("K8SMCP_AUTH_TOKEN_FILE")); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("config: cannot read K8SMCP_AUTH_TOKEN_FILE: %w", err)
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("config: K8SMCP_AUTH_TOKEN_FILE is empty (fail-closed)")
	}
	if t := strings.TrimSpace(os.Getenv("K8SMCP_AUTH_TOKEN")); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("config: no auth token configured (set K8SMCP_AUTH_TOKEN_FILE or K8SMCP_AUTH_TOKEN; fail-closed)")
}

// splitList parses a comma-separated env value into trimmed, non-empty
// entries.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
