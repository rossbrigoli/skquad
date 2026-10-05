package httpapi

// TG-3 façade tests: POST /api/v1/tools/web_fetch must keep the exact
// runtime contract whether the fetch runs on the legacy CP path or is
// forwarded to the tool gateway. Gateway failures surface as the CP's
// own 502-class tool errors.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
)

func facadeConfigHandler(t *testing.T, gwURL string, viaGateway bool) (http.Handler, *fakeCRWriter) {
	t.Helper()
	cfg := testConfig()
	cfg.ToolGatewayURL = gwURL
	cfg.WebFetchViaGateway = viaGateway
	fw := &fakeCRWriter{}
	return newServer(cfg, storage.NewMemoryStore(), serverDeps{crWriter: fw}), fw
}

// gatewayStub mimics the tool gateway's /v1/web/fetch response shape,
// including an additive field (extractedText) the façade must drop.
func gatewayStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/web/fetch", r.URL.Path)
		require.NotEmpty(t, r.Header.Get("X-Skquad-Agent-ID"), "façade must pass the agent id")
		require.NotEmpty(t, r.Header.Get("Authorization"), "façade must pass the bearer credential")
		var in map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.NotEmpty(t, in["url"])
		handler(w, r)
	}))
}

func TestWebFetchFacadeHappyPathParity(t *testing.T) {
	payload := "parity-body"
	gw := gatewayStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url":           "https://example.com/page",
			"status":        200,
			"contentType":   "text/html; charset=utf-8",
			"bodyB64":       base64.StdEncoding.EncodeToString([]byte(payload)),
			"truncated":     false,
			"extractedText": "extra field the façade must drop",
		})
	})
	defer gw.Close()

	handler, fw := facadeConfigHandler(t, gw.URL, true)
	agentID, token := enableWebFetch(t, handler, fw, nil)

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": "https://example.com/page"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// Exact legacy contract: five keys, no gateway extras.
	require.ElementsMatch(t, []string{"url", "status", "contentType", "bodyB64", "truncated"}, keysOf(out))
	require.Equal(t, "https://example.com/page", out["url"])
	require.Equal(t, float64(200), out["status"])
	require.Equal(t, "text/html; charset=utf-8", out["contentType"])
	require.Equal(t, false, out["truncated"])
	body, err := base64.StdEncoding.DecodeString(out["bodyB64"].(string))
	require.NoError(t, err)
	require.Equal(t, payload, string(body))
	require.NotContains(t, rec.Body.String(), "extractedText")
}

func TestWebFetchFacadeDeniedBecomes502(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "denied", "message": "ssrf_blocked"})
		w.WriteHeader(http.StatusForbidden)
	}))
	defer gw.Close()

	handler, fw := facadeConfigHandler(t, gw.URL, true)
	agentID, token := enableWebFetch(t, handler, fw, nil)

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": "http://169.254.169.254/"}))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	var body map[string]map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "fetch_failed", body["error"]["code"])
}

func TestWebFetchFacadeGatewayDownBecomes502(t *testing.T) {
	handler, fw := facadeConfigHandler(t, "http://127.0.0.1:1", true)
	agentID, token := enableWebFetch(t, handler, fw, nil)

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": "https://example.com"}))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "fetch_failed")
}

func TestWebFetchFacadeFlagOffUsesLegacyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("legacy-path"))
	}))
	defer upstream.Close()

	// Flag off → legacy CP-side guarded fetch (allowPrivate for the
	// loopback upstream), exactly as before TG-3.
	handler, fw := facadeConfigHandler(t, "http://127.0.0.1:1", false)
	agentID, token := enableWebFetch(t, handler, fw, map[string]any{"allowPrivateNetwork": true})

	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": upstream.URL}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	body, _ := base64.StdEncoding.DecodeString(out["bodyB64"].(string))
	require.Equal(t, "legacy-path", string(body))
}

func TestWebFetchFacadeEmptyURLMeansLegacy(t *testing.T) {
	// ToolGatewayURL empty (default) even with the flag on → legacy path.
	// Proven by the loopback upstream succeeding under allowPrivate.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("empty-url-legacy"))
	}))
	defer upstream.Close()

	cfg := testConfig()
	require.Empty(t, cfg.ToolGatewayURL, "test default must be empty")
	cfg.WebFetchViaGateway = true // flag on, no URL → legacy fallback
	fw := &fakeCRWriter{}
	handler := newServer(cfg, storage.NewMemoryStore(), serverDeps{crWriter: fw})

	agentID, token := enableWebFetch(t, handler, fw, map[string]any{"allowPrivateNetwork": true})
	rec := serve(handler, newAgentRequest(agentID, token, http.MethodPost, pathWebFetch,
		map[string]any{"url": upstream.URL}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestConfigGatewayDefaults(t *testing.T) {
	t.Setenv("SKQUAD_ADDR", ":0")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.True(t, cfg.WebFetchViaGateway, "gateway path must be the default")
	require.Empty(t, cfg.ToolGatewayURL, "no gateway URL by default (legacy fallback)")

	t.Setenv("SKQUAD_TOOL_GATEWAY_URL", "http://tool-gateway.skquad-system.svc.cluster.local:8080/")
	t.Setenv("SKQUAD_WEBFETCH_VIA_GATEWAY", "false")
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, "http://tool-gateway.skquad-system.svc.cluster.local:8080", cfg.ToolGatewayURL)
	require.False(t, cfg.WebFetchViaGateway)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
