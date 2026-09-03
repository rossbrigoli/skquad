package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBearerToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{name: "plain", header: "Bearer abc.def.ghi", wantToken: "abc.def.ghi", wantOK: true},
		{name: "lowercase scheme", header: "bearer abc.def.ghi", wantToken: "abc.def.ghi", wantOK: true},
		{name: "extra whitespace", header: "   Bearer    abc.def.ghi  ", wantToken: "abc.def.ghi", wantOK: true},
		{name: "empty header", header: "", wantOK: false},
		{name: "token only", header: "abc.def.ghi", wantOK: false},
		{name: "wrong scheme", header: "Basic abc.def.ghi", wantOK: false},
		{name: "no token", header: "Bearer", wantOK: false},
		{name: "blank token", header: "Bearer    ", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			token, ok := bearerToken(tc.header)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (token=%q)", ok, tc.wantOK, token)
			}
			if token != tc.wantToken {
				t.Fatalf("token = %q, want %q", token, tc.wantToken)
			}
		})
	}
}

// fakeIssuer is a minimal OIDC provider: discovery document + JWKS for one
// locally generated RSA key. Tokens are signed in-process so tests can control
// issuer, audience, expiry and claims exactly.
type fakeIssuer struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	audience string
}

func newFakeIssuer(t *testing.T, audience string) *fakeIssuer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeIssuer{key: key, kid: "test-key", audience: audience}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"issuer": %q,
			"authorization_endpoint": %q,
			"token_endpoint": %q,
			"jwks_uri": %q,
			"id_token_signing_alg_values_supported": ["RS256"]
		}`, f.server.URL, f.server.URL+"/authorize", f.server.URL+"/token", f.server.URL+"/keys")
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": f.kid,
				"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
			}},
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

type tokenOverrides struct {
	issuer       string
	audience     string
	expired      bool
	notBeforeFar bool
	kid          string
	claims       map[string]any
}

// token builds a signed RS256 JWT. Empty overrides fall back to the fake
// issuer's own values.
func (f *fakeIssuer) token(t *testing.T, subject string, ov tokenOverrides) string {
	t.Helper()

	issuer := ov.issuer
	if issuer == "" {
		issuer = f.server.URL
	}
	audience := ov.audience
	if audience == "" {
		audience = f.audience
	}
	kid := ov.kid
	if kid == "" {
		kid = f.kid
	}

	now := time.Now()
	notBefore := now.Add(-time.Minute)
	expiry := now.Add(5 * time.Minute)
	if ov.notBeforeFar {
		notBefore = now.Add(time.Hour)
	}
	if ov.expired {
		expiry = now.Add(-time.Minute)
	}

	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid}
	payload := map[string]any{
		"iss": issuer,
		"sub": subject,
		"aud": audience,
		"iat": now.Unix(),
		"nbf": notBefore.Unix(),
		"exp": expiry.Unix(),
	}
	for k, v := range ov.claims {
		payload[k] = v
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (f *fakeIssuer) authenticator(t *testing.T) *OIDCAuthenticator {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	authn, err := NewOIDCAuthenticator(ctx, f.server.URL, f.audience)
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	return authn
}

func TestOIDCAuthenticateHappyPath(t *testing.T) {
	audience := "skquad-api"
	f := newFakeIssuer(t, audience)
	authn := f.authenticator(t)

	token := f.token(t, "user-42", tokenOverrides{claims: map[string]any{
		"email":          "  Ada.Example@Example.Test ",
		"email_verified": true,
		"name":           "Ada Example",
	}})

	profile, err := authn.Authenticate(context.Background(), "Bearer "+token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if profile.Subject != "user-42" {
		t.Fatalf("Subject = %q, want user-42", profile.Subject)
	}
	if profile.Issuer != f.server.URL {
		t.Fatalf("Issuer = %q, want %q", profile.Issuer, f.server.URL)
	}
	if profile.Email != "ada.example@example.test" {
		t.Fatalf("Email = %q, want normalized lower-case trimmed address", profile.Email)
	}
	if !profile.EmailVerified {
		t.Fatal("EmailVerified = false, want true")
	}
	if profile.Name != "Ada Example" {
		t.Fatalf("Name = %q", profile.Name)
	}
}

func TestOIDCNameFallbacks(t *testing.T) {
	f := newFakeIssuer(t, "skquad-api")
	authn := f.authenticator(t)
	ctx := context.Background()

	preferred := f.token(t, "user-pref", tokenOverrides{claims: map[string]any{
		"email":              "pref@example.test",
		"name":               "   ",
		"preferred_username": "ada",
	}})
	profile, err := authn.Authenticate(ctx, "Bearer "+preferred)
	if err != nil {
		t.Fatalf("Authenticate(preferred_username): %v", err)
	}
	if profile.Name != "ada" {
		t.Fatalf("Name = %q, want preferred_username fallback", profile.Name)
	}

	emailOnly := f.token(t, "user-email", tokenOverrides{claims: map[string]any{
		"email": "fallback@example.test",
	}})
	profile, err = authn.Authenticate(ctx, "Bearer "+emailOnly)
	if err != nil {
		t.Fatalf("Authenticate(email-only): %v", err)
	}
	if profile.Name != "fallback@example.test" {
		t.Fatalf("Name = %q, want email fallback", profile.Name)
	}

	// email_verified absent means verified (many IdPs omit it).
	omitted := f.token(t, "user-omit", tokenOverrides{claims: map[string]any{
		"email": "omit@example.test",
	}})
	profile, err = authn.Authenticate(ctx, "Bearer "+omitted)
	if err != nil {
		t.Fatalf("Authenticate(omitted email_verified): %v", err)
	}
	if !profile.EmailVerified {
		t.Fatal("EmailVerified = false, want true when claim is omitted")
	}
}

func TestOIDCAuthenticateRejects(t *testing.T) {
	audience := "skquad-api"
	f := newFakeIssuer(t, audience)
	authn := f.authenticator(t)
	ctx := context.Background()

	baseClaims := map[string]any{
		"email":          "ada@example.test",
		"email_verified": true,
		"name":           "Ada",
	}

	cases := []struct {
		name   string
		header string
		token  func() string
	}{
		{
			name:   "missing header",
			header: "",
			token:  func() string { return "" },
		},
		{
			name:   "non-bearer scheme",
			header: "Basic dXNlcjpwYXNz",
			token:  func() string { return "" },
		},
		{
			name:   "wrong audience",
			header: "Bearer",
			token: func() string {
				return f.token(t, "user-1", tokenOverrides{audience: "someone-else", claims: baseClaims})
			},
		},
		{
			name:   "expired",
			header: "Bearer",
			token: func() string {
				return f.token(t, "user-2", tokenOverrides{expired: true, claims: baseClaims})
			},
		},
		{
			name:   "not yet valid",
			header: "Bearer",
			token: func() string {
				return f.token(t, "user-3", tokenOverrides{notBeforeFar: true, claims: baseClaims})
			},
		},
		{
			name:   "wrong issuer",
			header: "Bearer",
			token: func() string {
				return f.token(t, "user-4", tokenOverrides{issuer: "https://evil.example.test", claims: baseClaims})
			},
		},
		{
			name:   "email not verified",
			header: "Bearer",
			token: func() string {
				return f.token(t, "user-5", tokenOverrides{claims: map[string]any{
					"email":          "ada@example.test",
					"email_verified": false,
					"name":           "Ada",
				}})
			},
		},
		{
			name:   "missing email",
			header: "Bearer",
			token: func() string {
				return f.token(t, "user-6", tokenOverrides{claims: map[string]any{
					"email_verified": true,
					"name":           "Ada",
				}})
			},
		},
		{
			name:   "garbage token",
			header: "Bearer",
			token:  func() string { return "not-a-jwt" },
		},
		{
			name:   "tampered signature",
			header: "Bearer",
			token: func() string {
				good := f.token(t, "user-7", tokenOverrides{claims: baseClaims})
				head, _, _ := strings.Cut(good, ".")
				return head + ".eyJzdWIiOiJhdHRhY2tlciJ9.cGFkcGFk"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := tc.header
			if mint := tc.token; mint != nil {
				header += " " + mint()
			}
			if _, err := authn.Authenticate(ctx, header); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("Authenticate error = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestNewOIDCAuthenticatorDiscoveryFailure(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	url := closed.URL
	closed.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := NewOIDCAuthenticator(ctx, url, "skquad-api"); err == nil {
		t.Fatal("NewOIDCAuthenticator against unreachable issuer returned no error")
	}
}
