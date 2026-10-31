package runner

import (
	"context"
	"sync"
)

// FakeRunner is an in-memory Runner for unit tests (httpapi, govern).
type FakeRunner struct {
	mu     sync.Mutex
	Jobs   map[string]*Status
	Active map[string]int
	// CreateErr, when set, is returned by Create.
	CreateErr error
}

// NewFakeRunner builds an empty fake.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{Jobs: map[string]*Status{}, Active: map[string]int{}}
}

// Create records a queued build.
func (f *FakeRunner) Create(_ context.Context, spec Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return f.CreateErr
	}
	f.Jobs[spec.ID] = &Status{
		ID:          spec.ID,
		Principal:   spec.Principal,
		Repo:        spec.Repo,
		Revision:    spec.Revision,
		Destination: spec.Destination,
		State:       StateQueued,
	}
	f.Active[spec.Principal]++
	return nil
}

// SetState overrides a build's state (test helper).
func (f *FakeRunner) SetState(id, state, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.Jobs[id]; ok {
		s.State = state
		s.Digest = digest
		if state == StateSucceeded || state == StateFailed {
			if f.Active[s.Principal] > 0 {
				f.Active[s.Principal]--
			}
		}
	}
}

// Get returns the recorded status.
func (f *FakeRunner) Get(_ context.Context, id string) (*Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.Jobs[id]
	if !ok {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

// List returns the principal's builds.
func (f *FakeRunner) List(_ context.Context, principal string) ([]Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Status
	for _, s := range f.Jobs {
		if s.Principal == principal {
			out = append(out, *s)
		}
	}
	return out, nil
}

// CountActive returns the fake active count.
func (f *FakeRunner) CountActive(_ context.Context, principal string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Active[principal], nil
}
