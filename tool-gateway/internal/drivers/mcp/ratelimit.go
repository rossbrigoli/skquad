package mcp

import (
	"sync"
	"time"
)

// rateLimiters is a per-key leaky-bucket limiter (identical semantics to
// the web driver's limiter; kept per-driver so driver state never
// crosses packages). Capacity equals the
// per-minute rate (one minute of burst), refilled continuously at
// rate/60 tokens per second. Keys are (agentID, resourceID) pairs so
// one noisy agent cannot exhaust another's budget.
type rateLimiters struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiters() *rateLimiters {
	return &rateLimiters{buckets: make(map[string]*bucket)}
}

// allow consumes one token for key at the given rate. now is injected
// for deterministic tests.
func (r *rateLimiters) allow(key string, ratePerMin int, now time.Time) bool {
	if ratePerMin <= 0 {
		return true
	}
	refillPerSec := float64(ratePerMin) / 60.0
	capacity := float64(ratePerMin)

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{tokens: capacity, last: now}
		r.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * refillPerSec
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}
	return false
}
