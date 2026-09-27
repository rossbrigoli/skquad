package httpapi

// BT-6: web_fetch control-plane proxy tests. The proxy exists because
// agent pods have no internet egress (default-deny NetworkPolicy); the
// guarded fetch runs here. Tests cover: SSRF dial-guard address table,
// disabled-tool 403, credential requirement, request validation, the
// allowPrivateNetwork happy path, size truncation, and the redirect cap.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const pathWebFetch = "/api/v1/tools/web_fetch"

func TestBlockedDestAddrTable(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80",
		"127.0.0.53:53",
		"10.0.0.5:443",
		"172.16.9.9:80",
		"192.168.68.131:22",
		"169.254.169.254:80", // cloud metadata
		"100.64.0.1:443",     // CGNAT
		"100.127.255.255:80", // CGNAT upper edge
		"fc00::1:443",        // IPv6 ULA
		"fd12:3456::7:80",    // IPv6 ULA
		"fe80::1:80",         // link-local
		"0.0.0.0:80",
		":::80",
		"not-an-ip:80",      // malformed fails closed
		"999.999.999.999:9", // malformed fails closed
	}
	for _, addr := range blocked {
		t.Run("blocked/"+addr, func(t *testing.T) {
			require.True(t, blockedDestAddr(addr), "%s must be blocked", addr)
		})
	}

	allowed := []string{
		"93.184.216.34:443",
		"8.8.8.8:443",
		"[2606:2800:220:1:248:1893:25c8:1946]:443",
		"[2001:4860:4860::8888]:53",
	}
	for _, addr := range allowed {
		t.Run("allowed/"+addr, func(t *testing.T) {
			require.False(t, blockedDestAddr(addr), "%s must be allowed", addr)
		})
	}
}

// enableWebFetch boots an agent and enables the tool with the given policy.
func enableWebFetch(t *testing.T, handler http.Handler, fw *fakeCRWriter, policy map[string]any) (string, string) {
	t.Helper()
	agentID, token := agentWithCredential(t, handler, fw, "wf-squad", "wf-agent")
	body := map[string]any{"enabled": true}
	if policy != nil {
		body["policy"] = policy
	}
	var view builtinToolAdminView
	doJSON(t, handler, http.MethodPatch, pathAdminTools+"/web_fetch", body, http.StatusOK, &view)
	return agentID, token
}

func TestWebFetchDisabledReturns403(t *testing.T) {
	handler, fw := toolsHandler(t, nil)
	// Fresh store: web_fetch seeded disabled. Real agent credentials so the
	// request reaches the handler (unknown agents 404 at the middleware).
	agentID, token := agentWithCredential(t, handler, fw, "wf-off-squad", "wf-off-agent")
	var body map[string]map[string]string
	doAgentJSON(t, handler, agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": "https://example.com"}, http.StatusForbidden, &body)
	require.Equal(t, "tool_disabled", body["error"]["code"])
}

func TestWebFetchRequiresAgentCredential(t *testing.T) {
	handler, fw := toolsHandler(t, nil)
	_, _ = enableWebFetch(t, handler, fw, nil)
	rec := serve(handler, newAgentRequest("", "", http.MethodPost, pathWebFetch, map[string]any{"url": "https://example.com"}))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestWebFetchValidation(t *testing.T) {
	handler, fw := toolsHandler(t, nil)
	agentID, token := enableWebFetch(t, handler, fw, nil)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing url", map[string]any{}},
		{"empty url", map[string]any{"url": "   "}},
		{"ftp scheme", map[string]any{"url": "ftp://example.com/x"}},
		{"no host", map[string]any{"url": "http:///path"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch, tc.body))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}

func TestWebFetchSSRFBlocksLoopbackByDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secret-internal")
	}))
	defer upstream.Close()

	handler, fw := toolsHandler(t, nil)
	agentID, token := enableWebFetch(t, handler, fw, nil) // default: allowPrivateNetwork=false

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": upstream.URL + "/secret"}))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "fetch_failed")
	require.NotContains(t, rec.Body.String(), "secret-internal")
}

func TestWebFetchAllowPrivateNetworkHappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><h1>Hi</h1><p>content</p></body></html>")
	}))
	defer upstream.Close()

	handler, fw := toolsHandler(t, nil)
	agentID, token := enableWebFetch(t, handler, fw, map[string]any{"allowPrivateNetwork": true})

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": upstream.URL + "/page"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out struct {
		URL         string `json:"url"`
		Status      int    `json:"status"`
		ContentType string `json:"contentType"`
		BodyB64     string `json:"bodyB64"`
		Truncated   bool   `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, 200, out.Status)
	require.Contains(t, out.ContentType, "text/html")
	require.False(t, out.Truncated)
	body, err := base64.StdEncoding.DecodeString(out.BodyB64)
	require.NoError(t, err)
	require.Contains(t, string(body), "<h1>Hi</h1>")
}

func TestWebFetchTruncatesAtMaxBytes(t *testing.T) {
	payload := strings.Repeat("x", 5000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, payload)
	}))
	defer upstream.Close()

	handler, fw := toolsHandler(t, nil)
	agentID, token := enableWebFetch(t, handler, fw,
		map[string]any{"allowPrivateNetwork": true, "maxBytes": 100})

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": upstream.URL}))
	require.Equal(t, http.StatusOK, rec.Code)

	var out struct {
		BodyB64   string `json:"bodyB64"`
		Truncated bool   `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.True(t, out.Truncated)
	body, err := base64.StdEncoding.DecodeString(out.BodyB64)
	require.NoError(t, err)
	require.Len(t, body, 100)
}

func TestWebFetchRedirectCap(t *testing.T) {
	hops := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	handler, fw := toolsHandler(t, nil)
	agentID, token := enableWebFetch(t, handler, fw, map[string]any{"allowPrivateNetwork": true})

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": srv.URL}))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "too many redirects")
	// initial + 3 followed = 4 requests, then the cap stops it.
	require.Equal(t, 4, hops)
}

func TestWebFetchFollowsRedirectsWithinCap(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "arrived")
			return
		}
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	}))
	defer srv.Close()

	handler, fw := toolsHandler(t, nil)
	agentID, token := enableWebFetch(t, handler, fw, map[string]any{"allowPrivateNetwork": true})

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": srv.URL + "/start"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		URL     string `json:"url"`
		BodyB64 string `json:"bodyB64"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	body, _ := base64.StdEncoding.DecodeString(out.BodyB64)
	require.Equal(t, "arrived", string(body))
	require.Contains(t, out.URL, "/final")
}
