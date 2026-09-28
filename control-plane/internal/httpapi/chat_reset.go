// S-162: chat reset + agent restart handlers.
//
// Reset thread: the user clears the chat; everything before the reset
// stops being included in the runtime's contextual prompt, and the
// transcript is archived into agent_memory (provenance 'chat_reset')
// so the agent can still recall it semantically.
//
// Restart agent: the user evicts the agent's pods; the owning
// Deployment recreates them. Used when the agent is stuck/crashed.

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/config"
	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
	"github.com/rossbrigoli/skquad/control-plane/internal/kube"
)

// PodRestarter deletes an agent's pods by label (restart semantics).
// Satisfied by *kube.PodRestarter; nil when K8s config is absent.
type PodRestarter interface {
	RestartAgentPods(ctx context.Context, agentID string) (int, error)
}

// resetAgentChat handles POST /agents/{agentID}/chat/reset.
// Archives the visible chat transcript into agent memory, then moves
// the reset boundary so subsequent history/context reads exclude it.
func (s *Server) resetAgentChat(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadAgentForAction(w, r, "talk")
	if !ok {
		return
	}
	messages, err := s.store.ListAgentMessageHistory(r.Context(), target.ID)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	transcript := renderChatTranscript(messages)
	metadata, _ := json.Marshal(map[string]any{
		"kind":         "chat_transcript",
		"message_count": len(messages),
		"reset_by":     currentUser(r.Context()).ID,
	})
	archived, resetAt, err := s.store.ResetAgentChat(r.Context(), target.ID, target.SquadID, transcript, metadata)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reset_at": resetAt,
		"archived": archived,
	})
}

// restartAgent handles POST /agents/{agentID}/restart.
// Deletes the agent's pods (label skquad.io/agent-id=<id>); the
// Deployment recreates them. 202 with the deleted-pod count.
func (s *Server) restartAgent(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadAgentForAction(w, r, "talk")
	if !ok {
		return
	}
	if s.podRestarter == nil {
		writeError(w, http.StatusServiceUnavailable, "restart_unavailable", "pod restart (Kubernetes) is not configured on this control-plane")
		return
	}
	deleted, err := s.podRestarter.RestartAgentPods(r.Context(), target.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "restart_failed", fmt.Sprintf("could not restart agent pods: %v", err))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"restarting": true,
		"pods":       deleted,
	})
}

// renderChatTranscript turns the visible chat messages into a compact
// text archive. Each line: [RFC3339] who: text. Non-string payloads
// fall back to compact JSON so nothing is silently dropped.
func renderChatTranscript(messages []*domain.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		who := "agent"
		if msg.FromType == "user" {
			who = "user"
		}
		text := extractChatText(msg)
		fmt.Fprintf(&b, "[%s] %s: %s\n", msg.CreatedAt.UTC().Format(time.RFC3339), who, text)
	}
	return b.String()
}

func extractChatText(msg *domain.Message) string {
	var fields map[string]any
	if err := json.Unmarshal(msg.Payload, &fields); err == nil {
		if text, ok := fields["message"].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
		if text, ok := fields["content"].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return strings.TrimSpace(string(msg.Payload))
}

// newPodRestarter builds the S-162 restarter from in-cluster config.
// Returns nil (not an error) when K8s config is absent so dev mode
// keeps working; the handler surfaces 503 on use.
func newPodRestarter(cfg *config.Config) (*kube.PodRestarter, error) {
	if cfg == nil || !cfg.K8sEnabled || cfg.K8sAPIBase == "" || cfg.K8sTokenFile == "" {
		return nil, nil
	}
	return kube.NewPodRestarter(cfg)
}
