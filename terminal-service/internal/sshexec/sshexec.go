// Package sshexec performs the actual SSH work for the terminal service:
// one-shot exec and interactive PTY sessions, with mandatory host-key
// verification and hard timeouts. No scp/sftp, no port forwarding — the
// client never requests or accepts tunnel channels (§6.6).
package sshexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/rossbrigoli/skquad/terminal-service/internal/caclient"
)

// Errors surfaced to the HTTP layer (mapped to stable codes).
var (
	ErrHostVerificationRequired = errors.New("host_verification_required")
	ErrHostVerificationFailed   = errors.New("host_verification_failed")
	ErrBadKey                   = errors.New("invalid_private_key")
	ErrTimeout                  = errors.New("timed out")
)

// Auth carries per-call authentication material (never logged).
type Auth struct {
	Mode          string // "ca" | "static_key"
	PrivateKeyPEM string // static_key mode only
	CertTTL       time.Duration
	KnownHosts    string // known_hosts-format material (required)
}

// Request is one exec.
type Request struct {
	Host    string
	Port    int
	User    string
	Command string
	Timeout time.Duration
	Auth    Auth
	CAMint  CAMinter
}

// CAMinter abstracts certificate minting so tests can inject fakes.
type CAMinter interface {
	Mint(ctx context.Context, user, host string, ttl time.Duration) (*caclient.Cert, error)
}

// Result is the exec outcome. ExitCode -1 with Err=ErrTimeout on timeout.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// BuildHostKeyCallback parses known_hosts material into a verifying
// callback. Empty/invalid material → ErrHostVerificationRequired:
// there is NO code path that skips host verification.
func BuildHostKeyCallback(knownHostsMaterial string) (ssh.HostKeyCallback, error) {
	trimmed := strings.TrimSpace(knownHostsMaterial)
	if trimmed == "" {
		return nil, ErrHostVerificationRequired
	}
	f, err := os.CreateTemp("", "skquad-knownhosts-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(trimmed + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	cb, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, ErrHostVerificationRequired
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := cb(hostname, remote, key); err != nil {
			return ErrHostVerificationFailed
		}
		return nil
	}, nil
}

// ClientConfig builds the SSH client config for a request. The returned
// config always verifies the host key and never enables agent forwarding
// or tunneling.
func ClientConfig(ctx context.Context, req Request) (*ssh.ClientConfig, error) {
	hostKeyCb, err := BuildHostKeyCallback(req.Auth.KnownHosts)
	if err != nil {
		return nil, err
	}
	port := req.Port
	if port == 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User:            req.User,
		HostKeyCallback: hostKeyCb,
		Timeout:         15 * time.Second,
		// No ClientVersion override; standard algorithms only.
	}
	switch req.Auth.Mode {
	case "static_key":
		signer, err := ssh.ParsePrivateKey([]byte(req.Auth.PrivateKeyPEM))
		if err != nil {
			return nil, ErrBadKey
		}
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	case "ca", "":
		if req.CAMint == nil {
			return nil, errors.New("ca_unavailable")
		}
		cert, err := req.CAMint.Mint(ctx, req.User, req.Host, req.Auth.CertTTL)
		if err != nil {
			if strings.Contains(err.Error(), "ca_unavailable") {
				return nil, errors.New("ca_unavailable")
			}
			return nil, fmt.Errorf("ca_mint_failed: %w", err)
		}
		parsedCert, _, _, _, err := ssh.ParseAuthorizedKey(cert.CertPEM)
		if err != nil {
			return nil, fmt.Errorf("ca_mint_failed: %w", err)
		}
		tlsSigner, err := ssh.NewSignerFromKey(cert.PrivateKey)
		if err != nil {
			return nil, err
		}
		certSigner, err := ssh.NewCertSigner(parsedCert.(*ssh.Certificate), tlsSigner)
		if err != nil {
			return nil, err
		}
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(certSigner)}
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", req.Auth.Mode)
	}
	return cfg, nil
}

// Exec runs one command with a hard timeout. stdout/stderr are captured
// separately. On timeout the session is killed and Result.ExitCode=-1
// with Err=ErrTimeout (partial output preserved).
func Exec(ctx context.Context, req Request) (*Result, error) {
	cfg, err := ClientConfig(ctx, req)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(req.Host, strconv.Itoa(orPort(req.Port)))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		if errors.Is(err, ErrHostVerificationFailed) {
			return nil, err
		}
		return nil, fmt.Errorf("ssh_dial_failed: %w", err)
	}
	defer client.Close()

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh_session_failed: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(req.Command) }()

	select {
	case err := <-done:
		res := &Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}
		if err != nil {
			var ee *ssh.ExitError
			if errors.As(err, &ee) {
				res.ExitCode = ee.ExitStatus()
			} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				res.ExitCode = -1
				res.Stderr += "\ntimed out"
				return res, ErrTimeout
			} else {
				res.ExitCode = -1
				res.Stderr += "\n" + err.Error()
			}
		}
		return res, nil
	case <-runCtx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return &Result{
			Stdout:   stdout.String(),
			Stderr:   stderr.String() + "\ntimed out",
			ExitCode: -1,
		}, ErrTimeout
	}
}

func orPort(p int) int {
	if p == 0 {
		return 22
	}
	return p
}

// Session is a live interactive PTY session.
type Session struct {
	client  *ssh.Client
	session *ssh.Session
	stdin   io.WriteCloser
	// Output callback receives every chunk of pty output with a
	// timestamp. Called from a goroutine; must not block long.
	onOutput func([]byte)
	closed   bool
	mu       sync.Mutex
}

// OpenSession starts an interactive PTY session running the login
// shell. onOutput is invoked for every output chunk (already recorded
// by the caller's recorder wrapper if any).
func OpenSession(ctx context.Context, req Request, onOutput func([]byte)) (*Session, error) {
	cfg, err := ClientConfig(ctx, req)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(req.Host, strconv.Itoa(orPort(req.Port)))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		if errors.Is(err, ErrHostVerificationFailed) {
			return nil, err
		}
		return nil, fmt.Errorf("ssh_dial_failed: %w", err)
	}
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("ssh_session_failed: %w", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 50, 200, modes); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("pty_failed: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}
	outPipe, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}
	// With a PTY, stderr is folded into stdout by the remote pty; we
	// still drain StderrPipe defensively.
	errPipe, _ := session.StderrPipe()

	if err := session.Shell(); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("shell_failed: %w", err)
	}

	s := &Session{client: client, session: session, stdin: stdin, onOutput: onOutput}
	go s.pump(outPipe)
	if errPipe != nil {
		go s.pump(errPipe)
	}
	return s, nil
}

func (s *Session) pump(r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 && s.onOutput != nil {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.onOutput(chunk)
		}
		if err != nil {
			return
		}
	}
}

// Send writes stdin bytes.
func (s *Session) Send(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session closed")
	}
	_, err := s.stdin.Write(b)
	return err
}

// Close terminates the session and its SSH connection.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.session.Signal(ssh.SIGKILL)
	_ = s.session.Close()
	_ = s.client.Close()
}

// Closed reports session state.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
