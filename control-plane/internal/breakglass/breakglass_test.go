package breakglass

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	issueTokenErrFormat = "IssueToken: %v"
	testClientIP        = "192.168.68.5"
	testPassphrase      = "s3cret-breakglass-passphrase"
)

const testKey = "0123456789abcdef0123456789abcdef" // 32 bytes

// makePHC builds a valid argon2id PHC string for tests.
func makePHC(t *testing.T, password string) string {
	t.Helper()
	salt := []byte("0123456789abcdef")
	hash := argon2.IDKey([]byte(password), salt, 3, 65536, 2, 32)
	return "$argon2id$v=19$m=65536,t=3,p=2$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(hash)
}

func testAuth(t *testing.T, password string) *Auth {
	t.Helper()
	a, err := New(Config{
		Enabled:      true,
		Username:     "breakglass",
		PasswordHash: makePHC(t, password),
		JWTKey:       []byte(testKey),
		TokenTTL:     time.Hour,
		AllowedCIDRs: []*net.IPNet{parseCIDR(t, "192.168.68.0/24"), parseCIDR(t, "100.64.0.0/10")},
		MaxAttempts:  3,
		Window:       15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func parseCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	return n
}

func TestVerifyCredentials(t *testing.T) {
	a := testAuth(t, testPassphrase)

	if !a.VerifyCredentials("breakglass", testPassphrase) {
		t.Fatal("correct credentials rejected")
	}
	if a.VerifyCredentials("breakglass", "wrong-password") {
		t.Fatal("wrong password accepted")
	}
	if a.VerifyCredentials("not-the-user", testPassphrase) {
		t.Fatal("wrong username accepted")
	}
	// Username comparison is trimmed on the config side but must still match.
	if !a.VerifyCredentials("  breakglass  ", testPassphrase) {
		t.Fatal("trimmed username rejected")
	}
}

func TestVerifyArgon2idRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext-not-a-phc",
		"$argon2i$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA==$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"$argon2id$v=16$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA==$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"$argon2id$v=19$m=notanumber$AAAAAAAAAAAAAAAAAAAAAA==$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	} {
		if ok, err := verifyArgon2id(bad, "anything"); ok || err == nil {
			t.Fatalf("verifyArgon2id(%q) = (%v, %v), want rejection with error", bad, ok, err)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	a := testAuth(t, "passphrase-for-roundtrip")
	now := time.Unix(1_800_000_000, 0)

	token, exp, err := a.IssueToken("uid-1", "breakglass@breakglass.skquad.local", "platform_admin", now)
	if err != nil {
		t.Fatalf(issueTokenErrFormat, err)
	}
	if !exp.After(now) {
		t.Fatalf("expiry %v is not after issue time %v", exp, now)
	}

	claims, err := a.VerifyToken(token, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if claims.ID != "uid-1" || claims.Role != "platform_admin" || claims.Iss != Issuer {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.JTI == "" {
		t.Fatal("jti missing")
	}
}

func TestVerifyTokenRejectsTampering(t *testing.T) {
	a := testAuth(t, "passphrase-tamper")
	now := time.Unix(1_800_000_000, 0)
	token, _, err := a.IssueToken("uid-1", "x@y.z", "platform_admin", now)
	if err != nil {
		t.Fatalf(issueTokenErrFormat, err)
	}
	parts := strings.Split(token, ".")

	t.Run("payload swapped for an admin-escalated copy", func(t *testing.T) {
		assertRejected(t, a, tamperPayload(t, parts, "root"), now, "tampered payload accepted")
	})

	t.Run("signature garbage", func(t *testing.T) {
		tampered := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte("not-a-mac"))
		assertRejected(t, a, tampered, now, "bad signature accepted")
	})

	t.Run("alg none", func(t *testing.T) {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		assertRejected(t, a, header+"."+parts[1]+".", now, "alg=none accepted")
	})

	t.Run("wrong key", func(t *testing.T) {
		other := testAuth(t, "passphrase-tamper")
		other.cfg.JWTKey = []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		assertRejected(t, other, token, now, "token signed with a different key accepted")
	})

	t.Run("expired", func(t *testing.T) {
		assertRejected(t, a, token, now.Add(2*time.Hour), "expired token accepted")
	})

	t.Run("foreign issuer", func(t *testing.T) {
		claims := map[string]any{
			"iss": "https://evil.example.com", "sub": "u", "amr": "breakglass",
			"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "jti": "j",
		}
		// Same key, wrong issuer: must still be rejected.
		assertRejected(t, a, forgeSignedToken(t, claims), now, "foreign-issuer token accepted")
	})

	t.Run("missing amr", func(t *testing.T) {
		claims := map[string]any{
			"iss": Issuer, "sub": "u", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "jti": "j",
		}
		assertRejected(t, a, forgeSignedToken(t, claims), now, "token without amr=breakglass accepted")
	})
}

// assertRejected verifies that the given token is rejected at the given time.
func assertRejected(t *testing.T, a *Auth, token string, now time.Time, failMsg string) {
	t.Helper()
	if _, err := a.VerifyToken(token, now); err == nil {
		t.Fatal(failMsg)
	}
}

// tamperPayload re-signs the token parts with the payload's role replaced.
func tamperPayload(t *testing.T, parts []string, role string) string {
	t.Helper()
	var claims map[string]any
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	claims["role"] = role
	forged, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(forged) + "." + parts[2]
}

// forgeSignedToken builds an unsigned-payload JWT signed with the test key.
func forgeSignedToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	hb, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	input := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	mac := hmac.New(sha256.New, []byte(testKey))
	mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestIsBreakGlassToken(t *testing.T) {
	a := testAuth(t, "passphrase-detect")
	token, _, err := a.IssueToken("uid", "x@y.z", "platform_admin", time.Now())
	if err != nil {
		t.Fatalf(issueTokenErrFormat, err)
	}
	if !IsBreakGlassToken(token) {
		t.Fatal("break-glass token not detected")
	}
	for _, notOurs := range []string{"", "opaque-token", "a.b", "a.b.c.d", base64.RawURLEncoding.EncodeToString([]byte("junk"))} {
		if IsBreakGlassToken(notOurs) {
			t.Fatalf("non-token %q detected as break-glass", notOurs)
		}
	}
	// A Dex-style JWT with a different issuer must not be routed to us.
	otherIss := map[string]any{"iss": "https://cloud.rossbrigoli.com/auth", "sub": "x"}
	cb, _ := json.Marshal(otherIss)
	hb, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	other := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb) + ".sig"
	if IsBreakGlassToken(other) {
		t.Fatal("Dex JWT misrouted to break-glass detector")
	}
}

func TestClientAllowed(t *testing.T) {
	a := testAuth(t, "passphrase-cidr")
	cases := map[string]bool{
		"192.168.68.55": true,
		"100.64.9.9":    true,
		"100.100.5.5":   true,  // still inside 100.64.0.0/10 (64-127) - Tailscale
		"100.128.5.5":   false, // just past the /10
		"192.168.69.1":  false,
		"8.8.8.8":       false,
		"":              false,
		"not-an-ip":     false,
		"::1":           false,
		"fe80::1%eth0":  false,
	}
	for ip, want := range cases {
		if got := a.ClientAllowed(ip); got != want {
			t.Errorf("ClientAllowed(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestCloudflareSignals(t *testing.T) {
	// Cloudflare stamps these on every proxied request; presence means internet.
	for _, h := range []*http.Header{
		headerWith("Cf-Ray", "8f2c..."),
		headerWith("Cf-Connecting-Ip", "203.0.113.9"),
		headerWith("Cf-Visitor", `{"scheme":"https"}`),
	} {
		if !CloudflareSignals(*h) {
			t.Error("Cloudflare-fronted request not detected")
		}
	}
	plain := http.Header{}
	plain.Set("X-Forwarded-For", testClientIP)
	if CloudflareSignals(plain) {
		t.Error("plain LAN request flagged as Cloudflare")
	}
	empty := http.Header{}
	empty.Set("Cf-Ray", "   ")
	if CloudflareSignals(empty) {
		t.Error("whitespace-only Cf-Ray flagged as Cloudflare")
	}
}

func headerWith(k, v string) *http.Header {
	h := http.Header{}
	h.Set(k, v)
	return &h
}

func TestRateLimit(t *testing.T) {
	a := testAuth(t, "passphrase-rate")
	now := time.Unix(1_800_000_000, 0)
	for i := 1; i <= 3; i++ {
		if ok, _ := a.RateLimit(testClientIP, now); !ok {
			t.Fatalf("attempt %d blocked before the budget was exhausted", i)
		}
	}
	ok, retry := a.RateLimit(testClientIP, now)
	if ok {
		t.Fatal("4th attempt allowed with MaxAttempts=3")
	}
	if retry <= 0 {
		t.Fatalf("retry_after = %v, want positive", retry)
	}
	// A different IP has its own budget.
	if ok, _ := a.RateLimit("192.168.68.6", now); !ok {
		t.Fatal("unrelated IP blocked by another IP's budget")
	}
	// Window expiry resets the budget.
	if ok, _ := a.RateLimit(testClientIP, now.Add(16*time.Minute)); !ok {
		t.Fatal("budget did not reset after the window")
	}
}

func TestNewValidatesWhenEnabled(t *testing.T) {
	good := Config{
		Enabled: true, Username: "bg", PasswordHash: makePHC(t, "a-long-enough-passphrase-here"),
		JWTKey: []byte(testKey), AllowedCIDRs: []*net.IPNet{parseCIDR(t, "192.168.68.0/24")},
	}
	if _, err := New(good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := map[string]func(*Config){
		"no username":     func(c *Config) { c.Username = "" },
		"no hash":         func(c *Config) { c.PasswordHash = "" },
		"short jwt key":   func(c *Config) { c.JWTKey = []byte("tooshort") },
		"no allowed cidr": func(c *Config) { c.AllowedCIDRs = nil },
	}
	for name, mutate := range cases {
		bad := good
		mutate(&bad)
		if _, err := New(bad); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestDisabledFailsClosed(t *testing.T) {
	a, err := New(Config{Enabled: false})
	if err != nil {
		t.Fatalf("disabled New should not error: %v", err)
	}
	if a.Enabled() {
		t.Fatal("disabled auth reports enabled")
	}
	if a.VerifyCredentials("breakglass", "anything") {
		t.Fatal("disabled auth verified credentials")
	}
	if _, _, err := a.IssueToken("id", "e", "r", time.Now()); err == nil {
		t.Fatal("disabled auth issued a token")
	}
	if _, err := a.VerifyToken("x.y.z", time.Now()); err == nil {
		t.Fatal("disabled auth verified a token")
	}
	if a.ClientAllowed(testClientIP) {
		t.Fatal("disabled auth allowed a client")
	}
}
