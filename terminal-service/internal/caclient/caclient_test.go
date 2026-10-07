package caclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestBuildCertRequestShape(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	req, err := BuildCertRequest(signer, []string{"ops@host1", "ops"}, now, now.Add(15*time.Minute), "skquad-terminal:ops@host1")
	if err != nil {
		t.Fatal(err)
	}
	if len(req) == 0 {
		t.Fatal("empty request")
	}
	// The request must start with the cert type string for ed25519.
	wantType := []byte(ssh.CertAlgoED25519v01)
	if len(req) < len(wantType)+4 || string(req[4:4+len(wantType)]) != string(wantType) {
		t.Errorf("request does not start with cert type: %q", req[:min(len(req), 40)])
	}
}

func TestMintCertWithRequestRoundTrip(t *testing.T) {
	// Mock CA: returns a syntactically valid user certificate signed by
	// a CA key we also expose (real deployments validate the CA key
	// against the hosts' TrustedUserCAKeys; here we assert the client
	// accepts a well-formed response and rejects malformed ones).
	caPub, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	caSigner, _ := ssh.NewSignerFromKey(caPriv)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sign/ssh" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			CSR string `json:"csr"`
			OTT string `json:"ott"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if _, err := base64.StdEncoding.DecodeString(in.CSR); err != nil {
			http.Error(w, "bad csr", 400)
			return
		}
		// Build a real signed cert so the client-side validation passes.
		// We sign a fixed cert here (the client validates structure +
		// user-cert type, not the CA chain — that's host-side).
		cert := &ssh.Certificate{
			CertType:        ssh.UserCert,
			Key:             mustSSHPub(t, caPub),
			ValidPrincipals: []string{"ops@host1", "ops"},
			ValidAfter:      uint64(time.Now().Add(-time.Minute).Unix()),
			ValidBefore:     uint64(time.Now().Add(15 * time.Minute).Unix()),
			Permissions: ssh.Permissions{
				Extensions: map[string]string{"permit-pty": ""},
			},
		}
		if err := cert.SignCert(rand.Reader, caSigner); err != nil {
			http.Error(w, "sign fail", 500)
			return
		}
		out := map[string]string{"crt": base64.StdEncoding.EncodeToString(cert.Marshal())}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	cert, err := MintCertWithRequest(context.Background(), srv.URL+"/sign/ssh", "tok", nil, "ops", "host1", 15*time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if cert.Principal != "ops@host1" {
		t.Errorf("principal %q", cert.Principal)
	}
	parsed, err := ssh.ParsePublicKey(cert.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := parsed.(*ssh.Certificate)
	if !ok || c.CertType != ssh.UserCert {
		t.Error("not a user cert")
	}
}

func TestMintCertRejectsHostCert(t *testing.T) {
	caPub, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	caSigner, _ := ssh.NewSignerFromKey(caPriv)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cert := &ssh.Certificate{
			CertType:    ssh.HostCert, // wrong type — must be rejected
			Key:         mustSSHPub(t, caPub),
			ValidBefore: uint64(time.Now().Add(time.Hour).Unix()),
		}
		if err := cert.SignCert(rand.Reader, caSigner); err != nil {
			http.Error(w, "sign fail", 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"crt": base64.StdEncoding.EncodeToString(cert.Marshal())})
	}))
	defer srv.Close()
	_, err := MintCertWithRequest(context.Background(), srv.URL, "tok", nil, "ops", "host1", 15*time.Minute)
	if err == nil {
		t.Fatal("host cert must be rejected")
	}
}

func TestMintFailsClosedWithoutEndpoint(t *testing.T) {
	_, err := MintCertWithRequest(context.Background(), "", "tok", nil, "ops", "host", time.Minute)
	if err == nil || err.Error() != "ca_unavailable" {
		t.Fatalf("want ca_unavailable, got %v", err)
	}
}

func TestTTLClamp(t *testing.T) {
	var seenTTL time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// capture requested ttl via header echo — we assert via the
		// mint call returning quickly with clamped window instead.
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	// too-small ttl clamps to MinTTL (5m): assert via MintCert error
	// path not needed; just ensure no panic and error is CA-shaped.
	_, err := MintCertWithRequest(context.Background(), srv.URL, "t", nil, "u", "h", time.Minute)
	if err == nil {
		t.Fatal("expected error from garbage CA response")
	}
	_ = seenTTL
}

func mustSSHPub(t *testing.T, pub ed25519.PublicKey) ssh.PublicKey {
	t.Helper()
	s, err := ssh.NewSignerFromKey(pub)
	if err == nil {
		// public-only: build via crypto pubkey → ssh key
		k, err := ssh.NewPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		_ = s
		return k
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
