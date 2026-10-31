// Package audit emits structured JSON audit events for every build
// submission decision (allow AND deny) and for observed terminal build
// results. Events are written to stdout for cluster log collection.
//
// Privacy rule: audit events never contain bearer tokens, token hashes,
// or registry credentials — only principal ids, repo/revision/
// destination, decisions and digests.
package audit

import (
	"log/slog"
	"os"
	"time"
)

// Emitter emits audit events.
type Emitter interface {
	Emit(event string, fields map[string]any)
}

// Logger is a slog-backed JSON emitter.
type Logger struct {
	log *slog.Logger
}

// NewLogger builds the stdout JSON audit logger.
func NewLogger() *Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &Logger{log: slog.New(h)}
}

// Emit writes one audit event with a UTC timestamp.
func (l *Logger) Emit(event string, fields map[string]any) {
	args := make([]any, 0, 2*len(fields)+2)
	args = append(args, "ts", time.Now().UTC().Format(time.RFC3339), "event", event)
	for k, v := range fields {
		args = append(args, k, v)
	}
	l.log.Info("audit", args...)
}

// Nop discards events (tests).
type Nop struct{}

// Emit discards the event.
func (Nop) Emit(string, map[string]any) {}
