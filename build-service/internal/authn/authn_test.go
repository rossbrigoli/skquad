package authn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, doc any) string {
	t.Helper()
	raw, _ := json.Marshal(doc)
	p := filepath.Join(t.TempDir(), "principals.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHashTokenMatchesCPConvention(t *testing.T) {
	// base64 RawStdEncoding of sha256("hunter2") — precomputed with an
	// independent implementation (python hashlib), identical convention
	// to control-plane hashCredential / tool-gateway auth.HashCredential.
	const want = "9S+9MrKzuG/4jvbEkGKChfSCrxXdyylUH5S89Saj9sc"
	got := HashToken("hunter2")
	if got != want {
		t.Fatalf("hash convention drift: got %q want %q", got, want)
	}
	if HashToken("hunter3") == got {
		t.Fatal("collision between different tokens")
	}
}

func TestAuthenticate(t *testing.T) {
	doc := map[string]any{"principals": []map[string]any{
		{"id": "sherlock", "agent_id": "48da5ca3", "token_b64sha256": HashToken("tok-sherlock"), "repos_allow": []string{"github.com/rossbrigoli/*"}, "destinations_allow": []string{"ghcr.io/rossbrigoli/agent-builds/"}, "active": true},
		{"id": "gone", "token_b64sha256": HashToken("tok-gone"), "active": false},
	}}
	s, err := LoadStore(writeFile(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Authenticate("tok-sherlock"); p == nil || p.ID != "sherlock" {
		t.Fatalf("valid token not authenticated: %+v", p)
	}
	if p := s.Authenticate("tok-gone"); p != nil {
		t.Fatal("inactive principal must not authenticate")
	}
	if p := s.Authenticate("bogus"); p != nil {
		t.Fatal("unknown token must not authenticate")
	}
	if p := s.Authenticate(""); p != nil {
		t.Fatal("empty token must not authenticate")
	}
}

func TestLoadStoreRejectsBadFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.json")
	_ = os.WriteFile(p, []byte(`{"principals":[]}`), 0o600)
	if _, err := LoadStore(p); err == nil {
		t.Fatal("empty principals must be rejected")
	}
	_ = os.WriteFile(p, []byte(`{"principals":[{"id":"a","token_b64sha256":"x"},{"id":"a","token_b64sha256":"y"}]}`), 0o600)
	if _, err := LoadStore(p); err == nil {
		t.Fatal("duplicate ids must be rejected")
	}
	_ = os.WriteFile(p, []byte(`{"principals":[{"id":"a"}]}`), 0o600)
	if _, err := LoadStore(p); err == nil {
		t.Fatal("missing token hash must be rejected")
	}
}
