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
		// Cross-origin isolation (CH1, single-domain hardening): a hosted site sharing
		// the registrable domain must not be able to grab a window handle to a control
		// window (COOP) or no-cors-read control JSON/assets (CORP).
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}

// TestControlSecurityHeaders_CrossOriginIsolation pins the COOP/CORP isolation pair
// explicitly (CH1). The dashboard SPA is SAME-ORIGIN to the control API, so neither
// value affects it — they only cut cross-origin handles a hosted site could grab.
func TestControlSecurityHeaders_CrossOriginIsolation(t *testing.T) {
	e := newTestEnv(t)

	rec := e.request(http.MethodGet, "/api/config").do()
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/config status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
		t.Errorf("COOP = %q, want same-origin", got)
	}
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Errorf("CORP = %q, want same-origin", got)
	}
	// The control plane must NEVER weaken to same-site (it does not need to be embedded
	// or read by any sibling subdomain — only the SERVED plane does, for the preview).
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got == "same-site" {
		t.Errorf("control CORP must be same-origin, not same-site")
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
