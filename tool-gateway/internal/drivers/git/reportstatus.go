package git

// Streaming git pkt-line / report-status parser (TG-4b, docs §6.5).
//
// The receive-pack response is a pkt-line stream:
//
//	0009unpack ok\n
//	000aok refs/heads/main\n
//	000eng refs/other bad ref\n
//	0000            (flush)
//
// The tee parses each pkt-line as it flows toward the client and
// collects the "ok refs/..." / "ng refs/... <err>" lines for the
// audit record WITHOUT buffering the whole stream: bytes are written
// through per pkt-line and flushed, exactly as the plain streaming
// path does. A payload that is not valid pkt-line aborts parsing with
// an error (the bytes already written cannot be unwritten; the audit
// simply records what was parsed up to that point plus the stream
// error).

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	pktLineHeaderLen = 4
	// pktFlushLen is the flush-pkt "0000"; 0001/0002 are delimit/hash
	// markers we pass through without parsing.
	pktMaxLen = 65516 // git's MAX_PKT_DATA + header
)

// reportStatus holds the parsed audit fields of a receive-pack
// report-status (or the subset of it seen before an error).
type reportStatus struct {
	Unpack   string
	Advanced []string
	Rejected []string
}

// teeReportStatus copies src to dst pkt-line by pkt-line, flushing
// after every line, and parses report-status fields on the way. It
// is used for git-receive-pack responses so the audit can name the
// refs the push actually advanced.
func teeReportStatus(src io.Reader, dst io.Writer, flusher http.Flusher) (*reportStatus, error) {
	rs := &reportStatus{}
	hdr := make([]byte, pktLineHeaderLen)
	for {
		if _, err := io.ReadFull(src, hdr); err != nil {
			if err == io.EOF {
				return rs, nil
			}
			return rs, fmt.Errorf("pkt-line header: %w", err)
		}
		n, perr := strconv.ParseUint(string(hdr), 16, 32)
		if perr != nil {
			return rs, fmt.Errorf("pkt-line header not hex")
		}
		// Flush pkt (0000) and special pkts (0001/0002): pass the
		// header through; 0000 ends the report-status section.
		if n <= 2 {
			if _, werr := dst.Write(hdr); werr != nil {
				return rs, werr
			}
			flush(flusher)
			if n == 0 {
				return rs, nil
			}
			continue
		}
		if n > pktMaxLen {
			return rs, fmt.Errorf("pkt-line length %d exceeds protocol max", n)
		}
		payload := make([]byte, int(n)-pktLineHeaderLen)
		if _, err := io.ReadFull(src, payload); err != nil {
			return rs, fmt.Errorf("pkt-line payload: %w", err)
		}
		if _, werr := dst.Write(hdr); werr != nil {
			return rs, werr
		}
		if _, werr := dst.Write(payload); werr != nil {
			return rs, werr
		}
		flush(flusher)
		parseReportLines(rs, string(payload))
	}
}

// parseReportLines consumes one pkt-line payload (may hold multiple
// \n-separated report lines) and records ok/ng/unpack entries.
func parseReportLines(rs *reportStatus, payload string) {
	for _, line := range strings.Split(strings.TrimRight(payload, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "ok "):
			rs.Advanced = append(rs.Advanced, strings.TrimSpace(strings.TrimPrefix(line, "ok ")))
		case strings.HasPrefix(line, "ng "):
			rs.Rejected = append(rs.Rejected, strings.TrimSpace(strings.TrimPrefix(line, "ng ")))
		case strings.HasPrefix(line, "unpack "):
			rs.Unpack = strings.TrimSpace(strings.TrimPrefix(line, "unpack "))
		}
	}
}

func flush(f http.Flusher) {
	if f != nil {
		f.Flush()
	}
}
