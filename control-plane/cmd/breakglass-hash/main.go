// Command breakglass-hash generates the credentials needed for the skquad
// break-glass admin path.
//
// Usage:
//
//	go run ./cmd/breakglass-hash                      # prompts twice, hides echo
//	go run ./cmd/breakglass-hash --password 'secret'  # non-interactive (avoid: lands in shell history)
//
// Output is ready to paste into a SealedSecret:
//
//	SKQUAD_BREAKGLASS_PASSWORD_HASH=$argon2id$v=19$m=65536,t=3,p=2$...$...
//	SKQUAD_BREAKGLASS_JWT_KEY=<64 hex chars>
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/crypto/argon2"
	"golang.org/x/term"
)

// Parameters: 64 MiB, 3 iterations, parallelism 2 — a sane OWASP-aligned
// baseline for an interactive login that is still well under a second.
const (
	timeCost   = 3
	memoryCost = 65536
	threads    = 2
	keyLen     = 32
	saltLen    = 16
)

func main() {
	password := flag.String("password", "", "password to hash (omit for interactive prompt)")
	username := flag.String("username", "breakglass", "break-glass username")
	confirm := flag.Bool("confirm", true, "ask for the password twice (disable for scripted use)")
	flag.Parse()

	pw := *password
	if pw == "" {
		var err error
		pw, err = promptHidden("Break-glass password: ")
		if err != nil {
			fail(err)
		}
		if *confirm {
			check, err := promptHidden("Confirm password:    ")
			if err != nil {
				fail(err)
			}
			if pw != check {
				fail(fmt.Errorf("passwords do not match"))
			}
		}
	}
	if len(pw) < 20 {
		fail(fmt.Errorf("password is %d chars; use at least 20 (this account is platform_admin)", len(pw)))
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		fail(err)
	}
	hash := argon2.IDKey([]byte(pw), salt, timeCost, memoryCost, threads, keyLen)

	jwtKey := make([]byte, 32)
	if _, err := rand.Read(jwtKey); err != nil {
		fail(err)
	}

	fmt.Println()
	fmt.Println("# Paste into the SealedSecret (SKQUAD_BREAKGLASS_*):")
	fmt.Printf("SKQUAD_BREAKGLASS_USERNAME=%s\n", *username)
	fmt.Printf("SKQUAD_BREAKGLASS_PASSWORD_HASH=$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s\n",
		memoryCost, timeCost, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash))
	fmt.Printf("SKQUAD_BREAKGLASS_JWT_KEY=%x\n", jwtKey)
	fmt.Println()
	fmt.Println("# ConfigMap toggles (flip enabled/disabled without re-sealing):")
	fmt.Println("#   SKQUAD_BREAKGLASS_ENABLED=true")
	fmt.Println("#   SKQUAD_BREAKGLASS_ALLOWED_CIDRS=192.168.68.0/24,100.64.0.0/10")
	fmt.Println("#   SKQUAD_BREAKGLASS_TOKEN_TTL=60m")
	fmt.Println("#   SKQUAD_BREAKGLASS_MAX_ATTEMPTS=5")

	// Scrub the plaintext password from our own view of memory as best we can.
	pw = ""
	_ = pw
}

func promptHidden(prompt string) (string, error) {
	if term.IsTerminal(int(syscall.Stdin)) {
		b, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// Non-tty fallback (e.g. piped), so the CLI stays scriptable.
	fmt.Fprint(os.Stderr, prompt+"(non-tty, reading one line from stdin)\n")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
