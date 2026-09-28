// Shared Kubernetes HTTP transport for the control-plane's raw-API
// clients (CRWriter, SecretStore). Extracted from NewCRWriter (S-155)
// so every in-cluster client gets identical TLS/token handling.

package kube

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
)

// newK8sHTTPClient builds an HTTP client honoring the control-plane's
// K8s TLS config: explicit insecure dev opt-in, projected CA file, or
// system trust store fallback for out-of-cluster dev.
func newK8sHTTPClient(cfg *config.Config) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.K8sInsecure {
		// #nosec G402 -- explicit opt-in dev mode via SKQUAD_K8S_INSECURE;
		// production paths use the projected CA branch below.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} else if cfg.K8sCAFile != "" {
		// Trust the cluster CA (projected service-account CA by default). The
		// system trust store never contains the cluster's signing CA, so
		// without this every in-cluster API call fails x509 verification.
		pem, err := os.ReadFile(cfg.K8sCAFile)
		if err == nil {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("kube: no certificates parsed from CA file %s", cfg.K8sCAFile)
			}
			transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("kube: read CA file: %w", err)
		}
		// Missing CA file: fall back to the system trust store (out-of-cluster dev).
	}
	return &http.Client{Transport: transport}, nil
}
