package principal

import (
	"strings"
	"testing"
)

const validDoc = `{
  "principals": [
    {
      "id": "sherlock",
      "agent_id": "48da5ca3-5657-4115-a49e-caa643399683",
      "display_name": "Sherlock",
      "token_sha256_b64": "3RP2W2FmZmVyLW5vdC1hLXJlYWwtaGFzaA==",
      "repos_allow": ["rossbrigoli/*"],
      "destinations_allow": ["docker.io/brigss007/agent-builds/"]
    }
  ]
}`

func TestParseValid(t *testing.T) {
	s, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Count() != 1 {
		t.Fatalf("count=%d", s.Count())
	}
	if p := s.ByID("sherlock"); p == nil || p.AgentID == "" {
		t.Fatal("principal lookup failed")
	}
}

func TestParseRejectsBadDocs(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"empty list", `{"principals": []}`},
		{"missing token", `{"principals":[{"id":"a","repos_allow":["x/*"],"destinations_allow":["p/"]}]}`},
		{"bad base64", `{"principals":[{"id":"a","token_sha256_b64":"!!!","repos_allow":["x/*"],"destinations_allow":["p/"]}]}`},
		{"empty repos_allow", `{"principals":[{"id":"a","token_sha256_b64":"AAAA","repos_allow":[],"destinations_allow":["p/"]}]}`},
		{"dest no trailing slash", `{"principals":[{"id":"a","token_sha256_b64":"AAAA","repos_allow":["x/*"],"destinations_allow":["p"]}]}`},
		{"unknown field", `{"principals":[{"id":"a","token_sha256_b64":"AAAA","repos_allow":["x/*"],"destinations_allow":["p/"],"evil":1}]}`},
		{"dup id", `{"principals":[
			{"id":"a","token_sha256_b64":"QUFB","repos_allow":["x/*"],"destinations_allow":["p/"]},
			{"id":"a","token_sha256_b64":"QkJC","repos_allow":["x/*"],"destinations_allow":["p/"]}]}`},
		{"dup token", `{"principals":[
			{"id":"a","token_sha256_b64":"QUFB","repos_allow":["x/*"],"destinations_allow":["p/"]},
			{"id":"b","token_sha256_b64":"QUFB","repos_allow":["x/*"],"destinations_allow":["p/"]}]}`},
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c.doc)); err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	tok := "s3cret-agent-token"
	doc := strings.Replace(validDoc, "3RP2W2FmZmVyLW5vdC1hLXJlYWwtaGFzaA==", HashToken(tok), 1)
	s, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p := s.Authenticate(tok); p == nil || p.ID != "sherlock" {
		t.Fatal("valid token must authenticate")
	}
	if p := s.Authenticate("wrong-token"); p != nil {
		t.Fatal("wrong token must not authenticate")
	}
	if p := s.Authenticate(""); p != nil {
		t.Fatal("empty token must not authenticate")
	}
}

func TestDisabledPrincipalDenied(t *testing.T) {
	tok := "tok123"
	doc := strings.Replace(validDoc, `"disabled"` , `"disabled"`, 1)
	// inject disabled=true
	doc = strings.Replace(doc, `"display_name": "Sherlock",`, `"display_name": "Sherlock", "disabled": true,`, 1)
	doc = strings.Replace(doc, "3RP2W2FmZmVyLW5vdC1hLXJlYWwtaGFzaA==", HashToken(tok), 1)
	s, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p := s.Authenticate(tok); p != nil {
		t.Fatal("disabled principal must not authenticate")
	}
}

func TestHashTokenStable(t *testing.T) {
	if HashToken("x") != HashToken("x") {
		t.Fatal("hash must be deterministic")
	}
	if HashToken("x") == HashToken("y") {
		t.Fatal("different inputs must differ")
	}
}
