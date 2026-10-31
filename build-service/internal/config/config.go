// Package config loads and validates the build-service configuration.
//
// The service is deliberately config-driven: every governance boundary
// (destination registry prefixes, kaniko image, job budgets, quotas)
// lives here so operators can tighten policy without code changes.
// Defaults are fail-closed: required fields have no permissive fallback.
package config

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Config is the full service configuration (YAML at --config path).
type Config struct {
	// Listen is the HTTP listen address, e.g. ":8080".
	Listen string `json:"listen" yaml:"listen"`
	// Namespace is the dedicated build namespace where kaniko Jobs run
	// (expected: skquad-builds). The service's RBAC is scoped to it.
	Namespace string `json:"namespace" yaml:"namespace"`
	// KanikoImage is the kaniko executor image used for build Jobs.
	KanikoImage string `json:"kaniko_image" yaml:"kaniko_image"`
	// KanikoServiceAccount is the build-namespace SA for kaniko pods.
	// It holds NO RBAC (automount disabled): kaniko never talks to the API.
	KanikoServiceAccount string `json:"kaniko_service_account" yaml:"kaniko_service_account"`
	// RegistrySecret is the build-namespace docker-config Secret mounted
	// into kaniko pods at /kaniko/.docker for pushing allowlisted images.
	// Agents never see its contents.
	RegistrySecret string `json:"registry_secret" yaml:"registry_secret"`
	// PrincipalsFile is the mounted JSON file of build principals
	// (per-agent identities, token hashes, allowlists).
	PrincipalsFile string `json:"principals_file" yaml:"principals_file"`
	// CPPolicyURL is the control-plane base URL for the internal policy
	// read API (GET {CP}/internal/v1/policy?agent=). Empty disables the
	// live git-grant-scope check (static allowlists only) — allowed for
	// dev, NOT recommended for production.
	CPPolicyURL string `json:"cp_policy_url" yaml:"cp_policy_url"`
	// CPPolicyTTLSeconds bounds policy cache freshness (fail-closed on
	// expiry + fetch error, same posture as the tool-gateway policy cache).
	CPPolicyTTLSeconds int `json:"cp_policy_ttl_seconds" yaml:"cp_policy_ttl_seconds"`
	// CPPolicyTimeoutSeconds bounds each CP fetch.
	CPPolicyTimeoutSeconds int `json:"cp_policy_timeout_seconds" yaml:"cp_policy_timeout_seconds"`
	// DestinationsAllow are global destination prefixes (e.g.
	// "ghcr.io/rossbrigoli/agent-builds/"). A request destination must
	// start with one of these AND with one of the principal's prefixes.
	DestinationsAllow []string `json:"destinations_allow" yaml:"destinations_allow"`
	// GitHostsAllow is the SSRF floor for ls-remote probes: only these
	// canonical git hosts may be probed (e.g. ["github.com"]).
	GitHostsAllow []string `json:"git_hosts_allow" yaml:"git_hosts_allow"`
	// Limits are per-principal quotas.
	Limits Limits `json:"limits" yaml:"limits"`
	// Job bounds the kaniko Job lifecycle and resources.
	Job JobConfig `json:"job" yaml:"job"`
}

// Limits are per-principal build quotas (enforced best-effort in-process;
// concurrent-build counting is authoritative via the Jobs API).
type Limits struct {
	MaxConcurrentPerPrincipal int `json:"max_concurrent_per_principal" yaml:"max_concurrent_per_principal"`
	MaxPerHourPerPrincipal    int `json:"max_per_hour_per_principal" yaml:"max_per_hour_per_principal"`
}

// JobConfig bounds each kaniko Job.
type JobConfig struct {
	ActiveDeadlineSeconds   int64  `json:"active_deadline_seconds" yaml:"active_deadline_seconds"`
	TTLSecondsAfterFinished int64  `json:"ttl_seconds_after_finished" yaml:"ttl_seconds_after_finished"`
	CPURequest              string `json:"cpu_request" yaml:"cpu_request"`
	MemoryRequest           string `json:"memory_request" yaml:"memory_request"`
}

// Load reads, parses (strict) and validates the config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate enforces required fields and fail-closed defaults.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("config: listen is required")
	}
	if c.Namespace == "" {
		return fmt.Errorf("config: namespace is required")
	}
	if c.KanikoImage == "" {
		return fmt.Errorf("config: kaniko_image is required")
	}
	if c.PrincipalsFile == "" {
		return fmt.Errorf("config: principals_file is required")
	}
	if len(c.DestinationsAllow) == 0 {
		return fmt.Errorf("config: destinations_allow must not be empty (default-deny)")
	}
	for _, d := range c.DestinationsAllow {
		if strings.TrimSpace(d) == "" || d == "/" || !strings.HasSuffix(d, "/") {
			return fmt.Errorf("config: destinations_allow entry %q must be a non-empty prefix ending in '/'", d)
		}
	}
	if len(c.GitHostsAllow) == 0 {
		return fmt.Errorf("config: git_hosts_allow must not be empty (default-deny for revision probes)")
	}
	if c.Limits.MaxConcurrentPerPrincipal <= 0 {
		return fmt.Errorf("config: limits.max_concurrent_per_principal must be > 0")
	}
	if c.Limits.MaxPerHourPerPrincipal <= 0 {
		return fmt.Errorf("config: limits.max_per_hour_per_principal must be > 0")
	}
	if c.Job.ActiveDeadlineSeconds <= 0 {
		return fmt.Errorf("config: job.active_deadline_seconds must be > 0")
	}
	if c.Job.TTLSecondsAfterFinished <= 0 {
		return fmt.Errorf("config: job.ttl_seconds_after_finished must be > 0")
	}
	if c.CPPolicyURL != "" {
		if !strings.HasPrefix(c.CPPolicyURL, "http://") && !strings.HasPrefix(c.CPPolicyURL, "https://") {
			return fmt.Errorf("config: cp_policy_url must be an http(s) URL")
		}
		if c.CPPolicyTTLSeconds <= 0 {
			return fmt.Errorf("config: cp_policy_ttl_seconds must be > 0 when cp_policy_url is set")
		}
	}
	if c.CPPolicyTimeoutSeconds <= 0 {
		c.CPPolicyTimeoutSeconds = 5
	}
	if c.KanikoServiceAccount == "" {
		c.KanikoServiceAccount = "skquad-kaniko"
	}
	return nil
}
