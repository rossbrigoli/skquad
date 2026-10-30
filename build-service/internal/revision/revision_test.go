package revision

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner scripts git outputs per command.
type fakeRunner struct {
	fn func(args []string) (string, error)
}

func (f fakeRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	return f.fn(args)
}

var (
	shaA = strings.Repeat("a", 40)
	shaB = strings.Repeat("b", 40)
)

func TestResolveNamedRefSingle(t *testing.T) {
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		if args[0] == "ls-remote" {
			return shaA + "\trefs/heads/main\n", nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	}}}
	got, err := r.Resolve(context.Background(), "https://github.com/o/r.git", "main")
	if err != nil || got != shaA {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveRefBranchTagSameSHA(t *testing.T) {
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		return shaA + "\trefs/heads/v1\n" + shaA + "\trefs/tags/v1\n", nil
	}}}
	got, err := r.Resolve(context.Background(), "u", "v1")
	if err != nil || got != shaA {
		t.Fatalf("same-SHA branch+tag must not be ambiguous: got %q err %v", got, err)
	}
}

func TestResolveRefAmbiguous(t *testing.T) {
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		return shaA + "\trefs/heads/x\n" + shaB + "\trefs/tags/x\n", nil
	}}}
	_, err := r.Resolve(context.Background(), "u", "x")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
}

func TestResolveRefNotFound(t *testing.T) {
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		return "", nil
	}}}
	_, err := r.Resolve(context.Background(), "u", "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestResolveProbeFailure(t *testing.T) {
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		return "", errors.New("network down")
	}}}
	_, err := r.Resolve(context.Background(), "u", "main")
	if !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("expected ErrProbeFailed, got %v", err)
	}
}

func TestResolveBadSHAParse(t *testing.T) {
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		return "not-a-sha\trefs/heads/main\n", nil
	}}}
	_, err := r.Resolve(context.Background(), "u", "main")
	if !errors.Is(err, ErrBadResolved) {
		t.Fatalf("expected ErrBadResolved, got %v", err)
	}
}

func TestResolveFullSHAReachable(t *testing.T) {
	calls := 0
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		switch args[0] {
		case "init":
			return "", nil
		case "fetch":
			calls++
			return "", nil
		case "rev-parse":
			return shaA + "\n", nil
		}
		return "", errors.New("unexpected")
	}}}
	got, err := r.Resolve(context.Background(), "https://github.com/o/r.git", shaA)
	if err != nil || got != shaA || calls != 1 {
		t.Fatalf("got %q err %v fetchCalls=%d", got, err, calls)
	}
}

func TestResolveFullSHAUnreachable(t *testing.T) {
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		if args[0] == "fetch" {
			return "", errors.New("git upload-pack: remote error: bad object")
		}
		return "", nil
	}}}
	_, err := r.Resolve(context.Background(), "u", shaA)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got %v", err)
	}
}

func TestResolveFullSHADrift(t *testing.T) {
	r := &Resolver{Runner: fakeRunner{fn: func(args []string) (string, error) {
		if args[0] == "rev-parse" {
			return shaB + "\n", nil // FETCH_HEAD != requested
		}
		return "", nil
	}}}
	_, err := r.Resolve(context.Background(), "u", shaA)
	if !errors.Is(err, ErrBadResolved) {
		t.Fatalf("expected ErrBadResolved on FETCH_HEAD drift, got %v", err)
	}
}

func TestIsFullSHA(t *testing.T) {
	if !IsFullSHA(shaA) || !IsFullSHA(strings.Repeat("f", 64)) {
		t.Fatal("valid SHAs must be recognized")
	}
	if IsFullSHA("abc1234") || IsFullSHA("main") || IsFullSHA(strings.Repeat("g", 40)) {
		t.Fatal("invalid inputs must not be recognized")
	}
}
