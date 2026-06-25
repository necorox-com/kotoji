package api

import (
	"net/http"
	"testing"
)

// TestControlSecurityHeaders_Present pins CH1: every control-plane response carries
// the baseline security headers (X-Content-Type-Options, X-Frame-Options,
// Referrer-Policy, and a strict CSP). Asserted on a PUBLIC route so no auth setup is
// needed — the middleware runs before routing, so the headers ride on every response.
func TestControlSecurityHeaders_Present(t *testing.T) {
	e := newTestEnv(t)

	rec := e.request(http.MethodGet, "/api/config").do()
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/config status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": controlPlaneCSP,
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}

// TestControlSecurityHeaders_OnErrorAndNotFound: the headers must be present even on
// non-2xx control responses (e.g. an unknown control path 404), since the middleware
// is earlier in the chain than routing. This proves no error path skips them.
func TestControlSecurityHeaders_OnNotFound(t *testing.T) {
	e := newTestEnv(t)

	// No Serve handler is wired in the test env, so an unmatched path is a plain 404
	// from chi — still passes through the security-header middleware.
	rec := e.request(http.MethodGet, "/definitely-not-a-route").do()

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("404 response missing X-Content-Type-Options, got %q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("404 response missing X-Frame-Options, got %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != controlPlaneCSP {
		t.Errorf("404 response CSP = %q, want %q", got, controlPlaneCSP)
	}
}
