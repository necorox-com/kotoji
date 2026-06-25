package auth

import (
	"sync"
	"time"
)

// R2 (login brute-force lockout). Password login does an unconditional bcrypt
// compare per attempt, so without a lockout an attacker can both brute-force the
// password AND burn CPU (bcrypt is deliberately slow) — a credential-stuffing +
// DoS combo. The R1 key fix removed the XFF-rotation escape, so a per-source
// counter now actually bounds an attacker to one bucket. This file adds an
// in-memory failure tracker that, after N consecutive failures from a source key,
// locks that key out for an exponentially-growing window BEFORE the bcrypt compare
// runs (so a locked-out attacker cannot even spend our CPU).
//
// Scope (deliberately small): in-memory, per-process. A multi-instance deployment
// gets per-instance lockout, which is sufficient because the SAME source key hits
// the SAME instance behind a single reverse proxy (the documented topology). It is
// NOT a distributed rate limiter — that is the job of the upstream edge.

const (
	// lockoutThreshold is the number of CONSECUTIVE failures from one source key
	// before lockouts begin. The first (threshold-1) failures are free (a human
	// fat-fingering a password is not punished); the threshold-th failure starts
	// the backoff. 5 is the conventional balance (OWASP) between usability and
	// brute-force resistance.
	lockoutThreshold = 5

	// lockoutBaseDelay is the first lockout window once the threshold is crossed.
	// Each further failure doubles it (exponential backoff) up to lockoutMaxDelay.
	lockoutBaseDelay = 1 * time.Second

	// lockoutMaxDelay caps the backoff so a key is never permanently bricked (a
	// legitimate admin who later types the right password recovers within this
	// window). 15m makes sustained brute force economically pointless while a real
	// admin only ever waits minutes in the worst case.
	lockoutMaxDelay = 15 * time.Minute

	// lockoutEntryTTL is how long an idle entry is retained before eviction. It
	// bounds memory under a flood of distinct source keys (an attacker rotating
	// IPs). Once an entry is older than this with no recent activity it is dropped,
	// which also naturally resets the counter for a long-dormant key.
	lockoutEntryTTL = 30 * time.Minute
)

// lockoutEntry tracks one source key's consecutive-failure state.
type lockoutEntry struct {
	failures    int       // consecutive failures since the last success/reset
	lockedUntil time.Time // wall-clock instant the key is unlocked again (zero = not locked)
	lastSeen    time.Time // for idle eviction
}

// loginLockout is a concurrency-safe, idle-evicting, per-source-key failure
// tracker with exponential backoff. DI/testability: it takes an injected clock so
// the backoff math is deterministic in tests (mirrors ratelimit.Limiter).
type loginLockout struct {
	mu      sync.Mutex
	entries map[string]*lockoutEntry
	now     func() time.Time
	// lastSweep throttles the inline eviction sweep so the hot path stays O(1).
	lastSweep time.Time

	threshold int
	base      time.Duration
	max       time.Duration
	ttl       time.Duration
}

// newLoginLockout builds a lockout tracker with the package defaults. now may be
// nil (defaults to time.Now); tests inject a fake clock.
func newLoginLockout(now func() time.Time) *loginLockout {
	if now == nil {
		now = time.Now
	}
	return &loginLockout{
		entries:   make(map[string]*lockoutEntry),
		now:       now,
		lastSweep: now(),
		threshold: lockoutThreshold,
		base:      lockoutBaseDelay,
		max:       lockoutMaxDelay,
		ttl:       lockoutEntryTTL,
	}
}

// setClock swaps the injected clock. Tests use it (via the Auth seam) to make the
// exponential-backoff windows deterministic.
func (l *loginLockout) setClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
	l.lastSweep = now()
}

// locked reports whether key is currently locked out and, if so, how long until
// it unlocks. A locked key is rejected BEFORE the expensive bcrypt compare so a
// brute-force attempt cannot consume CPU. An empty key (no source signal) is
// never locked — failing open here is correct because the lockout is a hardening
// layer, and the R1-fixed limiter still bounds an unkeyable flood.
func (l *loginLockout) locked(key string) (bool, time.Duration) {
	if key == "" {
		return false, 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeSweepLocked(now)

	e, ok := l.entries[key]
	if !ok {
		return false, 0
	}
	e.lastSeen = now
	if now.Before(e.lockedUntil) {
		return true, e.lockedUntil.Sub(now)
	}
	return false, 0
}

// recordFailure increments the consecutive-failure count for key and, once it
// reaches the threshold, (re)arms an exponentially-growing lockout window. It
// returns the new lockout duration (0 while below the threshold). Called AFTER a
// confirmed bad-credential result so only real failures count.
func (l *loginLockout) recordFailure(key string) time.Duration {
	if key == "" {
		return 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeSweepLocked(now)

	e, ok := l.entries[key]
	if !ok {
		e = &lockoutEntry{}
		l.entries[key] = e
	}
	e.failures++
	e.lastSeen = now

	if e.failures < l.threshold {
		return 0
	}
	// Exponential backoff: base * 2^(failures-threshold), capped at max. Using the
	// overflow-safe shift (cap the exponent) avoids a duration overflow on a long
	// sustained attack.
	exp := e.failures - l.threshold
	delay := l.base
	for i := 0; i < exp && delay < l.max; i++ {
		delay *= 2
	}
	if delay > l.max {
		delay = l.max
	}
	e.lockedUntil = now.Add(delay)
	return delay
}

// recordSuccess clears any failure state for key (a correct credential resets the
// counter so a legitimate admin is never progressively penalized after recovering).
func (l *loginLockout) recordSuccess(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// maybeSweepLocked evicts idle entries at most once per TTL window. Caller holds mu.
func (l *loginLockout) maybeSweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < l.ttl {
		return
	}
	for k, e := range l.entries {
		// Evict entries idle past the TTL whose lockout (if any) has also elapsed, so
		// an actively-locked key is never dropped early (which would reset its backoff).
		if now.Sub(e.lastSeen) >= l.ttl && !now.Before(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
	l.lastSweep = now
}
