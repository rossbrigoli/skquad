// S-200: the agent's bound-model capability read.
//
// The runtime needs to know whether its bound model can accept image
// input BEFORE it fetches upload bytes and base64-inflates them into the
// request. This endpoint serves the bound primary model's capability
// flags at wake time (fetch-at-wake, mirroring the composed-prompt
// pattern). It is deliberately best-effort on the runtime side: any
// failure is treated as "not vision-capable" so the turn degrades to the
// text reference rather than erroring.
//
// Gating is on the REQUESTED (bound primary) model. If a non-vision
// fallback serves an image turn the upstream may reject it; per-served-
// model gateway gating is the future fix and the model_info flag is
// already provisioned for it (see gateway_models.go / vision_gate).

package httpapi

import (
	"net/http"
)

// agentModelResponse is the lean capability projection the runtime reads.
type agentModelResponse struct {
	ModelName      string `json:"model_name"`
	SupportsVision bool   `json:"supports_vision"`
	SupportsTools  bool   `json:"supports_tools"`
}

// getMyModel serves GET /api/v1/agents/me/model. An agent with no bound
// primary model (or an unresolvable binding) gets an all-false response
// rather than an error — the runtime's degradation contract.
func (s *Server) getMyModel(w http.ResponseWriter, r *http.Request) {
	principal := currentAgent(r.Context())
	resp := agentModelResponse{}
	if principal == nil || principal.Agent == nil || principal.Agent.AIModelID == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	model, err := s.store.GetAIModel(r.Context(), principal.Agent.AIModelID)
	if err != nil || model == nil {
		// Unresolvable binding: report "no capabilities" instead of
		// failing the wake. The runtime falls back to text-only.
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.ModelName = model.ModelName
	resp.SupportsVision = model.SupportsVision
	resp.SupportsTools = model.SupportsTools
	writeJSON(w, http.StatusOK, resp)
}
