// Package caclient mints short-lived OpenSSH user certificates from an
// SSH certificate authority (step-ca in production) for the terminal
// service's CA auth mode (docs/tool-gateway.md §6.6, TG-10).
//
// Per exec/session call we generate an EPHEMERAL ed25519 keypair and
// ask the CA to certify it with:
//
//	valid_principals = [user+"@"+host, user]
//	validity       = clamp(cert_ttl_minutes, 5..60) minutes from now
//
// The signed certificate + ephemeral private key are used as the SSH
// client auth material; nothing is persisted beyond the call.
//
// DECISION: the CA transport is behind the SSHCA interface. The default
// HTTPTokenCA speaks a minimal JSON contract — POST {base}/sign/ssh
// {"csr": b64(openssh cert request), "ott": "<token>"} →
// {"crt": b64(signed cert)}. step-ca's production provisioner flow
// wraps this in a JWS signed by the provisioner key; that adapter is
// finalized at deploy-integration time (parent owns step-ca bootstrap).
// If the live CA needs the JWS variant, a JWSAdapter implements SSHCA
// without touching callers.
package caclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/crypto/ssh"
)

// MinTTL / MaxTTL bound certificate lifetimes (§6.6: 15–60 min ceiling;
// we floor at 5 to survive clock skew on tiny calls).
const (
	MinTTL = 5 * time.Minute
	MaxTTL = 60 * time.Minute
)

// Cert is a minted certificate plus the ephemeral signing key.
type Cert struct {
	PrivateKey ed25519.PrivateKey
	CertPEM    []byte // openssh wire-format certificate (as signed)
	Principal  string // user@host (informational)
}

// SSHCA signs OpenSSH certificate requests.
type SSHCA interface {
	// SignSSHCert returns the signed certificate wire bytes for a
	// request built from pub with the given principals/validity.
	SignSSHCert(ctx context.Context, pub ssh.PublicKey, principals []string, validAfter, validBefore time.Time) ([]byte, error)
}

// BuildCertRequest produces the OpenSSH certificate signing request blob
// per PROTOCOL.certkeys §"Certificate request format": cert type,
// nonce, public key, key id, principals (sequential strings),
// valid_after/valid_before (uint64 unix seconds), critical options,
// extensions, reserved, then a signature over everything preceding by
// the requesting key.
//
// The field order below mirrors PROTOCOL.certkeys; the mock CA test
// asserts the shape round-trips. Production integration validates
// against the live step-ca decoder.
func BuildCertRequest(signer ssh.Signer, principals []string, validAfter, validBefore time.Time, keyID string) ([]byte, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("caclient: nonce: %w", err)
	}
	certType := certTypeForKey(signer.PublicKey())
	if certType == "" {
		return nil, fmt.Errorf("caclient: unsupported key type %s", signer.PublicKey().Type())
	}
	type certReqBody struct {
		CertType        string
		Nonce           []byte
		PubKey          interface{} `ssh:"pubkey"`
		KeyID           string
		Principals      []string
		ValidAfter      uint64
		ValidBefore     uint64
		CriticalOptions map[string]string
		Extensions      map[string]string
		Reserved        []byte
	}
	body := ssh.Marshal(certReqBody{
		CertType:        certType,
		Nonce:           nonce,
		PubKey:          signer.PublicKey(),
		KeyID:           keyID,
		Principals:      principals,
		ValidAfter:      uint64(validAfter.Unix()),
		ValidBefore:     uint64(validBefore.Unix()),
		CriticalOptions: map[string]string{},
		Extensions:      defaultExtensions(),
		Reserved:        nil,
	})
	sig, err := signer.Sign(rand.Reader, body)
	if err != nil {
		return nil, fmt.Errorf("caclient: sign request: %w", err)
	}
	return append(body, ssh.Marshal(*sig)...), nil
}

func defaultExtensions() map[string]string {
	// Standard OpenSSH client-cert extensions (no force-command, no
	// permit-X11 / agent-forwarding / port-forwarding — §6.6 forbids
	// tunnelling; we deliberately omit them so the CA never grants more
	// than the terminal needs).
	return map[string]string{
		"permit-pty": "",
	}
}

func certTypeForKey(pub ssh.PublicKey) string {
	switch pub.Type() {
	case ssh.KeyAlgoED25519:
		return ssh.CertAlgoED25519v01
	case ssh.KeyAlgoRSA:
		return ssh.CertAlgoRSAv01
	case ssh.KeyAlgoECDSA256:
		return ssh.CertAlgoECDSA256v01
	default:
		return ""
	}
}

// NewEphemeralKeypair generates the per-call ed25519 key + signer.
func NewEphemeralKeypair() (ed25519.PrivateKey, ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, err
	}
	return priv, signer, nil
}

// MintCert builds the full request/sign/auth pipeline for one call.
func MintCert(ctx context.Context, ca SSHCA, user, host string, ttl time.Duration) (*Cert, error) {
	if ttl < MinTTL {
		ttl = MinTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	priv, signer, err := NewEphemeralKeypair()
	if err != nil {
		return nil, err
	}
	principal := user + "@" + host
	principals := []string{principal, user}
	now := time.Now()
	certBytes, err := ca.SignSSHCert(ctx, signer.PublicKey(), principals, now, now.Add(ttl))
	if err != nil {
		return nil, err
	}
	// Validate what the CA returned before trusting it for auth.
	parsed, err := ssh.ParsePublicKey(certBytes)
	if err != nil {
		return nil, fmt.Errorf("caclient: CA returned unparseable cert: %w", err)
	}
	c, ok := parsed.(*ssh.Certificate)
	if !ok {
		return nil, fmt.Errorf("caclient: CA returned non-certificate key")
	}
	if c.CertType != ssh.UserCert {
		return nil, fmt.Errorf("caclient: CA returned non-user certificate")
	}
	return &Cert{PrivateKey: priv, CertPEM: certBytes, Principal: principal}, nil
}

// MintCertWithRequest is the concrete path used by the service: it
// builds the signed request with the ephemeral key and POSTs it.
func MintCertWithRequest(ctx context.Context, endpoint, token string, client *http.Client, user, host string, ttl time.Duration) (*Cert, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("ca_unavailable")
	}
	if ttl < MinTTL {
		ttl = MinTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	priv, signer, err := NewEphemeralKeypair()
	if err != nil {
		return nil, err
	}
	principal := user + "@" + host
	principals := []string{principal, user}
	now := time.Now()
	reqBlob, err := BuildCertRequest(signer, principals, now, now.Add(ttl), "skquad-terminal:"+principal)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]string{
		"csr": base64.StdEncoding.EncodeToString(reqBlob),
		"ott": token,
	})
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ca_unavailable: %w", err)
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("ca_unavailable: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ca_error: status %d", httpResp.StatusCode)
	}
	var out struct {
		CRT string `json:"crt"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.CRT == "" {
		return nil, fmt.Errorf("ca_error: bad response")
	}
	certBytes, err := base64.StdEncoding.DecodeString(out.CRT)
	if err != nil {
		return nil, fmt.Errorf("ca_error: bad cert encoding")
	}
	parsed, err := ssh.ParsePublicKey(certBytes)
	if err != nil {
		return nil, fmt.Errorf("ca_error: unparseable cert: %w", err)
	}
	c, ok := parsed.(*ssh.Certificate)
	if !ok || c.CertType != ssh.UserCert {
		return nil, fmt.Errorf("ca_error: not a user certificate")
	}
	return &Cert{PrivateKey: priv, CertPEM: certBytes, Principal: principal}, nil
}
