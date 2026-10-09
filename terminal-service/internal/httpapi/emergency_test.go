package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/terminal-service/internal/recorder"
	"github.com/rossbrigoli/skquad/terminal-service/internal/sshexec"
)

// captureStarter records the last open request so cert-TTL clamping is
// observable, and returns a live fake session.
type captureStarter struct {
	lastReq sshexec.Request
}

func (c *captureStarter) Open(ctx context.Context, req sshexec.Request, onOut func([]byte)) (LiveSession, error) {
	c.lastReq = req
	return &fakeLive{}, nil
}

func emergencyServer(t *testing.T) (*httptest.Server, string, *captureStarter) {
	t.Helper()
	dir := t.TempDir()
	starter := &captureStarter{}
	srv := testServer(t, Config{
		CAMint:   &okCAM{},
		Sessions: starter,
		RecorderSinkFactory: func(_ context.Context, _ string) (recorder.Sink, error) {
			return recorder.NewLocalDirSink(dir)
		},
	})
	return srv, dir, starter
}

func TestEmergencyPairValidation(t *testing.T) {
	srv, _, _ := emergencyServer(t)
	defer srv.Close()
	kh := validKH(t)
	cases := []struct {
		name string
		body string
		want string // "" = accepted
		code string
	}{
		{"emergency_without_incident_id",
			`{"host":"h","user":"ops","emergency":true,"auth":{"mode":"ca","known_hosts":"` + kh + `"}}`,
			"400", "emergency_without_incident_id"},
		{"incident_id_without_emergency",
			`{"host":"h","user":"ops","incident_id":"INC-1","auth":{"mode":"ca","known_hosts":"` + kh + `"}}`,
			"400", "incident_id_without_emergency"},
		{"invalid_control_char",
			"{\"host\":\"h\",\"user\":\"ops\",\"emergency\":true,\"incident_id\":\"INC-\\u0001x\",\"auth\":{\"mode\":\"ca\",\"known_hosts\":\"" + kh + "\"}}",
			"400", "invalid_incident_id"},
		{"invalid_too_long",
			`{"host":"h","user":"ops","emergency":true,"incident_id":"` + stringsRepeat("x", 121) + `","auth":{"mode":"ca","known_hosts":"` + kh + `"}}`,
			"400", "invalid_incident_id"},
		{"valid_pair",
			`{"host":"h","user":"ops","emergency":true,"incident_id":"INC-42","auth":{"mode":"ca","known_hosts":"` + kh + `"}}`,
			"201", ""},
	}
	for _, tc := range cases {
		resp := doReq(t, "POST", srv.URL+"/v1/sessions", "test-token", tc.body)
		wantStatus := 201
		if tc.want == "400" {
			wantStatus = 400
		}
		if resp.StatusCode != wantStatus {
			t.Errorf("%s: want %d, got %d", tc.name, wantStatus, resp.StatusCode)
		}
		if tc.code != "" {
			var out map[string]string
			json.NewDecoder(resp.Body).Decode(&out)
			if out["error"] != tc.code {
				t.Errorf("%s: want error %q, got %v", tc.name, tc.code, out)
			}
		}
		resp.Body.Close()
	}
}

func stringsRepeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestEmergencyOpenMetaTTLAndClamp(t *testing.T) {
	srv, dir, starter := emergencyServer(t)
	defer srv.Close()
	body := `{"resource_id":"r","agent_id":"a","task_id":"t","host":"h","user":"ops",` +
		`"emergency":true,"incident_id":"INC-99",` +
		`"auth":{"mode":"ca","cert_ttl_minutes":60,"known_hosts":"` + validKH(t) + `"}}`
	resp := doReq(t, "POST", srv.URL+"/v1/sessions", "test-token", body)
	if resp.StatusCode != http.StatusCreated {
		b := map[string]string{}
		json.NewDecoder(resp.Body).Decode(&b)
		t.Fatalf("want 201, got %d (%v)", resp.StatusCode, b)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["emergency"] != "true" || out["incident_id"] != "INC-99" {
		t.Errorf("response missing emergency/incident_id: %+v", out)
	}
	exp, err := time.Parse(time.RFC3339, out["expires_at"])
	if err != nil {
		t.Fatalf("expires_at unparsable: %v", err)
	}
	if d := time.Until(exp); d < 29*time.Minute || d > 31*time.Minute {
		t.Errorf("session TTL not ~30min: %v", d)
	}
	// Cert TTL clamped to ≤ 30 min despite the request asking for 60.
	if starter.lastReq.Auth.CertTTL > 30*time.Minute {
		t.Errorf("cert TTL not clamped: %v", starter.lastReq.Auth.CertTTL)
	}
	// Recording meta frame carries the emergency stamp. Kill the
	// session first so the recorder flushes the buffered meta into
	// part1 (the recorder only writes parts on flush/close).
	kill := doReq(t, "POST", srv.URL+"/v1/sessions/"+out["session_id"]+"/kill", "test-token", "")
	kill.Body.Close()
	parts, _ := filepath.Glob(filepath.Join(dir, "*.part1.jsonl"))
	if len(parts) != 1 {
		t.Fatalf("expected one part file, got %v", parts)
	}
	raw, err := os.ReadFile(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var frame recorder.Frame
	line := string(raw)
	for i, c := range line {
		if c == '\n' {
			line = line[:i]
			break
		}
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("frame: %v", err)
	}
	if frame.Stream != "meta" {
		t.Fatalf("first frame must be meta, got %q", frame.Stream)
	}
	metaBytes, _ := base64.StdEncoding.DecodeString(frame.DataB64)
	var meta recorder.Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatal(err)
	}
	if !meta.Emergency || meta.IncidentID != "INC-99" {
		t.Errorf("recording meta missing emergency/incident_id: %+v", meta)
	}
}

func TestEmergencyDeadlineExpiry(t *testing.T) {
	r := newRegistry(4, time.Hour)
	defer r.Stop()
	live := &session{id: "s1", lastActive: time.Now()}
	r.add(live)
	if r.expiredAt(live, time.Now()) {
		t.Error("non-emergency fresh session must not be expired")
	}
	em := &session{id: "s2", lastActive: time.Now(), emergency: true,
		deadline: time.Now().Add(-time.Second)}
	r.add(em)
	if !r.expiredAt(em, time.Now()) {
		t.Error("emergency session past hard deadline must be expired even when recently active")
	}
	em2 := &session{id: "s3", lastActive: time.Now(), emergency: true,
		deadline: time.Now().Add(10 * time.Minute)}
	r.add(em2)
	if r.expiredAt(em2, time.Now()) {
		t.Error("emergency session inside deadline must not be expired")
	}
}
