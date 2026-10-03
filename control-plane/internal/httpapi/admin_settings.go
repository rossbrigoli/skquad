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
	"strings"
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
	EmbedderRuntime        string `json:"embedder_runtime"`
	UpdatedAt            string `json:"updated_at,omitempty"`
	UpdatedBy            string `json:"updated_by,omitempty"`
}

// embedderRuntime reads the admin's embedder runtime choice
// (S-212, ADR-0013 §4). Unset means "auto": the operator's
// node-based detection decides.
func (s *Server) embedderRuntime(r *http.Request) string {
	raw, found, err := s.store.GetPlatformSetting(r.Context(), domain.PlatformSettingEmbedderRuntime)
	if err == nil && found {
		if v := strings.TrimSpace(raw); v != "" {
			return v
		}
	}
	return "auto"
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
		EmbedderRuntime:        s.embedderRuntime(r),
	})
}

// putAdminSettings updates platform settings (admin only). At least one
// field must be present. idle_scale_to_zero_minutes fans an agent mirror
// out through the Kubernetes outbox so every Agent CR adopts the new
// timeout without a redeploy. embedder_runtime (S-212, ADR-0013 §5) is
// written to the operator's override ConfigMap FIRST — the operator
// must be able to act on the intent before we record it; a ConfigMap
// write failure returns 503 and stores nothing.
func (s *Server) putAdminSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var req struct {
		IdleScaleToZeroMinutes *int    `json:"idle_scale_to_zero_minutes"`
		EmbedderRuntime        *string `json:"embedder_runtime"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IdleScaleToZeroMinutes == nil && req.EmbedderRuntime == nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"at least one of idle_scale_to_zero_minutes / embedder_runtime is required")
		return
	}
	userID := currentUser(r.Context()).ID

	// Validate everything before touching any store.
	runtime := ""
	if req.EmbedderRuntime != nil {
		runtime = strings.ToLower(strings.TrimSpace(*req.EmbedderRuntime))
		switch runtime {
		case "auto", "cuda", "vulkan", "cpu":
		default:
			writeError(w, http.StatusBadRequest, "bad_request",
				"embedder_runtime must be one of: auto, cuda, vulkan, cpu")
			return
		}
		if s.embedderConfig == nil {
			writeError(w, http.StatusServiceUnavailable, "embedder_config_unavailable",
				"embedder runtime override needs control-plane Kubernetes API access")
			return
		}
	}
	minutes := 0
	if req.IdleScaleToZeroMinutes != nil {
		minutes = *req.IdleScaleToZeroMinutes
		if minutes < minIdleScaleToZeroMinutes || minutes > maxIdleScaleToZeroMinutes {
			writeError(w, http.StatusBadRequest, "bad_request",
				"idle_scale_to_zero_minutes must be between "+
					strconv.Itoa(minIdleScaleToZeroMinutes)+" and "+strconv.Itoa(maxIdleScaleToZeroMinutes))
			return
		}
	}

	ctx := s.pendingUserAuditCtx(r, "settings.platform.update", "platform_settings", "", "", nil)

	// Embedder runtime: ConfigMap first (operator intent), DB second.
	if runtime != "" {
		if err := s.embedderConfig.SetEmbedderRuntime(ctx, runtime); err != nil {
			writeError(w, http.StatusServiceUnavailable, "embedder_config_unavailable",
				"could not write embedder runtime override: "+err.Error())
			return
		}
		if err := s.store.SetPlatformSetting(ctx, domain.PlatformSettingEmbedderRuntime, runtime, userID); err != nil {
			writeStorageError(w, err)
			return
		}
	}
	resp := map[string]any{}
	if runtime != "" {
		resp["embedder_runtime"] = runtime
	}
	if minutes != 0 {
		seconds := minutes * 60
		if err := s.store.SetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds, strconv.Itoa(seconds), userID); err != nil {
			writeStorageError(w, err)
			return
		}
		mirrored, err := s.store.EnqueueAllAgentUpserts(ctx)
		if err != nil {
			writeStorageError(w, err)
			return
		}
		resp["idle_scale_to_zero_minutes"] = minutes
		resp["mirrored_agents"] = mirrored
	}
	writeJSON(w, http.StatusOK, resp)
}
