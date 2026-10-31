// Package k8s is a minimal, scoped Kubernetes REST client for the
// k8s-mcp server (MIG-7 / S-313).
//
// Design constraints (docs/tool-gateway.md §6.3 + card S-313):
//   - No client-go, no kubectl: only net/http against the API server with
//     the pod's own ServiceAccount credentials. The surface is exactly
//     the methods defined here — there is no generic "run this query"
//     escape hatch.
//   - Defense-in-depth deny list: every request path is checked against a
//     hard-coded set of forbidden resources (secrets, configmaps, RBAC
//     objects, nodes, ...). Even if a future tool wired a path wrong, the
//     client refuses to fetch secret-bearing objects.
//   - Namespace scoping is enforced by the caller (tools layer) against
//     the configured allowlist; this client additionally refuses denied
//     resource kinds unconditionally.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// MaxResponseBytes caps a single API response body (2 MiB). Logs are
// additionally bounded by tail_lines upstream.
const MaxResponseBytes = 2 << 20

// deniedResources is the hard-coded deny list, checked against the
// resource segment of every request path. Secrets are the headline
// prohibition; configmaps are denied because they are a common
// secret-by-ignorance store; RBAC/cluster objects are denied because
// reading them aids lateral movement and is out of scope.
var deniedResources = map[string]bool{
	"secrets":                  true,
	"configmaps":               true,
	"serviceaccounts":          true,
	"roles":                    true,
	"rolebindings":             true,
	"clusterroles":             true,
	"clusterrolebindings":      true,
	"nodes":                    true,
	"namespaces":               true,
	"persistentvolumes":        true,
	"persistentvolumeclaims":   true,
	"serviceaccounttokens":     true,
	"selfsubjectaccessreviews": true,
	"tokenreviews":             true,
}

// IsDeniedResource reports whether a bare resource name (e.g. "secrets",
// "configmaps") is on the hard deny list. Exported for tests and for the
// tools layer to pre-check before composing paths.
func IsDeniedResource(resource string) bool {
	return deniedResources[strings.ToLower(strings.TrimSpace(resource))]
}

// Client talks to one Kubernetes API server with one ServiceAccount.
type Client struct {
	apiURL      string
	saTokenPath string
	http        *http.Client
}

// New builds a client that verifies the API server against the PEM bundle
// at caPath and authenticates with the SA token read from saTokenPath on
// every request (kubelet rotates the file; re-reading keeps long-lived
// pods working without caching stale tokens).
func New(apiURL, caPath, saTokenPath string, timeout time.Duration) (*Client, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("k8s: cannot read CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("k8s: CA bundle contained no certificates")
	}
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &Client{
		apiURL:      strings.TrimRight(apiURL, "/"),
		saTokenPath: saTokenPath,
		http:        &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

// guardPath enforces the deny list against a request path. The resource
// segment is the path element following "namespaces/<ns>/" (namespaced)
// or the first element after the API group/version (cluster-scoped).
// Anything whose resource segment is denied never reaches the network.
func guardPath(path string) error {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	resource := ""
	for i, p := range parts {
		if p == "namespaces" && i+1 < len(parts) {
			if i+2 < len(parts) {
				resource = parts[i+2]
			}
			break
		}
	}
	if resource == "" {
		// Cluster-scoped shape: /api/v1/<resource> or /apis/g/v/<resource>.
		switch {
		case len(parts) >= 3 && parts[0] == "api":
			resource = parts[2]
		case len(parts) >= 4 && parts[0] == "apis":
			resource = parts[3]
		}
	}
	// Strip subresource suffix for the check target's sake, but also deny
	// exec/attach/port-forward subresources outright wherever they appear.
	for _, p := range parts {
		switch p {
		case "exec", "attach", "portforward", "port-forward":
			return fmt.Errorf("k8s: denied subresource %q (no exec/attach/port-forward)", p)
		}
	}
	if IsDeniedResource(resource) {
		return fmt.Errorf("k8s: denied resource %q (secrets/configmaps/RBAC and other forbidden kinds are never fetched)", resource)
	}
	return nil
}

// do performs one authenticated request against the API server.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, contentType string) ([]byte, error) {
	if err := guardPath(path); err != nil {
		return nil, err
	}
	token, err := os.ReadFile(c.saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("k8s: cannot read service account token: %w", err)
	}
	u := c.apiURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("k8s: building request")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s: request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("k8s: reading response")
	}
	if len(raw) > MaxResponseBytes {
		return nil, fmt.Errorf("k8s: response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Return the body alongside the error so callers can surface the
		// API server's own reason (e.g. NotFound) to the agent.
		return raw, &APIError{Status: resp.StatusCode, Body: raw}
	}
	return raw, nil
}

// APIError carries a non-2xx response from the API server.
type APIError struct {
	Status int
	Body   []byte
}

func (e *APIError) Error() string {
	var k8sErr struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(e.Body, &k8sErr) == nil && k8sErr.Message != "" {
		return fmt.Sprintf("k8s: api status %d: %s", e.Status, k8sErr.Message)
	}
	return fmt.Sprintf("k8s: api status %d", e.Status)
}

// GetPods lists pods in ns, optionally filtered by a label selector.
func (c *Client) GetPods(ctx context.Context, ns, labelSelector string) ([]byte, error) {
	q := url.Values{}
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	return c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods", ns), q, nil, "")
}

// GetServices lists services in ns.
func (c *Client) GetServices(ctx context.Context, ns string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/services", ns), nil, nil, "")
}

// GetEvents lists events in ns, bounded by limit.
func (c *Client) GetEvents(ctx context.Context, ns string, limit int, fieldSelector string) ([]byte, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprint(limit))
	if fieldSelector != "" {
		q.Set("fieldSelector", fieldSelector)
	}
	return c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/events", ns), q, nil, "")
}

// GetPod fetches a single pod.
func (c *Client) GetPod(ctx context.Context, ns, name string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, name), nil, nil, "")
}

// GetPodLogs fetches container logs (plain text, bounded tail).
func (c *Client) GetPodLogs(ctx context.Context, ns, name, container string, tailLines int) ([]byte, error) {
	q := url.Values{}
	q.Set("tailLines", fmt.Sprint(tailLines))
	q.Set("timestamps", "true")
	if container != "" {
		q.Set("container", container)
	}
	return c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", ns, name), q, nil, "")
}

// GetArgoApp fetches an ArgoCD Application CR.
func (c *Client) GetArgoApp(ctx context.Context, argoNS, name string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, fmt.Sprintf("/apis/argoproj.io/v1alpha1/namespaces/%s/applications/%s", argoNS, name), nil, nil, "")
}

// PatchArgoApp applies a merge patch to an ArgoCD Application CR.
func (c *Client) PatchArgoApp(ctx context.Context, argoNS, name string, patch []byte) ([]byte, error) {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/apis/argoproj.io/v1alpha1/namespaces/%s/applications/%s", argoNS, name), nil, patch, "application/merge-patch+json")
}
