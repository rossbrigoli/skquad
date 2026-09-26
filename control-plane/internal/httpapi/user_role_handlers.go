// App-managed Layer-1 role administration.
//
// OIDC asserts identity only; the platform role lifecycle lives inside
// Skquad so promotions/demotions are explicit, guarded, and audited —
// instead of riding IdP group claims on every request. Group claims now
// only bootstrap a role on a user's FIRST login (INSERT); after that the
// row is owned by this handler.

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// setUserRole handles PATCH /api/v1/users/{userID}/role {"role": "..."}.
//
// Guards:
//   - caller must be platform_admin (requirePlatformAdmin)
//   - role must be a valid domain.Role ("platform_admin" | "user")
//   - the last platform_admin cannot be demoted (lockout prevention;
//     recovery without any admin is what the break-glass path is for)
//
// Every actual change is audited as "user.role_changed" with old/new role.
// If the audit write fails the role change is rolled back (fail closed):
// an unaudited authorization change is not acceptable.
func (s *Server) setUserRole(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	userID := chi.URLParam(r, "userID")

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	role := domain.Role(strings.TrimSpace(req.Role))
	if !role.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", "role must be 'platform_admin' or 'user'")
		return
	}

	target, err := s.store.GetUser(r.Context(), userID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if target.Role == role {
		// Idempotent no-op: still 200 with the current state.
		writeJSON(w, http.StatusOK, userRoleView(target))
		return
	}
	if role == domain.RoleUser && s.countPlatformAdmins(r) <= 1 {
		writeError(w, http.StatusConflict, "last_admin", "cannot demote the last platform admin")
		return
	}

	if err := s.store.SetUserRole(r.Context(), userID, role); err != nil {
		writeStorageError(w, err)
		return
	}

	metadata, _ := json.Marshal(map[string]string{
		"old_role": string(target.Role),
		"new_role": string(role),
	})
	if err := s.recordUserAuditRequired(r, "user.role_changed", "user", userID, "", metadata); err != nil {
		// Fail closed: compensate by restoring the previous role.
		_ = s.store.SetUserRole(r.Context(), userID, target.Role)
		writeError(w, http.StatusInternalServerError, "internal", "audit write failed; role change rolled back")
		return
	}

	target.Role = role
	writeJSON(w, http.StatusOK, userRoleView(target))
}

// countPlatformAdmins counts current platform_admin rows. On a listing
// error it returns 1 (conservative: behave as if the caller were the
// last admin rather than risking a lockout-inducing demotion spree).
func (s *Server) countPlatformAdmins(r *http.Request) int {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		return 1
	}
	n := 0
	for _, u := range users {
		if u.Role == domain.RolePlatformAdmin {
			n++
		}
	}
	return n
}

func userRoleView(u *domain.User) map[string]any {
	return map[string]any{
		"id":         u.ID,
		"email":      u.Email,
		"name":       u.Name,
		"role":       u.Role,
		"created_at": u.CreatedAt,
	}
}
