package auth

import (
	"context"
	"sync"
	"time"
)

// Server-side single-use guard for the OIDC login handshake. The deep-dive flagged
// that the signed login-state cookie binds state+nonce+PKCE but is NOT server-side
// single-use: a captured login-state cookie could be REPLAYED within loginStateTTL
// (the OAuth code itself is single-use at the IdP, but the state/nonce binding is
// replayable). This file closes that by recording each login state ONCE on the
// server so a second callback carrying the same state is rejected.
//
// MULTI-NODE: the default implementation here is in-memory and per-process. A
// single-node (or single-reverse-proxy-sticky) deployment is fully covered. A
// genuine multi-node deployment that load-balances callbacks across processes
// should supply a shared SingleUseStore (DB/Redis-backed) via the Auth constructor
// so the "first use" decision is atomic ACROSS nodes. The seam below
// (SingleUseStore) is deliberately a one-method interface so such an impl can be
// dropped in without touching the handlers.

const (
	// singleUseMaxEntries bounds the in-memory store so a flood of distinct states
	// (an attacker firing LoginStart repeatedly) cannot grow the map without limit.
	// Each entry is tiny (a string key + an expiry), and entries self-expire after
	// the consume TTL (loginStateTTL), so this cap is only ever hit under abuse; when
	// it is hit we proactively sweep expired entries before recording a new one.
	singleUseMaxEntries = 100_000

	// singleUseSweepInterval throttles the periodic eviction sweep so the hot path
	// (Consume) stays effectively O(1): we only walk the map at most once per window.
	singleUseSweepInterval = 1 * time.Minute
)

// SingleUseStore is the DI seam for the server-side single-use guard. Consume
// atomically records key and returns firstUse=true the FIRST time it is seen within
// ttl, and false on any subsequent (replayed) call within that ttl. After ttl the
// key is forgotten, so a later (legitimately fresh) login reusing the SAME key — in
// practice impossible since the state is 256-bit random per login — would again be
// treated as first use. The err return lets a remote/backed implementation surface
// a store failure (the in-memory default never errors).
//
// SWAPPABLE: a multi-node deployment can supply a DB/Redis-backed implementation so
// the first-use decision is atomic across processes (see the file header note).
type SingleUseStore interface {
	Consume(ctx context.Context, key string, ttl time.Duration) (firstUse bool, err error)
}

// memSingleUseStore is the in-memory, mutex-guarded default SingleUseStore: a
// map[key]expiry with lazy/periodic eviction of expired entries and a hard cap so
// it cannot grow unboundedly. It is per-process (see the multi-node note in the
// file header). DI/testability: it takes an injected clock so the expiry/cleanup
// math is deterministic in tests (mirrors loginLockout / ratelimit.Limiter).
type memSingleUseStore struct {
	mu      sync.Mutex
	entries map[string]time.Time // key -> expiry instant
	now     func() time.Time
	// lastSweep throttles the periodic eviction sweep so Consume stays O(1) amortized.
	lastSweep time.Time

	maxEntries    int
	sweepInterval time.Duration
}

// newMemSingleUseStore builds an in-memory single-use store with the package
// defaults. now may be nil (defaults to time.Now); tests inject a fake clock.
func newMemSingleUseStore(now func() time.Time) *memSingleUseStore {
	if now == nil {
		now = time.Now
	}
	return &memSingleUseStore{
		entries:       make(map[string]time.Time),
		now:           now,
		lastSweep:     now(),
		maxEntries:    singleUseMaxEntries,
		sweepInterval: singleUseSweepInterval,
	}
}

// setClock swaps the injected clock. Tests use it to make the TTL/expiry windows
// deterministic.
func (s *memSingleUseStore) setClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
	s.lastSweep = now()
}

// Consume records key with an expiry of now+ttl and returns firstUse=true only on
// the FIRST call within the live window; any replay while the entry is still live
// returns false. An expired (or never-seen) key is treated as first use and (re)armed.
// The in-memory implementation never returns a non-nil error.
func (s *memSingleUseStore) Consume(_ context.Context, key string, ttl time.Duration) (bool, error) {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeSweepLocked(now)

	// A live (unexpired) entry means this key was already consumed -> replay.
	if exp, ok := s.entries[key]; ok && now.Before(exp) {
		return false, nil
	}

	// Bound the map: if we are at the cap and could not free space by the periodic
	// sweep above, force a full eviction of expired entries now. If the map is STILL
	// full (every entry live — only possible under an extreme flood within a single
	// ttl), reject as non-first-use rather than growing unboundedly. A rejected
	// LOGIN simply fails closed (the user retries); it never weakens the guard.
	if len(s.entries) >= s.maxEntries {
		s.evictExpiredLocked(now)
		if len(s.entries) >= s.maxEntries {
			return false, nil
		}
	}

	// First use within the window: record the expiry and report firstUse.
	s.entries[key] = now.Add(ttl)
	return true, nil
}

// maybeSweepLocked evicts expired entries at most once per sweepInterval. Caller
// holds mu. This keeps the common Consume path off the full O(n) walk.
func (s *memSingleUseStore) maybeSweepLocked(now time.Time) {
	if now.Sub(s.lastSweep) < s.sweepInterval {
		return
	}
	s.evictExpiredLocked(now)
	s.lastSweep = now
}

// evictExpiredLocked drops every entry whose expiry has passed. Caller holds mu.
func (s *memSingleUseStore) evictExpiredLocked(now time.Time) {
	for k, exp := range s.entries {
		if !now.Before(exp) { // now >= exp => expired
			delete(s.entries, k)
		}
	}
}

// size reports the current entry count (test-only assertion helper for the
// expiry/cleanup behavior).
func (s *memSingleUseStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
