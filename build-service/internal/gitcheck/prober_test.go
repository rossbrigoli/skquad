package gitcheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func pkt(s string) string {
	return fmt.Sprintf("%04x", 4+len(s)) + s
}

func TestParsePktLinesSmartHTTP(t *testing.T) {
	body := pkt("# git-upload-pack 'rossbrigoli/skquad'\n") +
		pkt("0123456789abcdef0123456789abcdef01234567 HEAD\x00multi_ack thin-pack symref=HEAD:refs/heads/main\n") +
		pkt("0123456789abcdef0123456789abcdef01234567\trefs/heads/main\n") +
		pkt("ffffffffffffffffffffffffffffffffffffffff refs/tags/v1.0\n") +
		"0000"
	lines, err := ParsePktLines([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0123456789abcdef0123456789abcdef01234567 HEAD",
		"0123456789abcdef0123456789abcdef01234567\trefs/heads/main",
		"ffffffffffffffffffffffffffffffffffffffff refs/tags/v1.0",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines: %v", len(lines), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d: got %q want %q", i, lines[i], want[i])
		}
	}
}

func TestParsePktLinesRejectsGarbage(t *testing.T) {
	if _, err := ParsePktLines([]byte("zzzzhello")); err == nil {
		t.Fatal("bad hex length must error")
	}
	if _, err := ParsePktLines([]byte("0002ab")); err == nil {
		t.Fatal("length < 4 payload must error")
	}
}

func TestListRefsViaHTTP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rossbrigoli/skquad.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") != "git-upload-pack" {
			http.Error(w, "nope", 403)
			return
		}
		_, _ = w.Write([]byte(
			pkt("# git-upload-pack 'x'\n") +
				pkt("0123456789abcdef0123456789abcdef01234567 HEAD\x00caps\n") +
				pkt("0123456789abcdef0123456789abcdef01234567\trefs/heads/main\n") +
				"0000"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := NewHTTPProber(2 * time.Second)
	refs, err := p.ListRefs(context.Background(), srv.URL+"/rossbrigoli/skquad.git")
	if err != nil {
		t.Fatal(err)
	}
	if refs["refs/heads/main"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("refs wrong: %v", refs)
	}
}

func TestMatchRevision(t *testing.T) {
	refs := map[string]string{
		"HEAD":            "0123456789abcdef0123456789abcdef01234567",
		"refs/heads/main": "0123456789abcdef0123456789abcdef01234567",
		"refs/tags/v1.0":  "ffffffffffffffffffffffffffffffffffffffff",
	}
	cases := []struct {
		rev     string
		want    bool
		wantRef string
	}{
		{"main", true, "refs/heads/main"},
		{"v1.0", true, "refs/tags/v1.0"},
		{"refs/heads/main", true, "refs/heads/main"},
		{"HEAD", true, "HEAD"},
		{"ffffffffffffffffffffffffffffffffffffffff", true, "refs/tags/v1.0"},
		{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", false, ""},
		{"nope", false, ""},
		{"", false, ""},
	}
	for _, tc := range cases {
		ok, ref := MatchRevision(refs, tc.rev)
		if ok != tc.want || (tc.want && ref != tc.wantRef) {
			t.Fatalf("rev %q: got (%v,%q) want (%v,%q)", tc.rev, ok, ref, tc.want, tc.wantRef)
		}
	}
}

func TestListRefsSpaceSeparatedWireFormat(t *testing.T) {
	// The real git smart-HTTP ref advertisement uses SPACE (0x20)
	// between oid and refname (verified against GitHub + kernel.org,
	// 2026-11-19). A TAB-only parser silently yields zero refs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		// pkt-line framing: service header, flush, refs, flush.
		w.Write([]byte("001e# service=git-upload-pack\n0000"))
		// 40 sha + 1 SP + 4 "HEAD" + 1 NUL + 9 "multi_ack" + 1 LF = 56 payload + 4 = 60 = 0x3c
		w.Write([]byte("003c6e9f14bcb48f7daf5a706d77ad2c491220d65698 HEAD\x00multi_ack\n"))
		// 40 sha + 1 SP + 15 "refs/heads/main" + 1 LF = 57 payload + 4 = 61 = 0x3d
		w.Write([]byte("003d6e9f14bcb48f7daf5a706d77ad2c491220d65698 refs/heads/main\n"))
		w.Write([]byte("0000"))
	}))
	defer srv.Close()
	p := NewHTTPProber(2 * time.Second)
	refs, err := p.ListRefs(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %d: %v", len(refs), refs)
	}
	if ok, ref := MatchRevision(refs, "main"); !ok || ref != "refs/heads/main" {
		t.Fatalf("main match: %v %s", ok, ref)
	}
}
