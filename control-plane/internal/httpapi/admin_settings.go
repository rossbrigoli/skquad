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
	"context"
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
	IdleScaleToZeroMinutes int                `json:"idle_scale_to_zero_minutes"`
	EmbedderRuntime        string             `json:"embedder_runtime"`
	AgentResources         agentResourcesView `json:"agent_resources"`
	UpdatedAt              string             `json:"updated_at,omitempty"`
	UpdatedBy              string             `json:"updated_by,omitempty"`
}

// agentResourcesView is the S-287 agent pod resources block: the four
// Kubernetes quantities applied to every agent pod. GET always returns
// the effective values (stored or built-in defaults).
type agentResourcesView struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

// agentResources reads the effective agent pod resources (platform
// settings with built-in defaults, S-287).
func (s *Server) agentResources(r *http.Request) agentResourcesView {
	res := domain.ResolveAgentResources(r.Context(), s.store.GetPlatformSetting)
	return agentResourcesView{
		CPURequest:    res.CPURequest,
		CPULimit:      res.CPULimit,
		MemoryRequest: res.MemoryRequest,
		MemoryLimit:   res.MemoryLimit,
	}
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
		AgentResources:         s.agentResources(r),
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
		IdleScaleToZeroMinutes *int                `json:"idle_scale_to_zero_minutes"`
		EmbedderRuntime        *string             `json:"embedder_runtime"`
		AgentResources         *agentResourcesView `json:"agent_resources"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IdleScaleToZeroMinutes == nil && req.EmbedderRuntime == nil && req.AgentResources == nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"at least one of idle_scale_to_zero_minutes / embedder_runtime / agent_resources is required")
		return
	}
	userID := currentUser(r.Context()).ID

	// Validate everything before touching any store.
	runtime, ok := s.validateEmbedderRuntime(w, req.EmbedderRuntime)
	if !ok {
		return
	}
	minutes, ok := validateIdleScaleMinutes(w, req.IdleScaleToZeroMinutes)
	if !ok {
		return
	}
	agentRes, ok := s.validateAgentResources(w, req.AgentResources)
	if !ok {
		return
	}

	ctx := s.pendingUserAuditCtx(r, "settings.platform.update", "platform_settings", "", "", nil)

	resp := map[string]any{}
	// Embedder runtime: ConfigMap first (operator intent), DB second.
	if runtime != "" {
		if !s.applyEmbedderRuntimeSetting(ctx, w, runtime, userID) {
			return
		}
		resp["embedder_runtime"] = runtime
	}
	if minutes != 0 {
		mirrored, ok := s.applyIdleScaleSetting(ctx, w, minutes, userID)
		if !ok {
			return
		}
		resp["idle_scale_to_zero_minutes"] = minutes
		resp["mirrored_agents"] = mirrored
	}
	if agentRes != nil {
		if !s.applyAgentResourcesSetting(ctx, w, *agentRes, userID) {
			return
		}
		resp["agent_resources"] = agentResourcesView{
			CPURequest:    agentRes.CPURequest,
			CPULimit:      agentRes.CPULimit,
			MemoryRequest: agentRes.MemoryRequest,
			MemoryLimit:   agentRes.MemoryLimit,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// validateAgentResources normalises and validates the requested agent
// pod resources (S-287), including the availability of the override
// writer. Returns nil when the block is absent.
func (s *Server) validateAgentResources(w http.ResponseWriter, raw *agentResourcesView) (*domain.AgentResourceSettings, bool) {
	if raw == nil {
		return nil, true
	}
	res, err := domain.ValidateAgentResources(
		strings.TrimSpace(raw.CPURequest),
		strings.TrimSpace(raw.CPULimit),
		strings.TrimSpace(raw.MemoryRequest),
		strings.TrimSpace(raw.MemoryLimit),
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return nil, false
	}
	if s.agentResourcesConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_resources_config_unavailable",
			"agent resources override needs control-plane Kubernetes API access")
		return nil, false
	}
	return &res, true
}

// applyAgentResourcesSetting writes the operator ConfigMap override
// first, then records the four platform settings (S-287, same
// ConfigMap-first posture as applyEmbedderRuntimeSetting).
func (s *Server) applyAgentResourcesSetting(ctx context.Context, w http.ResponseWriter, res domain.AgentResourceSettings, userID string) bool {
	if err := s.agentResourcesConfig.SetAgentResources(ctx, res); err != nil {
		writeError(w, http.StatusServiceUnavailable, "agent_resources_config_unavailable",
			"could not write agent resources override: "+err.Error())
		return false
	}
	keys := []struct {
		key   string
		value string
	}{
		{domain.PlatformSettingAgentCPURequest, res.CPURequest},
		{domain.PlatformSettingAgentCPULimit, res.CPULimit},
		{domain.PlatformSettingAgentMemoryRequest, res.MemoryRequest},
		{domain.PlatformSettingAgentMemoryLimit, res.MemoryLimit},
	}
	for _, kv := range keys {
		if err := s.store.SetPlatformSetting(ctx, kv.key, kv.value, userID); err != nil {
			writeStorageError(w, err)
			return false
		}
	}
	return true
}

// validateEmbedderRuntime normalises and validates the requested
// embedder runtime, including the availability of the override writer
// (S-189 split out of putAdminSettings).
func (s *Server) validateEmbedderRuntime(w http.ResponseWriter, raw *string) (string, bool) {
	if raw == nil {
		return "", true
	}
	runtime := strings.ToLower(strings.TrimSpace(*raw))
	switch runtime {
	case "auto", "cuda", "vulkan", "cpu":
	default:
		writeError(w, http.StatusBadRequest, "bad_request",
			"embedder_runtime must be one of: auto, cuda, vulkan, cpu")
		return "", false
	}
	if s.embedderConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "embedder_config_unavailable",
			"embedder runtime override needs control-plane Kubernetes API access")
		return "", false
	}
	return runtime, true
}

// validateIdleScaleMinutes range-checks the requested idle timeout
// (S-189 split). Returns 0 when the field is absent.
func validateIdleScaleMinutes(w http.ResponseWriter, raw *int) (int, bool) {
	if raw == nil {
		return 0, true
	}
	minutes := *raw
	if minutes < minIdleScaleToZeroMinutes || minutes > maxIdleScaleToZeroMinutes {
		writeError(w, http.StatusBadRequest, "bad_request",
			"idle_scale_to_zero_minutes must be between "+
				strconv.Itoa(minIdleScaleToZeroMinutes)+" and "+strconv.Itoa(maxIdleScaleToZeroMinutes))
		return 0, false
	}
	return minutes, true
}

// applyEmbedderRuntimeSetting writes the operator ConfigMap override
// first, then records the platform setting (S-189 split).
func (s *Server) applyEmbedderRuntimeSetting(ctx context.Context, w http.ResponseWriter, runtime, userID string) bool {
	if err := s.embedderConfig.SetEmbedderRuntime(ctx, runtime); err != nil {
		writeError(w, http.StatusServiceUnavailable, "embedder_config_unavailable",
			"could not write embedder runtime override: "+err.Error())
		return false
	}
	if err := s.store.SetPlatformSetting(ctx, domain.PlatformSettingEmbedderRuntime, runtime, userID); err != nil {
		writeStorageError(w, err)
		return false
	}
	return true
}

// applyIdleScaleSetting stores the idle timeout (seconds) and fans the
// change out through the Kubernetes agent-mirror outbox (S-189 split).
func (s *Server) applyIdleScaleSetting(ctx context.Context, w http.ResponseWriter, minutes int, userID string) (int, bool) {
	seconds := minutes * 60
	if err := s.store.SetPlatformSetting(ctx, domain.PlatformSettingIdleScaleToZeroSeconds, strconv.Itoa(seconds), userID); err != nil {
		writeStorageError(w, err)
		return 0, false
	}
	mirrored, err := s.store.EnqueueAllAgentUpserts(ctx)
	if err != nil {
		writeStorageError(w, err)
		return 0, false
	}
	return mirrored, true
}
