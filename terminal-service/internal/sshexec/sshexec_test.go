package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/rossbrigoli/skquad/terminal-service/internal/caclient"
)

// testSSHServer is a minimal in-process SSH server exercising the
// client paths: publickey auth, exec, shell (PTY), exit statuses.
type testSSHServer struct {
	listener   net.Listener
	hostKey    ssh.Signer
	clientKey  ed25519.PrivateKey
	clientPub  ssh.PublicKey
	knownHosts string

	execDelay time.Duration
	execFn    func(cmd string) (stdout, stderr string, code int)
}

func newTestSSHServer(t *testing.T) *testSSHServer {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, clientPrivKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPrivKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &testSSHServer{
		listener:  ln,
		hostKey:   hostSigner,
		clientKey: clientPrivKey,
		clientPub: clientSigner.PublicKey(),
		execFn:    func(cmd string) (string, string, int) { return "out:" + cmd, "", 0 },
	}
	srv.knownHosts = knownhosts.Line([]string{ln.Addr().String()}, hostSigner.PublicKey())
	go srv.serve()
	return srv
}

func (s *testSSHServer) portStr() string { return s.listener.Addr().String() }

func (s *testSSHServer) port() int {
	_, port, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		panic(err)
	}
	var p int
	fmt.Sscanf(port, "%d", &p)
	return p
}

func (s *testSSHServer) serve() {
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(s.clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	config.AddHostKey(s.hostKey)
	for {
		nConn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(nConn, config)
	}
}

func (s *testSSHServer) handleConn(nConn net.Conn, config *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		nConn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chReqs)
	}
}

func (s *testSSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	var (
		mu      sync.Mutex
		shellRe bool
		execCmd string
		ready   = make(chan struct{})
		settled sync.Once
	)
	settle := func() {
		settled.Do(func() { close(ready) })
	}

	go func() {
		for r := range reqs {
			switch r.Type {
			case "pty-req":
				r.Reply(true, nil)
			case "exec":
				var req struct {
					Command string
				}
				_ = ssh.Unmarshal(r.Payload, &req)
				mu.Lock()
				execCmd = req.Command
				mu.Unlock()
				r.Reply(true, nil)
				settle()
			case "shell":
				mu.Lock()
				shellRe = true
				mu.Unlock()
				r.Reply(true, nil)
				settle()
			default:
				r.Reply(true, nil)
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		return
	}
	time.Sleep(s.execDelay)

	mu.Lock()
	shell := shellRe
	cmd := execCmd
	mu.Unlock()

	if shell {
		// Echo loop: every line received is echoed as "echo:<line>".
		go func() {
			buf := make([]byte, 1024)
			var pending string
			for {
				n, err := ch.Read(buf)
				if n > 0 {
					pending += string(buf[:n])
					for {
						idx := strings.IndexByte(pending, '\n')
						if idx < 0 {
							break
						}
						line := pending[:idx+1]
						pending = pending[idx+1:]
						fmt.Fprintf(ch, "echo:%s", line)
					}
				}
				if err != nil {
					return
				}
			}
		}()
		// Keep the channel open until the client closes it.
		time.Sleep(30 * time.Second)
		return
	}

	stdout, stderr, code := s.execFn(cmd)
	if stdout != "" {
		fmt.Fprint(ch, stdout)
	}
	if stderr != "" {
		fmt.Fprint(ch.Stderr(), stderr)
	}
	status := ssh.Marshal(struct{ Code uint32 }{uint32(code)})
	ch.SendRequest("exit-status", false, status)
	ch.Close()
}

// ---- tests ----

func staticAuth(keyPEM, kh string) Auth {
	return Auth{Mode: "static_key", PrivateKeyPEM: keyPEM, KnownHosts: kh}
}

func TestExecSuccess(t *testing.T) {
	srv := newTestSSHServer(t)
	defer srv.listener.Close()
	res, err := Exec(context.Background(), Request{
		Host: "127.0.0.1", Port: srv.port(), User: "tester",
		Command: "uptime", Timeout: 10 * time.Second,
		Auth: staticAuth(pemED25519(t, srv.clientKey), srv.knownHosts),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "out:uptime" {
		t.Errorf("res = %+v", res)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	srv := newTestSSHServer(t)
	defer srv.listener.Close()
	srv.execFn = func(cmd string) (string, string, int) { return "", "boom", 7 }
	res, err := Exec(context.Background(), Request{
		Host: "127.0.0.1", Port: srv.port(), User: "tester",
		Command: "false", Timeout: 10 * time.Second,
		Auth: staticAuth(pemED25519(t, srv.clientKey), srv.knownHosts),
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 7 || !strings.Contains(res.Stderr, "boom") {
		t.Errorf("res = %+v", res)
	}
}

func TestExecTimeout(t *testing.T) {
	srv := newTestSSHServer(t)
	defer srv.listener.Close()
	srv.execDelay = 2 * time.Second
	srv.execFn = func(cmd string) (string, string, int) { return "late", "", 0 }
	_, err := Exec(context.Background(), Request{
		Host: "127.0.0.1", Port: srv.port(), User: "tester",
		Command: "sleep 100", Timeout: 300 * time.Millisecond,
		Auth: staticAuth(pemED25519(t, srv.clientKey), srv.knownHosts),
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}

func TestHostVerificationRequired(t *testing.T) {
	srv := newTestSSHServer(t)
	defer srv.listener.Close()
	_, err := Exec(context.Background(), Request{
		Host: "127.0.0.1", Port: srv.port(), User: "tester",
		Command: "ls", Timeout: 5 * time.Second,
		Auth: staticAuth(pemED25519(t, srv.clientKey), ""),
	})
	if !errors.Is(err, ErrHostVerificationRequired) {
		t.Fatalf("want ErrHostVerificationRequired, got %v", err)
	}
}

func TestHostVerificationFailed(t *testing.T) {
	srv := newTestSSHServer(t)
	defer srv.listener.Close()
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, oerr := ssh.NewSignerFromKey(otherPriv)
	if oerr != nil {
		t.Fatal(oerr)
	}
	otherKH := knownhosts.Line([]string{srv.portStr()}, otherSigner.PublicKey())
	_, err := Exec(context.Background(), Request{
		Host: "127.0.0.1", Port: srv.port(), User: "tester",
		Command: "ls", Timeout: 5 * time.Second,
		Auth: staticAuth(pemED25519(t, srv.clientKey), otherKH),
	})
	if !errors.Is(err, ErrHostVerificationFailed) {
		t.Fatalf("want ErrHostVerificationFailed, got %v", err)
	}
}

func TestSessionOpenSendClose(t *testing.T) {
	srv := newTestSSHServer(t)
	defer srv.listener.Close()
	var mu sync.Mutex
	var got []string
	sess, err := OpenSession(context.Background(), Request{
		Host: "127.0.0.1", Port: srv.port(), User: "tester",
		Auth: staticAuth(pemED25519(t, srv.clientKey), srv.knownHosts),
	}, func(chunk []byte) {
		mu.Lock()
		got = append(got, string(chunk))
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if err := sess.Send([]byte("hello\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	joined := ""
	for time.Now().Before(deadline) {
		mu.Lock()
		joined = strings.Join(got, "")
		mu.Unlock()
		if strings.Contains(joined, "echo:hello") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(joined, "echo:hello") {
		t.Fatalf("no echo output, got %q", joined)
	}
	sess.Close()
	if !sess.Closed() {
		t.Error("session should be closed")
	}
}

func TestCAMinterInvokedWithPrincipals(t *testing.T) {
	var gotUser, gotHost string
	mint := &fakeCAMinter{fn: func(user, host string, ttl time.Duration) (*caclient.Cert, error) {
		gotUser, gotHost = user, host
		return nil, errors.New("ca_unavailable")
	}}
	// syntactically valid known_hosts so the failure we assert is the
	// CA mint, not host-key parsing.
	_, anyPriv, _ := ed25519.GenerateKey(rand.Reader)
	anySigner, kerr := ssh.NewSignerFromKey(anyPriv)
	if kerr != nil {
		t.Fatal(kerr)
	}
	_, err := Exec(context.Background(), Request{
		Host: "h", User: "ops", Command: "ls", Timeout: time.Second,
		Auth:   Auth{Mode: "ca", KnownHosts: knownhosts.Line([]string{"h:22"}, anySigner.PublicKey())},
		CAMint: mint,
	})
	if gotUser != "ops" || gotHost != "h" {
		t.Errorf("mint args wrong: %s %s", gotUser, gotHost)
	}
	if err == nil || !strings.Contains(err.Error(), "ca_unavailable") {
		t.Errorf("want ca_unavailable, got %v", err)
	}
}

type fakeCAMinter struct {
	fn func(user, host string, ttl time.Duration) (*caclient.Cert, error)
}

func (f *fakeCAMinter) Mint(ctx context.Context, user, host string, ttl time.Duration) (*caclient.Cert, error) {
	return f.fn(user, host, ttl)
}

func pemED25519(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(key, "test")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}
