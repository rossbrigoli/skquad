package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rossbrigoli/skquad/terminal-service/internal/caclient"
	"github.com/rossbrigoli/skquad/terminal-service/internal/recorder"
	"github.com/rossbrigoli/skquad/terminal-service/internal/sshexec"
)

func testServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.InternalToken == "" {
		cfg.InternalToken = "test-token"
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 2
	}
	if cfg.SessionIdleTimeout == 0 {
		cfg.SessionIdleTimeout = time.Hour
	}
	if cfg.CAMint == nil {
		cfg.CAMint = &failCAM{}
	}
	if cfg.RecorderSinkFactory == nil {
		dir := t.TempDir()
		cfg.RecorderSinkFactory = func(_ context.Context, _ string) (recorder.Sink, error) {
			return recorder.NewLocalDirSink(dir)
		}
	}
	h, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(h)
}

type fakeStarter struct{}

type fakeLive struct{ closed bool }

func (f *fakeLive) Send(b []byte) error { return nil }
func (f *fakeLive) Close()              { f.closed = true }
func (f *fakeLive) Closed() bool        { return f.closed }

func (f *fakeStarter) Open(ctx context.Context, req sshexec.Request, onOut func([]byte)) (LiveSession, error) {
	return &fakeLive{}, nil
}

type failCAM struct{}

func (f *failCAM) Mint(ctx context.Context, user, host string, ttl time.Duration) (*caclient.Cert, error) {
	return nil, errors.New("ca_unavailable")
}

func doReq(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthRequired(t *testing.T) {
	srv := testServer(t, Config{})
	defer srv.Close()
	for _, path := range []string{"/v1/exec", "/v1/sessions"} {
		resp := doReq(t, "POST", srv.URL+path, "", `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
		resp = doReq(t, "POST", srv.URL+path, "wrong", `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s wrong token: want 401, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestHealthUnauthed(t *testing.T) {
	srv := testServer(t, Config{})
	defer srv.Close()
	for _, p := range []string{"/healthz", "/readyz"} {
		resp := doReq(t, "GET", srv.URL+p, "", "")
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestExecValidation(t *testing.T) {
	srv := testServer(t, Config{})
	defer srv.Close()
	// missing fields
	resp := doReq(t, "POST", srv.URL+"/v1/exec", "test-token", `{"host":"h"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// CA unavailable → 502 ca_unavailable (fail closed)
	body := `{"resource_id":"r","agent_id":"a","host":"h","user":"ops","command":"ls","auth":{"mode":"ca","known_hosts":"` + validKH(t) + `"}}`
	resp = doReq(t, "POST", srv.URL+"/v1/exec", "test-token", body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("want 502, got %d", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["error"] != "ca_unavailable" {
		t.Errorf("want ca_unavailable, got %v", out)
	}
}

func TestBodyCap(t *testing.T) {
	srv := testServer(t, Config{})
	defer srv.Close()
	big := `{"host":"` + strings.Repeat("x", 2<<20) + `"}`
	resp := doReq(t, "POST", srv.URL+"/v1/exec", "test-token", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("want 413, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSessionLimit(t *testing.T) {
	srv := testServer(t, Config{MaxSessions: 1})
	defer srv.Close()
	// First open fails at CA (502) but must not leak a registry slot:
	// we use a mint that succeeds to occupy the slot.
	okCAM := &okCAM{}
	srv.Close()
	srv2 := testServer(t, Config{MaxSessions: 1, CAMint: okCAM, Sessions: &fakeStarter{}})
	defer srv2.Close()
	body := `{"resource_id":"r","agent_id":"a","host":"h","user":"ops","auth":{"mode":"ca","known_hosts":"` + validKH(t) + `"}}`
	resp := doReq(t, "POST", srv2.URL+"/v1/sessions", "test-token", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	var first map[string]string
	json.NewDecoder(resp.Body).Decode(&first)
	resp.Body.Close()
	resp2 := doReq(t, "POST", srv2.URL+"/v1/sessions", "test-token", body)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want 429, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()
	// kill frees the slot
	kill := doReq(t, "POST", srv2.URL+"/v1/sessions/"+first["session_id"]+"/kill", "test-token", "")
	if kill.StatusCode != 200 {
		t.Errorf("kill: %d", kill.StatusCode)
	}
	kill.Body.Close()
	resp3 := doReq(t, "POST", srv2.URL+"/v1/sessions", "test-token", body)
	if resp3.StatusCode != http.StatusCreated {
		t.Errorf("after kill want 201, got %d", resp3.StatusCode)
	}
	resp3.Body.Close()
}

// okCAM mints a structurally valid, CA-signed user certificate so the
// session-open path proceeds past cert parsing (the SSH dial itself is
// exercised in the sshexec package against a live test server).
type okCAM struct{}

func (f *okCAM) Mint(ctx context.Context, user, host string, ttl time.Duration) (*caclient.Cert, error) {
	_, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	caSigner, err := ssh.NewSignerFromKey(caPriv)
	if err != nil {
		return nil, err
	}
	_, cliPriv, _ := ed25519.GenerateKey(rand.Reader)
	cliSigner, err := ssh.NewSignerFromKey(cliPriv)
	if err != nil {
		return nil, err
	}
	cert := &ssh.Certificate{
		CertType:        ssh.UserCert,
		Key:             cliSigner.PublicKey(),
		ValidPrincipals: []string{user + "@" + host, user},
		ValidAfter:      uint64(time.Now().Add(-time.Minute).Unix()),
		ValidBefore:     uint64(time.Now().Add(ttl).Unix()),
		Permissions:     ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}},
	}
	if err := cert.SignCert(rand.Reader, caSigner); err != nil {
		return nil, err
	}
	return &caclient.Cert{PrivateKey: cliPriv, CertPEM: cert.Marshal(), Principal: user + "@" + host}, nil
}

func TestUnknownSession(t *testing.T) {
	srv := testServer(t, Config{})
	defer srv.Close()
	resp := doReq(t, "GET", srv.URL+"/v1/sessions/deadbeef/events?cursor=0", "test-token", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEventLogCursor(t *testing.T) {
	l := &eventLog{}
	l.append("out", []byte("a"))
	l.append("out", []byte("b"))
	evs, next := l.since(0)
	if len(evs) != 2 || next != 2 {
		t.Fatalf("want 2/2, got %d/%d", len(evs), next)
	}
	if string(mustB64(evs[0].Data)) != "a" {
		t.Errorf("ev0 %q", evs[0].Data)
	}
	evs, next = l.since(2)
	if len(evs) != 0 || next != 2 {
		t.Errorf("want 0/2, got %d/%d", len(evs), next)
	}
	evs, next = l.since(99)
	if len(evs) != 0 || next != 2 {
		t.Errorf("overflow cursor: want 0/2, got %d/%d", len(evs), next)
	}
}

func validKH(t *testing.T) string {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return knownhosts.Line([]string{"h:22"}, signer.PublicKey())
}

func mustB64(s string) []byte {
	b, _ := base64.StdEncoding.DecodeString(s)
	return b
}

var _ = os.Getenv
var _ = bytes.MinRead
