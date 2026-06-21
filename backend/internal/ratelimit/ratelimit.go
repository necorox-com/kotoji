// Package ratelimit provides a keyed token-bucket limiter (golang.org/x/time/rate)
// with idle eviction, plus net/http middleware that keys requests per session (or
// per client IP) on the control plane and per IP on the data plane
// (architecture.md §8.4.5). It is the hardening layer that stops an AI client or a
// scripted upload from hammering the API, and a single visitor from flooding the
// data plane.
//
// DI / testability: the limiter takes an injected clock so the token-refill math
// is deterministic in tests, and the middleware takes a key function so the
// keying strategy (session vs IP) is a parameter, not hardcoded.
package ratelimit

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Config tunes a Limiter.
type Config struct {
	// RPS is the sustained per-key request rate (tokens refilled per second).
	// <= 0 disables limiting (Allow always true) — used when an operator sets the
	// rate to 0 to turn the limiter off.
	RPS float64
	// Burst is the bucket capacity (max instantaneous requests). Defaults to
	// max(1, ceil(RPS)) when <= 0 so a single key can always make one request.
	Burst int
	// TTL is how long an idle key's bucket is retained before eviction. Defaults
	// to 10m. Eviction bounds memory under a churn of distinct keys (e.g. IPs).
	TTL time.Duration
	// Now is the injected clock (tests). Defaults to time.Now.
	Now func() time.Time
}

const (
	defaultTTL = 10 * time.Minute
)

// entry is one key's bucket plus its last-seen time for eviction.
type entry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// Limiter is a concurrency-safe, idle-evicting, keyed token-bucket limiter.
type Limiter struct {
	mu       sync.Mutex
	buckets  map[string]*entry
	rps      rate.Limit
	burst    int
	ttl      time.Duration
	now      func() time.Time
	disabled bool
	// lastSweep throttles the inline eviction sweep so Allow stays O(1) amortized.
	lastSweep time.Time
}

// New builds a Limiter from cfg. Defaults are applied for unset fields.
func New(cfg Config) *Limiter {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	disabled := cfg.RPS <= 0
	burst := cfg.Burst
	if burst <= 0 {
		// At least 1 so a key can always make a single request when enabled.
		burst = int(cfg.RPS)
		if float64(burst) < cfg.RPS {
			burst++ // ceil
		}
		if burst < 1 {
			burst = 1
		}
	}
	return &Limiter{
		buckets:   make(map[string]*entry),
		rps:       rate.Limit(cfg.RPS),
		burst:     burst,
		ttl:       ttl,
		now:       now,
		disabled:  disabled,
		lastSweep: now(),
	}
}

// Allow reports whether the request for key may proceed (consuming one token). A
// disabled limiter (RPS <= 0) always allows. It lazily creates a key's bucket and
// opportunistically evicts idle buckets so memory stays bounded.
func (l *Limiter) Allow(key string) bool {
	if l.disabled {
		return true
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	// Opportunistic eviction sweep, at most once per TTL window, so a long-lived
	// process with many transient keys does not leak buckets.
	if now.Sub(l.lastSweep) >= l.ttl {
		l.evictLocked(now)
		l.lastSweep = now
	}

	e, ok := l.buckets[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[key] = e
	}
	e.lastSeen = now
	// AllowN with the injected clock keeps refill math deterministic in tests.
	return e.lim.AllowN(now, 1)
}

// evictLocked removes buckets idle longer than ttl. The caller holds l.mu.
func (l *Limiter) evictLocked(now time.Time) {
	for k, e := range l.buckets {
		if now.Sub(e.lastSeen) >= l.ttl {
			delete(l.buckets, k)
		}
	}
}

// Len returns the number of live buckets (for tests/metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// KeyFunc derives the limiter key from a request. Returning "" makes the
// middleware skip limiting for that request (e.g. an unauthenticated request on a
// session-keyed limiter falls through to an IP-keyed wrapper).
type KeyFunc func(r *http.Request) string

// Denied is called when a request is rate-limited so the caller controls the
// response shape (the API writes its JSON error envelope; the data plane writes a
// plain 429). It must write the full response.
type Denied func(w http.ResponseWriter, r *http.Request)

// Middleware returns net/http middleware that limits per key. A request whose key
// is "" is allowed through unconditionally (the keyer opted it out). When the
// bucket is empty, onDenied writes the response and the chain stops.
func Middleware(l *Limiter, key KeyFunc, onDenied Denied) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := key(r)
			if k == "" || l.Allow(k) {
				next.ServeHTTP(w, r)
				return
			}
			onDenied(w, r)
		})
	}
}

// ClientIP extracts the best-effort client IP from a request: the first
// X-Forwarded-For hop when proxy headers are trusted, else the RemoteAddr host.
// It is the data-plane keyer and the control-plane fallback (anonymous requests).
func ClientIP(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The left-most token is the original client (NPM appends hops).
			if i := indexComma(xff); i >= 0 {
				return trimSpace(xff[:i])
			}
			return trimSpace(xff)
		}
		if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
			return trimSpace(xrip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // RemoteAddr without a port (rare); use as-is
	}
	return host
}

// indexComma returns the index of the first comma, or -1.
func indexComma(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			return i
		}
	}
	return -1
}

// trimSpace trims ASCII spaces/tabs without importing strings (tiny dependency
// hygiene for a hot-path helper).
func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
