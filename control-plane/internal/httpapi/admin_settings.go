package httpapi

// S-183 — platform-admin idle scale-to-zero setting.
//
// "Only scale to zero when there have been no agent activities for the
// past 15 minutes." The value lives in the platform_settings store so a
// platform admin can change it from the Settings screen without
// redeploying Helm. The effective timeout reaches the scale decision via
// the Kubernetes outbox: every agent mirror resolves
// idleTimeout = per-agent override (>0) ?? platform setting ?? 15 min,
// and a PUT here re-enqueues all agent mirrors so running CRs converge
// immediately.
//
// The idle clock itself is the busy/idle status machinery: an agent is
// busy while a task runs, a chat turn is in progress, or inbox work is
// pending (heartbeats upgrade idle→busy on pending work), so the
// operator only starts counting idleSince after the last activity.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// idleScaleToZero bounds (minutes) accepted from the admin API.
const (
	minIdleScaleToZeroMinutes = 1
	maxIdleScaleToZeroMinutes = 240
)

type adminSettingsView struct {
	IdleScaleToZeroMinutes int    `json:"idle_scale_to_zero_minutes"`
	UpdatedAt            string `json:"updated_at,omitempty"`
	UpdatedBy            string `json:"updated_by,omitempty"`
}

// idleScaleToZeroMinutes reads the platform setting, falling back to the
// deploy-time default (SKQUAD_DEFAULT_IDLE_TIMEOUT) when unset.
func (s *Server) idleScaleToZeroMinutes(r *http.Request) int {
	raw, found, err := s.store.GetPlatformSetting(r.Context(), domain.PlatformSettingIdleScaleToZeroSeconds)
	if err == nil && found {
		if seconds, convErr := strconv.Atoi(raw); convErr == nil && seconds > 0 {
			minutes := seconds / 60
			if minutes < 1 {
				minutes = 1
			}
			return minutes
		}
	}
	return int(s.cfg.DefaultIdleTimeout / time.Minute)
}

// getAdminSettings returns the platform settings block (admin only).
func (s *Server) getAdminSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, adminSettingsView{
		IdleScaleToZeroMinutes: s.idleScaleToZeroMinutes(r),
	})
}

// putAdminSettings updates the idle scale-to-zero minutes (admin only),
// then fans an agent mirror out through the Kubernetes outbox so every
// Agent CR adopts the new effective timeout without a redeploy.
func (s *Server) putAdminSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req struct {
		IdleScaleToZeroMinutes *int `json:"idle_scale_to_zero_minutes"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IdleScaleToZeroMinutes == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "idle_scale_to_zero_minutes is required")
		return
	}
	minutes := *req.IdleScaleToZeroMinutes
	if minutes < minIdleScaleToZeroMinutes || minutes > maxIdleScaleToZeroMinutes {
		writeError(w, http.StatusBadRequest, "bad_request",
			"idle_scale_to_zero_minutes must be between "+
				strconv.Itoa(minIdleScaleToZeroMinutes)+" and "+strconv.Itoa(maxIdleScaleToZeroMinutes))
		return
	}
	userID := currentUser(r.Context()).ID
	seconds := minutes * 60
	ctx := s.pendingUserAuditCtx(r, "settings.idle_scale_to_zero.update", "platform_settings", "", "", nil)
	if err := s.store.SetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds, strconv.Itoa(seconds), userID); err != nil {
		writeStorageError(w, err)
		return
	}
	mirrored, err := s.store.EnqueueAllAgentUpserts(ctx)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"idle_scale_to_zero_minutes": minutes,
		"mirrored_agents":            mirrored,
	})
}
