package storage

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rossbrigoli/skquad/control-plane/internal/domain"
)

// consultTimeoutReason (S-173) is the terminal_reason carried by the
// synthetic reply posted when a consult goes unanswered past its deadline.
// It rides on an otherwise normal reply so every consumer (inbox UI,
// runtime untrusted-content wrapper) can render it without special-casing
// a new message type, while the marker makes the synthetic origin loud.
const consultTimeoutReason = "consult_timeout"

// consultTimeoutPayload builds the synthetic reply body delivered to the
// asker. It is explicitly marked system-generated so the receiving LLM
// never mistakes it for the peer agent's actual answer.
func consultTimeoutPayload(consult *domain.Message) json.RawMessage {
	out, err := json.Marshal(consultTimeoutPayloadMap(consult.ID, consult.CreatedAt))
	if err != nil {
		return json.RawMessage(`{"message":"[system] consult timeout","consult_timeout":true,"system_generated":true}`)
	}
	return out
}

// consultTimeoutPayloadMap is the shared builder so the Postgres sweep
// (which holds plain strings/times, not a full domain.Message) and the
// memory store produce byte-identical notices.
func consultTimeoutPayloadMap(consultID string, createdAt time.Time) map[string]any {
	return map[string]any{
		"message": fmt.Sprintf(
			"[system] Consult %s (sent %s) got no reply before its deadline. The target agent did not answer in time. You may retry with a different peer, report the timeout to your user, or proceed without an answer.",
			consultID, createdAt.UTC().Format(time.RFC3339),
		),
		"consult_timeout":      true,
		"system_generated":     true,
		"timed_out_consult_id": consultID,
	}
}
