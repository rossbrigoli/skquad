package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/breakglass"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// breakGlassLocalIssuer and subject identify the break-glass principal in the
// users table. Using the existing (oidc_issuer, oidc_subject) unique index means
// no schema change is required, and the sentinel "local" issuer makes it obvious
// in the DB that this row did not come from Dex.
const (
	breakGlassLocalIssuer  = "local"
	breakGlassLocalSubject = "breakglass"
)

// breakGlassStatus reports whether the break-glass path is live. It deliberately
// discloses nothing beyond a boolean so the login screen can decide whether to
// render the form without leaking any configuration detail.
func (s *Server) breakGlassStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.breakGlass != nil && s.breakGlass.Enabled(),
	})
}

// breakGlassLogin authenticates the break-glass operator with a username and
// password and returns a short-lived locally-signed bearer token.
//
// Reachability controls, evaluated in order and all fail-closed:
//  1. disabled            -> 404 (endpoint is not advertised at all)
//  2. Cloudflare-fronted  -> 403 (request came from the public internet)
//  3. IP not allowlisted  -> 403 (not LAN / Tailscale)
//  4. rate limited        -> 429 + Retry-After
//  5. bad credentials     -> 401
//
// Rule 2 is the load-bearing one. Cloudflare always stamps CF-Ray /
// CF-Connecting-IP on requests it proxies and a client cannot strip or forge
// them, so any request carrying those headers arrived from the internet — even
// when it was initiated from inside the house using the public hostname.
func (s *Server) breakGlassLogin(w http.ResponseWriter, r *http.Request) {
	if s.breakGlass == nil || !s.breakGlass.Enabled() {
		// 404, not 403/401: do not confirm the endpoint exists while disabled.
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}

	clientIP := breakglass.ResolveClientIP(r)

	if breakglass.CloudflareSignals(r.Header) {
		log.Printf("BREAKGLASS rejected: request arrived via Cloudflare (client=%s) - break-glass is LAN/Tailscale only", clientIP)
		writeError(w, http.StatusForbidden, "forbidden", "break-glass is not reachable over the public endpoint; use the internal LAN/Tailscale address")
		return
	}

	if !s.breakGlass.ClientAllowed(clientIP) {
		log.Printf("BREAKGLASS rejected: client %s is outside the allowed CIDRs", clientIP)
		writeError(w, http.StatusForbidden, "forbidden", "source address is not permitted for break-glass login")
		return
	}

	if allowed, retryAfter := s.breakGlass.RateLimit(clientIP, time.Now()); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		log.Printf("BREAKGLASS rate-limited: client %s retry_after=%s", clientIP, retryAfter)
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many break-glass login attempts")
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "expected a JSON body with username and password")
		return
	}

	if !s.breakGlass.VerifyCredentials(body.Username, body.Password) {
		// Never log the password, not even masked.
		log.Printf("BREAKGLASS failed login: username=%q client=%s", body.Username, clientIP)
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		return
	}

	user, err := s.store.UpsertUser(r.Context(), &domain.User{
		OIDCIssuer:    breakGlassLocalIssuer,
		OIDCSubject:   breakGlassLocalSubject,
		Email:         breakGlassEmail(s.cfg.BreakGlassUsername),
		EmailVerified: true,
		Name:          "break-glass",
		Role:          domain.RolePlatformAdmin,
	})
	if err != nil {
		log.Printf("BREAKGLASS error: could not persist principal: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "failed to persist break-glass principal")
		return
	}
	// UpsertUser only assigns role on INSERT; guarantee the emergency account is
	// actually admin even if the row predates this feature.
	if user.Role != domain.RolePlatformAdmin {
		if err := s.store.SetUserRole(r.Context(), user.ID, domain.RolePlatformAdmin); err != nil {
			log.Printf("BREAKGLASS error: could not promote principal: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "failed to promote break-glass principal")
			return
		}
		user.Role = domain.RolePlatformAdmin
	}

	token, expiresAt, err := s.breakGlass.IssueToken(user.ID, user.Email, string(user.Role), time.Now())
	if err != nil {
		log.Printf("BREAKGLASS error: could not issue token: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "failed to issue break-glass token")
		return
	}

	// Loud on purpose: a break-glass login is an exceptional event and must be
	// visible in the logs with no secrets attached.
	log.Printf("BREAKGLASS LOGIN GRANTED: user_id=%s client=%s expires=%s jti-issued=yes", user.ID, clientIP, expiresAt.Format(time.RFC3339))

	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expiresAt.Format(time.RFC3339),
		"user":       user,
	})
}

// breakGlassEmail derives a stable email for the break-glass row so it never
// collides with a real OIDC identity.
func breakGlassEmail(username string) string {
	if username == "" {
		username = "breakglass"
	}
	return username + "@breakglass.skquad.local"
}
