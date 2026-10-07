// Package recorder writes terminal-session recordings as framed JSONL.
//
// Format (one JSON object per line):
//
//	{"v":1,"recording_id":"...","t_ms":<unix ms>,"stream":"meta"|"out"|"in","data_b64":"..."}
//
// The first line is always the "meta" frame describing the session.
// Frames are buffered and flushed to a Sink every FlushBytes and at
// Close. Sinks: local directory (dev/tests) or S3-compatible object
// storage (MinIO). S3 uploads use incremental part files
// (<recording_id>.partN.jsonl) plus a final <recording_id>.index.json
// because object storage has no append semantics.
//
// Recording failures NEVER propagate as session failures: the recorder
// records the first error and surfaces it via Err() at finish time.
package recorder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// FlushBytes is the buffer threshold that triggers a part upload.
	FlushBytes = 64 * 1024
)

// Sink persists ordered part payloads for one recording.
type Sink interface {
	// WritePart writes part number n (1-based) containing concatenated
	// JSONL frames and returns its stored name/URI.
	WritePart(ctx context.Context, recordingID string, n int, data []byte) (string, error)
	// WriteIndex persists the final index listing stored parts.
	WriteIndex(ctx context.Context, recordingID string, parts []string) error
}

// Frame is one recording frame.
type Frame struct {
	V           int    `json:"v"`
	RecordingID string `json:"recording_id"`
	TMS         int64  `json:"t_ms"`
	Stream      string `json:"stream"`
	DataB64     string `json:"data_b64"`
}

// Meta is the first frame's payload (kept human-readable in the meta
// frame's data field as JSON).
type Meta struct {
	ResourceID string `json:"resource_id"`
	AgentID    string `json:"agent_id"`
	TaskID     string `json:"task_id"`
	Host       string `json:"host"`
	User       string `json:"user"`
	Command    string `json:"command,omitempty"`
}

// Recorder buffers frames for one recording and flushes to a Sink.
type Recorder struct {
	sink        Sink
	recordingID string

	mu      sync.Mutex
	buf     bytes.Buffer
	parts   []string
	err     error
	closed  bool
	started time.Time
}

// New starts a recording with the given meta. The meta frame is written
// immediately into the buffer.
func New(sink Sink, recordingID string, meta Meta) (*Recorder, error) {
	if recordingID == "" {
		return nil, fmt.Errorf("recorder: recording_id required")
	}
	r := &Recorder{sink: sink, recordingID: recordingID, started: time.Now()}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("recorder: meta: %w", err)
	}
	if err := r.append("meta", metaJSON); err != nil {
		return nil, err
	}
	return r, nil
}

// RecordingID returns the recording id.
func (r *Recorder) RecordingID() string { return r.recordingID }

// AppendOut records output bytes (stdout/pty stream).
func (r *Recorder) AppendOut(data []byte) error { return r.append("out", data) }

// AppendIn records input bytes (stdin).
func (r *Recorder) AppendIn(data []byte) error { return r.append("in", data) }

func (r *Recorder) append(stream string, data []byte) error {
	f := Frame{
		V:           1,
		RecordingID: r.recordingID,
		TMS:         time.Now().UnixMilli(),
		Stream:      stream,
		DataB64:     base64.StdEncoding.EncodeToString(data),
	}
	line, err := json.Marshal(f)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("recorder: closed")
	}
	r.buf.Write(line)
	r.buf.WriteByte('\n')
	if r.buf.Len() >= FlushBytes {
		return r.flushLocked(context.Background())
	}
	return nil
}

// Flush forces buffered frames into a part.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.buf.Len() == 0 {
		return nil
	}
	return r.flushLocked(ctx)
}

// Close flushes remaining frames and writes the index. Idempotent.
func (r *Recorder) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	err := r.flushLocked(ctx)
	if err == nil {
		err = r.sink.WriteIndex(ctx, r.recordingID, r.parts)
		if err != nil {
			r.err = err
		}
	}
	r.closed = true
	return err
}

// Err returns the first recording error (never fatal to the session).
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// flushLocked uploads the current buffer as the next part. Caller holds mu.
func (r *Recorder) flushLocked(ctx context.Context) error {
	if r.buf.Len() == 0 {
		return nil
	}
	data := make([]byte, r.buf.Len())
	copy(data, r.buf.Bytes())
	r.buf.Reset()
	name, err := r.sink.WritePart(ctx, r.recordingID, len(r.parts)+1, data)
	if err != nil {
		r.err = err
		return err
	}
	r.parts = append(r.parts, name)
	return nil
}

// LocalDirSink writes parts + index under a directory (dev/tests).
type LocalDirSink struct {
	Dir string
}

// NewLocalDirSink creates (if needed) and returns a local sink.
func NewLocalDirSink(dir string) (*LocalDirSink, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &LocalDirSink{Dir: dir}, nil
}

// WritePart stores <recordingID>.partN.jsonl under the dir.
func (s *LocalDirSink) WritePart(_ context.Context, recordingID string, n int, data []byte) (string, error) {
	name := fmt.Sprintf("%s.part%d.jsonl", recordingID, n)
	if err := os.WriteFile(filepath.Join(s.Dir, name), data, 0o640); err != nil {
		return "", err
	}
	return name, nil
}

// WriteIndex stores <recordingID>.index.json.
func (s *LocalDirSink) WriteIndex(_ context.Context, recordingID string, parts []string) error {
	idx := map[string]any{"parts": parts}
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir, recordingID+".index.json"), b, 0o640)
}

// DatePrefix returns the YYYY-MM-DD prefix used for key layout.
func DatePrefix(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// S3Sink uploads parts to an S3-compatible bucket under
// recordings/<date>/<recordingID>.*. Implemented against a minimal
// PutObject interface so the concrete client (minio-go) is injected at
// main wiring and tests can fake it.
type S3Sink struct {
	Bucket string
	Date   string // YYYY-MM-DD; empty → today UTC
	Put    func(ctx context.Context, bucket, key string, data []byte) error
}

// WritePart uploads recordings/<date>/<recordingID>.partN.jsonl.
func (s *S3Sink) WritePart(ctx context.Context, recordingID string, n int, data []byte) (string, error) {
	date := s.Date
	if date == "" {
		date = DatePrefix(time.Now())
	}
	key := strings.Join([]string{"recordings", date, fmt.Sprintf("%s.part%d.jsonl", recordingID, n)}, "/")
	if err := s.Put(ctx, s.Bucket, key, data); err != nil {
		return "", err
	}
	return key, nil
}

// WriteIndex uploads recordings/<date>/<recordingID>.index.json.
func (s *S3Sink) WriteIndex(ctx context.Context, recordingID string, parts []string) error {
	date := s.Date
	if date == "" {
		date = DatePrefix(time.Now())
	}
	key := strings.Join([]string{"recordings", date, recordingID + ".index.json"}, "/")
	b, err := json.Marshal(map[string]any{"parts": parts})
	if err != nil {
		return err
	}
	return s.Put(ctx, s.Bucket, key, b)
}
