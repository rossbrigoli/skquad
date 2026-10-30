// Package gitcheck verifies that a requested git revision exists before a
// build is accepted, using a minimal git smart-HTTP ls-remote (pkt-line)
// probe. No clone, no credentials required for public repos; the probe
// only ever targets hosts on the configured git_hosts_allow floor.
//
// A deliberately tiny protocol implementation is preferred over a full
// git library here: the service image stays slim (distroless, no git
// binary), the attack surface is one GET against an allowlisted host,
// and the parse is fully unit-testable offline.
package gitcheck

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Prober lists refs of a git repo over smart HTTP.
type Prober interface {
	ListRefs(ctx context.Context, httpsURL string) (map[string]string, error)
}

// HTTPProber implements ls-remote via GET {repo}/info/refs?service=git-upload-pack.
type HTTPProber struct {
	client *http.Client
}

// NewHTTPProber builds the prober with a bounded timeout.
func NewHTTPProber(timeout time.Duration) *HTTPProber {
	return &HTTPProber{client: &http.Client{Timeout: timeout}}
}

// ListRefs returns refname -> sha for the repo.
func (p *HTTPProber) ListRefs(ctx context.Context, httpsURL string) (map[string]string, error) {
	u := strings.TrimSuffix(httpsURL, "/") + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("ls-remote request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ls-remote transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ls-remote status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return nil, fmt.Errorf("ls-remote read: %w", err)
	}
	lines, err := ParsePktLines(body)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, ln := range lines {
		// Each ref line: "<sha>\t<refname>" (capabilities are NUL-attached
		// to the first line and stripped by ParsePktLines).
		sha, rest, ok := strings.Cut(ln, "\t")
		if !ok || len(sha) < 40 {
			continue
		}
		ref := strings.TrimSpace(rest)
		if ref == "" {
			continue
		}
		refs[ref] = sha[:40]
	}
	return refs, nil
}

// ParsePktLines decodes pkt-line framed data into payload lines
// (flush packets skipped; NUL capability suffixes truncated; the
// leading "0001"/"0002" service line of smart-HTTP responses is
// skipped).
func ParsePktLines(data []byte) ([]string, error) {
	var out []string
	r := bufio.NewReader(strings.NewReader(string(data)))
	first := true
	for {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if len(out) == 0 && err == io.EOF {
					return nil, fmt.Errorf("empty pkt-line stream")
				}
				return out, nil
			}
			return nil, fmt.Errorf("pkt-line header: %w", err)
		}
		var length int
		if n, err := fmt.Sscanf(string(hdr), "%04x", &length); err != nil || n != 1 {
			return nil, fmt.Errorf("pkt-line bad length %q", string(hdr))
		}
		if length == 0 { // flush packet
			first = false
			continue
		}
		if length < 4 || length > 65536 {
			return nil, fmt.Errorf("pkt-line invalid length %d", length)
		}
		payload := make([]byte, length-4)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("pkt-line payload: %w", err)
		}
		s := string(payload)
		if first && !strings.Contains(s, "\t") {
			// smart-HTTP service advertisement line ("# git-upload-pack ...")
			first = false
			continue
		}
		first = false
		if i := strings.IndexByte(s, 0); i >= 0 {
			s = s[:i] // strip capabilities
		}
		s = strings.TrimSuffix(s, "\n")
		if s != "" {
			out = append(out, s)
		}
	}
}

// MatchRevision reports whether revision exists among refs. Accepted
// forms:
//   - exact ref name present in refs (refs/heads/main, refs/tags/v1, HEAD)
//   - branch shorthand: "main" matching refs/heads/main
//   - tag shorthand: "v1" matching refs/tags/v1
//   - full 40-hex SHA matching any ref tip's commit
//
// Returns the matched ref name (or the sha itself) and whether it matched.
func MatchRevision(refs map[string]string, revision string) (bool, string) {
	if revision == "" {
		return false, ""
	}
	if _, ok := refs[revision]; ok {
		return true, revision
	}
	if _, ok := refs["refs/heads/"+revision]; ok {
		return true, "refs/heads/" + revision
	}
	if _, ok := refs["refs/tags/"+revision]; ok {
		return true, "refs/tags/" + revision
	}
	if isFullSHA(revision) {
		for name, sha := range refs {
			if sha == revision {
				return true, name
			}
		}
	}
	return false, ""
}

func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
