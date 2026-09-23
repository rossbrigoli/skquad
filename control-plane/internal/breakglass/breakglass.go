// Package breakglass implements an authentication path that is fully independent
// of the OIDC provider (Dex) and of GitHub.
//
// Rationale: the 2026-09-23 incident that locked Ross out of skquad was an
// AUTHORIZATION failure (missing group->role mapping), not an authentication
// failure. Dex and GitHub were both healthy. A break-glass routed through the
// IdP would therefore not have recovered us, because it still traverses the same
// app-side authz logic. This path bypasses OIDC entirely and lands directly as
// platform_admin.
//
// Reachability is deliberately constrained: this endpoint must never be reachable
// from the public internet. See ClientIP for how Cloudflare-fronted requests are
// detected and rejected.
package breakglass

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Issuer is the fixed token issuer for break-glass sessions. The auth middleware
// uses it to route a bearer token to the local verifier instead of Dex.
const Issuer = "skquad-breakglass"

// AuthN errors. Callers must not leak which of these fired to the client.
var (
	ErrBadCredentials = errors.New("breakglass: invalid username or password")
	ErrBadToken       = errors.New("breakglass: invalid token")
	ErrNotEnabled     = errors.New("breakglass: not enabled")
)

// Config is the resolved break-glass configuration.
type Config struct {
	Enabled      bool
	Username     string
	PasswordHash string // argon2id PHC string, e.g. $argon2id$v=19$m=65536,t=3,p=2$...$...
	JWTKey       []byte
	TokenTTL     time.Duration
	AllowedCIDRs []*net.IPNet
	MaxAttempts  int
	Window       time.Duration
}

// Claims is the payload of a break-glass token.
type Claims struct {
	ID    string
	Email string
	Role  string
	JTI   string
	Iss   string
	Exp   int64
	Iat   int64
}

// Auth issues and verifies break-glass credentials and tokens.
type Auth struct {
	cfg Config

	mu       sync.Mutex
	attempts map[string]*attemptWindow
}

type attemptWindow struct {
	count   int
	expires time.Time
}

// New validates the config and returns an Auth. An disabled config is legal and
// cheap: every operation fails closed.
func New(cfg Config) (*Auth, error) {
	if !cfg.Enabled {
		return &Auth{cfg: cfg, attempts: map[string]*attemptWindow{}}, nil
	}
	if strings.TrimSpace(cfg.Username) == "" {
		return nil, errors.New("breakglass: username is required when enabled")
	}
	if strings.TrimSpace(cfg.PasswordHash) == "" {
		return nil, errors.New("breakglass: password hash is required when enabled")
	}
	if len(cfg.JWTKey) < 32 {
		return nil, errors.New("breakglass: JWT key must be at least 32 bytes")
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 60 * time.Minute
	}
	if cfg.Window <= 0 {
		cfg.Window = 15 * time.Minute
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if len(cfg.AllowedCIDRs) == 0 {
		return nil, errors.New("breakglass: at least one allowed CIDR is required when enabled")
	}
	return &Auth{cfg: cfg, attempts: map[string]*attemptWindow{}}, nil
}

// Enabled reports whether the break-glass path is live.
func (a *Auth) Enabled() bool { return a != nil && a.cfg.Enabled }

// VerifyCredentials constant-time-checks the supplied username and password
// against the stored argon2id hash. Both the username and the password mismatch
// paths take a comparable amount of work so the response time does not reveal
// which field was wrong.
func (a *Auth) VerifyCredentials(username, password string) bool {
	if !a.Enabled() {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(username)), []byte(a.cfg.Username)) == 1
	hashOK, err := verifyArgon2id(a.cfg.PasswordHash, password)
	if !userOK {
		// Still perform a hash computation against a dummy password so a wrong
		// username costs the same as a wrong password.
		_, _ = verifyArgon2id(a.cfg.PasswordHash, "timing-equalizer-not-the-password")
		return false
	}
	return err == nil && hashOK
}

// IssueToken mints a short-lived HMAC-SHA256 signed token for the given principal.
func (a *Auth) IssueToken(id, email, role string, now time.Time) (string, time.Time, error) {
	if !a.Enabled() {
		return "", time.Time{}, ErrNotEnabled
	}
	exp := now.Add(a.cfg.TokenTTL)
	jti := fmt.Sprintf("bg-%d-%s", now.UnixNano(), randomHex(8))
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	claims := map[string]any{
		"iss":   Issuer,
		"sub":   id,
		"email": email,
		"role":  role,
		"amr":   "breakglass",
		"iat":   now.Unix(),
		"exp":   exp.Unix(),
		"jti":   jti,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", time.Time{}, err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	signingInput := b64(hb) + "." + b64(cb)
	return signingInput + "." + b64(a.sign(signingInput)), exp, nil
}

// VerifyToken validates signature, issuer and expiry.
func (a *Auth) VerifyToken(token string, now time.Time) (*Claims, error) {
	if !a.Enabled() {
		return nil, ErrNotEnabled
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, ErrBadToken
	}
	signingInput := parts[0] + "." + parts[1]
	mac := a.sign(signingInput)
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrBadToken
	}
	if subtle.ConstantTimeCompare(mac, provided) != 1 {
		return nil, ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrBadToken
	}
	var c struct {
		ISS   string `json:"iss"`
		SUB   string `json:"sub"`
		Email string `json:"email"`
		Role  string `json:"role"`
		AMR   string `json:"amr"`
		IAT   int64  `json:"iat"`
		EXP   int64  `json:"exp"`
		JTI   string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrBadToken
	}
	if c.ISS != Issuer {
		return nil, ErrBadToken
	}
	if c.AMR != "breakglass" {
		return nil, ErrBadToken
	}
	if c.EXP <= now.Unix() {
		return nil, ErrBadToken
	}
	if strings.TrimSpace(c.SUB) == "" {
		return nil, ErrBadToken
	}
	return &Claims{ID: c.SUB, Email: c.Email, Role: c.Role, JTI: c.JTI, Iss: c.ISS, Exp: c.EXP, Iat: c.IAT}, nil
}

// IsBreakGlassToken reports whether a bearer token should be handled by this
// verifier rather than by the OIDC verifier. It is a cheap, unauthenticated
// structural check on the issuer claim only.
func IsBreakGlassToken(token string) bool {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var c struct {
		ISS string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return false
	}
	return c.ISS == Issuer
}

func (a *Auth) sign(input string) []byte {
	mac := hmac.New(sha256.New, a.cfg.JWTKey)
	mac.Write([]byte(input))
	return mac.Sum(nil)
}

// RateLimit reports whether an attempt from ip is permitted. It returns a retry
// hint when the caller is over budget. The bucket is per client IP.
func (a *Auth) RateLimit(ip string, now time.Time) (allowed bool, retryAfter time.Duration) {
	if a == nil || a.cfg.MaxAttempts <= 0 {
		return true, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket, ok := a.attempts[ip]
	if !ok || now.After(bucket.expires) {
		a.attempts[ip] = &attemptWindow{count: 1, expires: now.Add(a.cfg.Window)}
		return true, 0
	}
	bucket.count++
	if bucket.count > a.cfg.MaxAttempts {
		return false, time.Until(bucket.expires)
	}
	return true, 0
}

// ClientAllowed reports whether ip falls inside the configured allowlist.
func (a *Auth) ClientAllowed(ip string) bool {
	if a == nil || len(a.cfg.AllowedCIDRs) == 0 {
		return false
	}
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, cidr := range a.cfg.AllowedCIDRs {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

// CloudflareSignals returns true when the request arrived through Cloudflare.
//
// This is the load-bearing control for "LAN/Tailscale only". Cloudflare overwrites
// CF-Ray and CF-Connecting-IP on every request it proxies, so a client cannot
// forge them to look like a non-Cloudflare request, and it cannot strip them
// either — the edge adds them. Any request carrying these headers came from the
// public internet and must never reach the break-glass endpoint.
func CloudflareSignals(h http.Header) bool {
	for _, k := range []string{"Cf-Ray", "Cf-Connecting-Ip", "Cf-Visitor", "Cf-Apo"} {
		if strings.TrimSpace(h.Get(k)) != "" {
			return true
		}
	}
	return false
}

// ResolveClientIP returns the best client IP for allowlisting. It prefers
// X-Real-IP, then the leftmost X-Forwarded-For entry, then the TCP peer address.
// Callers must reject Cloudflare-fronted requests BEFORE trusting this value.
func ResolveClientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return firstToken(v)
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		// leftmost = original client
		return firstToken(strings.Split(v, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

func firstToken(s string) string {
	return strings.TrimSpace(strings.Split(s, " ")[0])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buf)
}

// verifyArgon2id checks a password against an argon2id PHC string:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<b64 salt>$<b64 hash>
func verifyArgon2id(phc, password string) (bool, error) {
	parts := strings.Split(strings.TrimSpace(phc), "$")
	// leading "" from the leading $
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("breakglass: unsupported hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != 19 {
		return false, fmt.Errorf("breakglass: unsupported argon2 version")
	}
	var m, t, p uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, fmt.Errorf("breakglass: malformed argon2 parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		salt, err = base64.StdEncoding.DecodeString(parts[4])
		if err != nil {
			return false, fmt.Errorf("breakglass: bad salt encoding")
		}
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		want, err = base64.StdEncoding.DecodeString(parts[5])
		if err != nil {
			return false, fmt.Errorf("breakglass: bad hash encoding")
		}
	}
	got := argon2.IDKey([]byte(password), salt, t, m, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
