package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/storage"
	"golang.org/x/crypto/argon2"
)

const (
	bgPassword = "correct-horse-battery-staple-breakglass"
	bgUser     = "breakglass"
)

func bgPHC(password string) string {
	salt := []byte("0123456789abcdef")
	hash := argon2.IDKey([]byte(password), salt, 3, 65536, 2, 32)
	return "$argon2id$v=19$m=65536,t=3,p=2$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(hash)
}

// bgConfig returns an OIDC-mode config with break-glass switched on. OIDC mode is
// deliberate: it proves the break-glass path works while the normal human path is
// still locked behind Dex.
func bgConfig(enabled bool) *config.Config {
	cfg := testConfig()
	cfg.AuthMode = config.AuthOIDC
	cfg.IssuerURL = "https://cloud.rossbrigoli.com/auth"
	cfg.Audience = "skquad-v2"
	cfg.BreakGlassEnabled = enabled
	cfg.BreakGlassUsername = bgUser
	cfg.BreakGlassPasswordHash = bgPHC(bgPassword)
	cfg.BreakGlassJWTKey = strings.Repeat("a", 32)
	cfg.BreakGlassAllowedCIDRs = []string{"192.168.68.0/24", "100.64.0.0/10"}
	cfg.BreakGlassMaxAttempts = 3
	cfg.BreakGlassWindow = 15 * time.Minute
	cfg.BreakGlassTokenTTL = time.Hour
	return cfg
}

// bgPost issues a request with an explicit peer address and headers.
func bgPost(t *testing.T, h http.Handler, path string, body any, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var payload strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		payload = *strings.NewReader(string(b))
	} else {
		payload = *strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, path, &payload)
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bgGet(t *testing.T, h http.Handler, path, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBreakGlassDisabledIsNotFound(t *testing.T) {
	handler := NewWithOIDCAuthenticator(bgConfig(false), storage.NewMemoryStore(), fakeOIDC{})

	rec := bgPost(t, handler, "/api/v1/auth/breakglass/login",
		map[string]string{"username": bgUser, "password": bgPassword}, "192.168.68.5:1234", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled login status = %d, want 404 (must not be advertised)", rec.Code)
	}

	rec = bgGet(t, handler, "/api/v1/auth/breakglass/status", "192.168.68.5:1234", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["enabled"] != false {
		t.Fatalf("status body = %v, want enabled=false", body)
	}
}

func TestBreakGlassRejectsCloudflareFrontedRequest(t *testing.T) {
	handler := NewWithOIDCAuthenticator(bgConfig(true), storage.NewMemoryStore(), fakeOIDC{})

	// Even with a LAN-looking X-Forwarded-For, the CF-Ray header proves the
	// request came through the public tunnel. Credentials are correct: it must
	// still be refused.
	rec := bgPost(t, handler, "/api/v1/auth/breakglass/login",
		map[string]string{"username": bgUser, "password": bgPassword},
		"10.42.0.7:40000",
		map[string]string{
			"Cf-Ray":          "8f2c1d3e4a5b-ADL",
			"X-Forwarded-For": "192.168.68.5",
			"X-Real-IP":       "192.168.68.5",
		})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Cloudflare-fronted login = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal LAN/Tailscale") {
		t.Logf("body: %s", rec.Body.String())
	}
}

func TestBreakGlassRejectsNonAllowlistedSource(t *testing.T) {
	handler := NewWithOIDCAuthenticator(bgConfig(true), storage.NewMemoryStore(), fakeOIDC{})

	rec := bgPost(t, handler, "/api/v1/auth/breakglass/login",
		map[string]string{"username": bgUser, "password": bgPassword},
		"203.0.113.77:44444", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-allowlisted login = %d, want 403", rec.Code)
	}
}

func TestBreakGlassRejectsBadCredentials(t *testing.T) {
	handler := NewWithOIDCAuthenticator(bgConfig(true), storage.NewMemoryStore(), fakeOIDC{})

	rec := bgPost(t, handler, "/api/v1/auth/breakglass/login",
		map[string]string{"username": bgUser, "password": "wrong-password-entirely"},
		"192.168.68.5:1234", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad credentials = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), bgPassword) {
		t.Fatal("response leaked the password")
	}
}

func TestBreakGlassLoginIssuesWorkingAdminToken(t *testing.T) {
	store := storage.NewMemoryStore()
	// fakeOIDC always errors here, so a working /auth/me proves the token was
	// verified locally and never touched Dex.
	handler := NewWithOIDCAuthenticator(bgConfig(true), store, fakeOIDC{err: errors.New("dex unreachable")})

	rec := bgPost(t, handler, "/api/v1/auth/breakglass/login",
		map[string]string{"username": bgUser, "password": bgPassword},
		"100.64.9.9:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		User      struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Token == "" || out.ExpiresAt == "" {
		t.Fatalf("missing token/expiry: %+v", out)
	}
	if out.User.Role != "platform_admin" {
		t.Fatalf("role = %q, want platform_admin", out.User.Role)
	}

	// The token authenticates against a human-authenticated endpoint in OIDC mode.
	me := bgGet(t, handler, "/api/v1/auth/me", "100.64.9.9:5555",
		map[string]string{"Authorization": "Bearer " + out.Token})
	if me.Code != http.StatusOK {
		t.Fatalf("auth/me with break-glass token = %d, want 200; body=%s", me.Code, me.Body.String())
	}
	var meBody map[string]any
	if err := json.Unmarshal(me.Body.Bytes(), &meBody); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if meBody["role"] != "platform_admin" {
		t.Fatalf("me role = %v, want platform_admin", meBody["role"])
	}

	// And it can actually perform an admin-only mutation, which is the whole point.
	dep := bgPost(t, handler, "/api/v1/registry/llm-providers", map[string]any{
		"name":        "bg-provider",
		"kind":        "openai",
		"base_url":    "https://api.example.com/v1",
		"api_key_ref": "k8s://secret/bg-key",
	}, "100.64.9.9:5555", map[string]string{"Authorization": "Bearer " + out.Token})
	if dep.Code != http.StatusCreated && dep.Code != http.StatusOK {
		t.Fatalf("admin mutation with break-glass token = %d, want 200/201; body=%s", dep.Code, dep.Body.String())
	}
}

func TestBreakGlassTokenFromAnotherKeyRejected(t *testing.T) {
	handler := NewWithOIDCAuthenticator(bgConfig(true), storage.NewMemoryStore(), fakeOIDC{})
	rec := bgPost(t, handler, "/api/v1/auth/breakglass/login",
		map[string]string{"username": bgUser, "password": bgPassword}, "192.168.68.5:1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	token, _ := out["token"].(string)

	// A second server with a different JWT key must reject the first one's token.
	otherCfg := bgConfig(true)
	otherCfg.BreakGlassJWTKey = strings.Repeat("b", 32)
	other := NewWithOIDCAuthenticator(otherCfg, storage.NewMemoryStore(), fakeOIDC{})

	me := bgGet(t, other, "/api/v1/auth/me", "192.168.68.5:1",
		map[string]string{"Authorization": "Bearer " + token})
	if me.Code != http.StatusUnauthorized {
		t.Fatalf("cross-key token = %d, want 401", me.Code)
	}
}

func TestBreakGlassRateLimitsBruteForce(t *testing.T) {
	handler := NewWithOIDCAuthenticator(bgConfig(true), storage.NewMemoryStore(), fakeOIDC{})

	// MaxAttempts is 3; the 4th attempt from the same IP is refused even before
	// credentials are checked.
	for i := 1; i <= 3; i++ {
		rec := bgPost(t, handler, "/api/v1/auth/breakglass/login",
			map[string]string{"username": bgUser, "password": "wrong"}, "192.168.68.99:2", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	rec := bgPost(t, handler, "/api/v1/auth/breakglass/login",
		map[string]string{"username": bgUser, "password": bgPassword}, "192.168.68.99:2", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th attempt = %d, want 429 (correct password must not bypass the limit)", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After header")
	}
}

func TestBreakGlassStartupValidationPanicsOnBadConfig(t *testing.T) {
	// Enabled but missing the hash: must fail loudly at construction rather than
	// run with a silently broken emergency path.
	cfg := bgConfig(true)
	cfg.BreakGlassPasswordHash = ""
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on enabled-but-misconfigured break-glass")
		}
	}()
	_ = NewWithOIDCAuthenticator(cfg, storage.NewMemoryStore(), fakeOIDC{})
}
