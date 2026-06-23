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
		{name: "xff first hop trusted", trust: true, xff: "1.2.3.4, 5.6.7.8", remote: "10.0.0.1:9999", want: "1.2.3.4"},
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
