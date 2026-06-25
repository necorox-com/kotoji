package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeClock is an injectable, advanceable clock for deterministic refill math.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestLimiter_Allow_Table(t *testing.T) {
	tests := []struct {
		name      string
		rps       float64
		burst     int
		calls     int
		wantAllow int // how many of `calls` should be allowed with no time advance
	}{
		{name: "burst caps instantaneous calls", rps: 1, burst: 3, calls: 5, wantAllow: 3},
		{name: "burst of 1", rps: 1, burst: 1, calls: 4, wantAllow: 1},
		{name: "default burst ceils rps", rps: 2.5, burst: 0, calls: 10, wantAllow: 3}, // ceil(2.5)=3
		{name: "disabled allows everything", rps: 0, burst: 0, calls: 100, wantAllow: 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
			l := New(Config{RPS: tc.rps, Burst: tc.burst, Now: clk.now})
			allowed := 0
			for i := 0; i < tc.calls; i++ {
				if l.Allow("k") {
					allowed++
				}
			}
			if allowed != tc.wantAllow {
				t.Fatalf("allowed=%d want=%d", allowed, tc.wantAllow)
			}
		})
	}
}

func TestLimiter_RefillsOverTime(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := New(Config{RPS: 1, Burst: 1, Now: clk.now})

	if !l.Allow("k") {
		t.Fatal("first call should be allowed")
	}
	if l.Allow("k") {
		t.Fatal("second immediate call should be denied (bucket empty)")
	}
	// Advance one second: exactly one token refilled.
	clk.advance(time.Second)
	if !l.Allow("k") {
		t.Fatal("call after 1s should be allowed (token refilled)")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := New(Config{RPS: 1, Burst: 1, Now: clk.now})
	if !l.Allow("a") {
		t.Fatal("a first call allowed")
	}
	if !l.Allow("b") {
		t.Fatal("b must have its own bucket")
	}
	if l.Allow("a") {
		t.Fatal("a second call denied")
	}
}

func TestLimiter_EvictsIdleKeys(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := New(Config{RPS: 1, Burst: 1, TTL: time.Minute, Now: clk.now})
	l.Allow("a")
	if got := l.Len(); got != 1 {
		t.Fatalf("len after first key = %d want 1", got)
	}
	// Advance past TTL and touch a different key: the idle "a" bucket is swept.
	clk.advance(2 * time.Minute)
	l.Allow("b")
	if got := l.Len(); got != 1 {
		t.Fatalf("len after eviction = %d want 1 (only b)", got)
	}
}

func TestMiddleware_EmptyKeySkips(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := New(Config{RPS: 1, Burst: 1, Now: clk.now})
	var served int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++; w.WriteHeader(200) })
	mw := Middleware(l, func(*http.Request) string { return "" }, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h := mw(next)
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != 200 {
			t.Fatalf("empty key must skip limiting; got %d", rec.Code)
		}
	}
	if served != 5 {
		t.Fatalf("served=%d want 5", served)
	}
}

func TestMiddleware_DeniesWhenExhausted(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := New(Config{RPS: 1, Burst: 1, Now: clk.now})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mw := Middleware(l, func(*http.Request) string { return "fixed" }, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h := mw(next)

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec1.Code != 200 {
		t.Fatalf("first request code=%d want 200", rec1.Code)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request code=%d want 429", rec2.Code)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name   string
		trust  bool
		xff    string
		xrip   string
		remote string
		want   string
	}{
		// R1: with a single trusted proxy the RIGHT-most hop (the address the proxy
		// appended) is the real client — NOT the left-most token the client controls.
		{name: "xff rightmost hop trusted", trust: true, xff: "1.2.3.4, 5.6.7.8", remote: "10.0.0.1:9999", want: "5.6.7.8"},
		{name: "xff single hop trusted", trust: true, xff: "1.2.3.4", remote: "10.0.0.1:9999", want: "1.2.3.4"},
		{name: "xff ignored when untrusted", trust: false, xff: "1.2.3.4", remote: "10.0.0.1:9999", want: "10.0.0.1"},
		{name: "x-real-ip fallback", trust: true, xrip: "9.9.9.9", remote: "10.0.0.1:9999", want: "9.9.9.9"},
		{name: "remoteaddr host", trust: true, remote: "203.0.113.7:443", want: "203.0.113.7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xrip != "" {
				r.Header.Set("X-Real-IP", tc.xrip)
			}
			if got := ClientIP(r, tc.trust); got != tc.want {
				t.Fatalf("ClientIP = %q want %q", got, tc.want)
			}
		})
	}
}

// TestClientIP_SpoofedLeftMostDoesNotChangeKey is the core R1 regression: an
// attacker who prepends arbitrary spoofed tokens to X-Forwarded-For must NOT be
// able to change the limiter key. Behind a single trusted proxy the key is fixed
// to the right-most (proxy-appended) hop regardless of how many tokens the client
// prepends.
func TestClientIP_SpoofedLeftMostDoesNotChangeKey(t *testing.T) {
	const trustedProxyHop = "5.6.7.8" // what the trusted proxy actually appended

	// Three requests from the same real client, each prepending a DIFFERENT spoofed
	// left-most token in an attempt to rotate the limiter key.
	spoofs := []string{
		"9.9.9.9, " + trustedProxyHop,
		"8.8.8.8, 7.7.7.7, " + trustedProxyHop,
		"1.1.1.1, " + trustedProxyHop,
	}
	for _, xff := range spoofs {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.1:9999"
		r.Header.Set("X-Forwarded-For", xff)
		if got := ClientIP(r, true); got != trustedProxyHop {
			t.Fatalf("spoofed XFF %q changed the key: ClientIP = %q want %q", xff, got, trustedProxyHop)
		}
	}
}

// TestClientIPHops_MultipleTrustedProxies covers an operator-configured >1 trusted
// proxy chain: with 2 trusted hops the client IP is the 2nd token from the right,
// and a spoofed left-most token still cannot reach it.
func TestClientIPHops_MultipleTrustedProxies(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:9999"
	// client , proxyA(appended client) , proxyB(appended proxyA). With 2 trusted
	// proxies (A,B) the real client is the token 2-from-the-right: "2.2.2.2".
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 2.2.2.2, 3.3.3.3")
	if got := ClientIPHops(r, true, 2); got != "2.2.2.2" {
		t.Fatalf("ClientIPHops(2) = %q want %q", got, "2.2.2.2")
	}

	// Fewer tokens than trusted hops => fall back to RemoteAddr, never a client token.
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "203.0.113.7:443"
	r2.Header.Set("X-Forwarded-For", "9.9.9.9") // only 1 token, but 2 hops configured
	if got := ClientIPHops(r2, true, 2); got != "203.0.113.7" {
		t.Fatalf("short chain ClientIPHops(2) = %q want RemoteAddr host %q", got, "203.0.113.7")
	}
}
