package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("K8SMCP_AUTH_TOKEN", "test-token")
	t.Setenv("K8SMCP_ALLOWED_NAMESPACES", "skquad-system")
	t.Setenv("K8SMCP_ARGO_ALLOWED_APPS", "skquad")
}

func TestLoadFailsClosedWithoutToken(t *testing.T) {
	t.Setenv("K8SMCP_ALLOWED_NAMESPACES", "skquad-system")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no auth token")
}

func TestLoadFailsClosedWithoutNamespaces(t *testing.T) {
	t.Setenv("K8SMCP_AUTH_TOKEN", "test-token")
	t.Setenv("K8SMCP_ALLOWED_NAMESPACES", "")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one namespace")
}

func TestLoadRejectsWildcardNamespace(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("K8SMCP_ALLOWED_NAMESPACES", "skquad-system,*")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid namespace")
}

func TestLoadRejectsPathishNamespace(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("K8SMCP_ALLOWED_NAMESPACES", "kube-system/../default")
	_, err := Load()
	require.Error(t, err)
}

func TestLoadRejectsWildcardApp(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("K8SMCP_ARGO_ALLOWED_APPS", "*")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid app")
}

func TestLoadTokenFromFile(t *testing.T) {
	setBaseEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("file-token\n"), 0o600))
	t.Setenv("K8SMCP_AUTH_TOKEN_FILE", path)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "file-token", cfg.AuthToken)
}

func TestLoadEmptyTokenFileFails(t *testing.T) {
	setBaseEnv(t)
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("  \n"), 0o600))
	t.Setenv("K8SMCP_AUTH_TOKEN_FILE", path)
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestLoadHappyPath(t *testing.T) {
	setBaseEnv(t)
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"skquad-system"}, cfg.AllowedNamespaces)
	assert.Equal(t, []string{"skquad"}, cfg.ArgoAllowedApps)
	assert.Equal(t, "argocd", cfg.ArgoNamespace)
	assert.Equal(t, DefaultMaxLogLines, cfg.MaxLogLines)
}

func TestLoadEmptyArgoAppsAllowedButMeansDenyAll(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("K8SMCP_ARGO_ALLOWED_APPS", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.ArgoAllowedApps)
}

func TestLoadInvalidMaxLogLines(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("K8SMCP_MAX_LOG_LINES", "0")
	_, err := Load()
	require.Error(t, err)
}
