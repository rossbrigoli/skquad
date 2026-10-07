package recorder

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []Frame {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []Frame
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var f Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("bad frame %q: %v", line, err)
		}
		out = append(out, f)
	}
	return out
}

func TestLocalDirFraming(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLocalDirSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	meta := Meta{ResourceID: "r1", AgentID: "a1", TaskID: "t1", Host: "h1", User: "u1", Command: "ls -la"}
	r, err := New(sink, "recA", meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AppendOut([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if err := r.AppendIn([]byte("pwd\n")); err != nil {
		t.Fatal(err)
	}
	if err := r.AppendOut([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	frames := readLines(t, filepath.Join(dir, "recA.part1.jsonl"))
	if len(frames) != 4 {
		t.Fatalf("want 4 frames, got %d", len(frames))
	}
	if frames[0].Stream != "meta" {
		t.Errorf("first frame must be meta, got %s", frames[0].Stream)
	}
	var gotMeta Meta
	if err := json.Unmarshal([]byte(frames[0].DataB64ToBytes(t)), &gotMeta); err != nil {
		t.Fatal(err)
	}
	if gotMeta.Host != "h1" || gotMeta.Command != "ls -la" {
		t.Errorf("meta wrong: %+v", gotMeta)
	}
	if frames[1].Stream != "out" || frames[1].DataB64ToBytes(t) != "hello " {
		t.Errorf("frame1 wrong: %+v", frames[1])
	}
	if frames[2].Stream != "in" || frames[2].DataB64ToBytes(t) != "pwd\n" {
		t.Errorf("frame2 wrong: %+v", frames[2])
	}
	if frames[3].Stream != "out" || frames[3].DataB64ToBytes(t) != "world" {
		t.Errorf("frame3 wrong: %+v", frames[3])
	}
	for _, f := range frames {
		if f.V != 1 || f.RecordingID != "recA" || f.TMS <= 0 {
			t.Errorf("frame envelope wrong: %+v", f)
		}
	}

	idxBytes, err := os.ReadFile(filepath.Join(dir, "recA.index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Parts []string `json:"parts"`
	}
	if err := json.Unmarshal(idxBytes, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Parts) != 1 || idx.Parts[0] != "recA.part1.jsonl" {
		t.Errorf("index wrong: %v", idx.Parts)
	}
}

func TestFlushParts(t *testing.T) {
	dir := t.TempDir()
	sink, _ := NewLocalDirSink(dir)
	r, _ := New(sink, "recB", Meta{Host: "h", User: "u"})
	big := strings.Repeat("x", 40*1024)
	for i := 0; i < 4; i++ {
		if err := r.AppendOut([]byte(big)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 4×40KiB with 64KiB threshold → at least 2 parts + final flush.
	entries, _ := os.ReadDir(dir)
	parts := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "recB.part") {
			parts++
		}
	}
	if parts < 2 {
		t.Errorf("expected >=2 parts, got %d", parts)
	}
}

func TestRecordingErrorNotFatal(t *testing.T) {
	sink := &failSink{}
	r, err := New(sink, "recC", Meta{Host: "h", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	// First append stays under the flush threshold → no error yet.
	if err := r.AppendOut([]byte("small")); err != nil {
		t.Fatalf("small append should not error: %v", err)
	}
	// Force a flush that fails.
	err = r.Flush(context.Background())
	if err == nil {
		t.Fatal("expected flush error")
	}
	if r.Err() == nil {
		t.Error("recorder should remember the error")
	}
	// Session continues: further appends still work (buffered).
	if err := r.AppendOut([]byte("more")); err != nil {
		t.Fatalf("append after error should still buffer: %v", err)
	}
}

type failSink struct{}

func (f *failSink) WritePart(ctx context.Context, id string, n int, data []byte) (string, error) {
	return "", errors.New("sink boom")
}
func (f *failSink) WriteIndex(ctx context.Context, id string, parts []string) error {
	return errors.New("index boom")
}

func TestS3SinkKeys(t *testing.T) {
	var keys []string
	sink := &S3Sink{
		Bucket: "b",
		Date:   "2026-10-08",
		Put: func(ctx context.Context, bucket, key string, data []byte) error {
			if bucket != "b" {
				t.Errorf("bucket %q", bucket)
			}
			keys = append(keys, key)
			return nil
		},
	}
	r, _ := New(sink, "recD", Meta{Host: "h", User: "u"})
	_ = r.AppendOut([]byte("data"))
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"recordings/2026-10-08/recD.part1.jsonl",
		"recordings/2026-10-08/recD.index.json",
	}
	if len(keys) != 2 || keys[0] != want[0] || keys[1] != want[1] {
		t.Errorf("keys wrong: %v", keys)
	}
}

func TestDatePrefix(t *testing.T) {
	ts := time.Date(2026, 10, 8, 23, 30, 0, 0, time.UTC)
	if got := DatePrefix(ts); got != "2026-10-08" {
		t.Errorf("got %s", got)
	}
}

// helper method on Frame via embedded testing helper (defined as a
// function to keep the struct clean).
func (f Frame) DataB64ToBytes(t *testing.T) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(f.DataB64)
	if err != nil {
		t.Fatalf("decode %q: %v", f.DataB64, err)
	}
	return string(b)
}

var _ = hex.EncodeToString
