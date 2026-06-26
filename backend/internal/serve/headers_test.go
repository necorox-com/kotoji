package serve

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// TestDefaultSecurityHeaderConfig_PreviewSafe pins the single-domain-safe defaults
// that keep the dashboard PREVIEW iframe (control origin embedding a sibling
// subdomain) and hosted apps working:
//   - frame-ancestors defaults to 'self' (NOT 'none') so framing is possible at all,
//   - CORP defaults to same-site (NOT same-origin) so a same-site embedder can read
//     the served bytes,
//   - no X-Frame-Options is emitted (it would be a blanket frame-deny).
func TestDefaultSecurityHeaderConfig_PreviewSafe(t *testing.T) {
	c := DefaultSecurityHeaderConfig()

	if !strings.Contains(c.CSP, "frame-ancestors 'self'") {
		t.Fatalf("default frame-ancestors must be 'self', CSP=%q", c.CSP)
	}
	if strings.Contains(c.CSP, "frame-ancestors 'none'") {
		t.Fatalf("default CSP must not deny all framing (breaks preview): %q", c.CSP)
	}
	if c.CrossOriginResourcePolicy != "same-site" {
		t.Fatalf("default CORP = %q, want same-site", c.CrossOriginResourcePolicy)
	}

	// And the emitted headers must match: CORP same-site, NO X-Frame-Options.
	rec := httptest.NewRecorder()
	c.applySecurityHeaders(rec, false)
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-site" {
		t.Fatalf("emitted CORP = %q, want same-site", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "" {
		t.Fatalf("served content must not emit X-Frame-Options, got %q", got)
	}
	// nosniff must remain (constraint: keep nosniff).
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff missing, got %q", got)
	}
}

// TestSecurityHeaderConfig_FrameAncestorsControlOrigin verifies the control origin is
// folded into frame-ancestors so the dashboard can embed the preview iframe, while
// 'self' is always retained (the published page frames itself).
func TestSecurityHeaderConfig_FrameAncestorsControlOrigin(t *testing.T) {
	const ctrl = "https://hosting.example.com"
	c := DefaultSecurityHeaderConfigForControl(ctrl)

	if !strings.Contains(c.CSP, "frame-ancestors 'self' "+ctrl) {
		t.Fatalf("frame-ancestors must include 'self' + control origin, CSP=%q", c.CSP)
	}
	if strings.Contains(c.CSP, "frame-ancestors 'none'") {
		t.Fatalf("must not deny framing: %q", c.CSP)
	}

	// Empty control origin => bare 'self' default (fail-safe), no stray space.
	bare := DefaultSecurityHeaderConfigForControl("")
	if !strings.Contains(bare.CSP, "frame-ancestors 'self'") {
		t.Fatalf("empty control origin must yield 'self', CSP=%q", bare.CSP)
	}
	if strings.Contains(bare.CSP, "frame-ancestors 'self' ;") || strings.Contains(bare.CSP, "frame-ancestors 'self'  ") {
		t.Fatalf("empty control origin must not leave a dangling source: %q", bare.CSP)
	}
}

// TestSecurityHeaderConfig_TightenCORPToSameOrigin verifies the config-aware escape
// hatch: an operator on a genuinely separate usercontent domain can tighten served
// CORP to same-origin (no same-site embedding needed there).
func TestSecurityHeaderConfig_TightenCORPToSameOrigin(t *testing.T) {
	c := DefaultSecurityHeaderConfig()
	c.CrossOriginResourcePolicy = "same-origin"
	c = c.normalize()

	rec := httptest.NewRecorder()
	c.applySecurityHeaders(rec, false)
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Fatalf("override CORP = %q, want same-origin", got)
	}
}

// TestSecurityHeaderConfig_ExplicitFrameAncestors verifies an explicit FrameAncestors
// value wins over the control-origin default (operator full control).
func TestSecurityHeaderConfig_ExplicitFrameAncestors(t *testing.T) {
	c := SecurityHeaderConfig{
		FrameAncestors:              "'self' https://only-this.example",
		FrameAncestorsControlOrigin: "https://ignored.example", // must be ignored
	}
	c = c.normalize()
	if !strings.Contains(c.CSP, "frame-ancestors 'self' https://only-this.example") {
		t.Fatalf("explicit frame-ancestors not honored: %q", c.CSP)
	}
	if strings.Contains(c.CSP, "ignored.example") {
		t.Fatalf("control origin must be ignored when FrameAncestors is explicit: %q", c.CSP)
	}
}

// TestNewHandler_HonorsIsolationKnobs guards the NewHandler default-detection: a
// Security config that sets ONLY the isolation knobs (no CSP/ConnectSrc/etc.) must be
// normalized (its values honored), not discarded in favor of the bare default.
func TestNewHandler_HonorsIsolationKnobs(t *testing.T) {
	const ctrl = "https://dash.example.com"
	files := fstest.MapFS{"index.html": {Data: []byte("<html><head></head>ok</html>")}}
	h := NewHandler(Deps{
		Resolver: staticResolver{target: publishedTarget("s")},
		Trees:    &fakeTreeProvider{byHandle: map[string]fakeSite{"s": {fsys: files}}},
		Authz:    OpenPreviewAuthz{},
		Config: HandlerConfig{
			Security: SecurityHeaderConfig{FrameAncestorsControlOrigin: ctrl},
			Now:      func() time.Time { return fixedTime },
		},
	})

	rec := do(h, "GET", "/")

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'self' "+ctrl) {
		t.Fatalf("isolation knob discarded: frame-ancestors missing control origin, CSP=%q", csp)
	}
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-site" {
		t.Fatalf("CORP = %q, want same-site default", got)
	}
}
