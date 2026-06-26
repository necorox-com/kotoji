package serve

import (
	"net/http"
	"strings"
)

// SecurityHeaderConfig lets self-hosters tune the data-plane security headers
// without code changes (routing-and-serving.md §6.1). The zero value is NOT
// usable directly; use DefaultSecurityHeaderConfig and override fields.
type SecurityHeaderConfig struct {
	// CSP is the full Content-Security-Policy string. If empty, it is built from
	// ConnectSrc + TopLevelSandbox by buildCSP at construction time.
	CSP string
	// ConnectSrc is the connect-src directive value. Default "*" (the locked
	// "in-page fetch OK" feature); tighten to an allowlist if desired.
	ConnectSrc string
	// TopLevelSandbox, when true, adds the sandbox directive (re-granting
	// allow-scripts/forms/popups/modals/downloads/same-origin). Default false: the
	// real isolation is per-subdomain origin, and sandbox can break downloads/popups.
	TopLevelSandbox bool
	// ReferrerPolicy default "strict-origin-when-cross-origin".
	ReferrerPolicy string
	// PermissionsPolicy default locks geolocation/mic/camera/payment/usb + opts out
	// of FLoC.
	PermissionsPolicy string
	// FrameAncestors is the CSP frame-ancestors directive value for served content.
	// Default "'self'" PLUS the control origin (see FrameAncestorsControlOrigin):
	// the dashboard renders a PREVIEW iframe of a hosted site (the control origin
	// embeds a sibling subdomain origin), so served content MUST permit framing by
	// the control origin and by itself — but nothing else. A blanket 'none' / an
	// X-Frame-Options: DENY would break that preview iframe, so served content
	// deliberately does NOT emit X-Frame-Options at all; framing is governed solely
	// by this directive. On a genuinely separate usercontent domain an operator can
	// tighten this to "'self'" (drop the control origin) if no preview is needed.
	FrameAncestors string
	// FrameAncestorsControlOrigin, when non-empty, is appended to the default
	// frame-ancestors list so the control-origin dashboard can embed the preview
	// iframe. It is ignored when FrameAncestors is set explicitly. Single-domain
	// default: the control base URL origin (e.g. "https://hosting.example.com").
	FrameAncestorsControlOrigin string
	// CrossOriginResourcePolicy is the Cross-Origin-Resource-Policy value for served
	// content. Default "same-site" (NOT "same-origin"): the published site, its own
	// same-origin assets, AND same-site embedders (the dashboard preview iframe lives
	// on the control origin, a sibling subdomain of the SAME registrable site) can
	// read the bytes, while a truly cross-site malicious embedder cannot no-cors-read
	// them. An operator on a separate usercontent domain may tighten this to
	// "same-origin".
	CrossOriginResourcePolicy string
	// ExtraHeaders is an escape hatch applied last (overrides nothing it does not name).
	ExtraHeaders map[string]string
}

// Defaults for the security header config (routing-and-serving.md §6.1).
const (
	defaultConnectSrc        = "*"
	defaultReferrerPolicy    = "strict-origin-when-cross-origin"
	defaultPermissionsPolicy = "geolocation=(), microphone=(), camera=(), payment=(), usb=(), interest-cohort=()"
	// defaultFrameAncestors is the single-domain-safe base for served content: a
	// hosted page may only be framed by ITSELF (same origin). The control origin is
	// added on top of this at construction time when known (see normalize), so the
	// dashboard PREVIEW iframe — control origin embedding a sibling subdomain — works.
	// Deliberately NOT 'none': a 'none'/X-Frame-Options DENY would break that preview.
	defaultFrameAncestors = "'self'"
	// defaultCrossOriginResourcePolicy is "same-site" (NOT "same-origin"): same-site
	// embedders such as the control-origin dashboard preview iframe (a sibling
	// subdomain of the SAME registrable site) may read the served bytes, while a
	// truly cross-site malicious embedder cannot no-cors-read them.
	defaultCrossOriginResourcePolicy = "same-site"
	// serverBanner replaces any upstream Server detail (routing-and-serving.md §6.2).
	serverBanner = "kotoji"
	// svgCSP neutralizes inline <script> in an SVG served as a top-level document
	// (routing-and-serving.md §6.1 SVG note / Q4: recommended).
	svgCSP = "script-src 'none'"
)

// DefaultSecurityHeaderConfig returns the locked v1 policy from routing-and-serving.md
// §6.1: permissive toward the hosted app's own code, strict toward cross-origin,
// sandbox OFF (per-origin isolation is the real control).
func DefaultSecurityHeaderConfig() SecurityHeaderConfig {
	c := SecurityHeaderConfig{
		ConnectSrc:                defaultConnectSrc,
		TopLevelSandbox:           false,
		ReferrerPolicy:            defaultReferrerPolicy,
		PermissionsPolicy:         defaultPermissionsPolicy,
		FrameAncestors:            defaultFrameAncestors,
		CrossOriginResourcePolicy: defaultCrossOriginResourcePolicy,
	}
	c.CSP = buildCSP(c)
	return c
}

// DefaultSecurityHeaderConfigForControl returns the locked default served-content
// policy with the control origin allowed to FRAME served content (the dashboard
// PREVIEW iframe embeds a sibling subdomain). controlOrigin must be an ORIGIN
// (scheme://host[:port]); pass "" to get the bare default ('self'-only framing).
// This is the single-binary deployment's served config (see app.servedSecurityConfig).
func DefaultSecurityHeaderConfigForControl(controlOrigin string) SecurityHeaderConfig {
	c := SecurityHeaderConfig{
		ConnectSrc:                  defaultConnectSrc,
		TopLevelSandbox:             false,
		ReferrerPolicy:              defaultReferrerPolicy,
		PermissionsPolicy:           defaultPermissionsPolicy,
		FrameAncestorsControlOrigin: controlOrigin,
		CrossOriginResourcePolicy:   defaultCrossOriginResourcePolicy,
	}
	// Leave FrameAncestors empty so normalize folds in the control origin, then build
	// the CSP from the resolved directive.
	c = c.normalize()
	return c
}

// normalize fills any unset field with its default and (re)builds CSP if empty.
// Called once at handler construction so the hot path never re-derives strings.
func (c SecurityHeaderConfig) normalize() SecurityHeaderConfig {
	if c.ConnectSrc == "" {
		c.ConnectSrc = defaultConnectSrc
	}
	if c.ReferrerPolicy == "" {
		c.ReferrerPolicy = defaultReferrerPolicy
	}
	if c.PermissionsPolicy == "" {
		c.PermissionsPolicy = defaultPermissionsPolicy
	}
	// Resolve the frame-ancestors directive: explicit value wins; otherwise build the
	// single-domain-safe default ('self' [+ control origin]) so the dashboard preview
	// iframe (control origin embedding a sibling subdomain) is permitted.
	if c.FrameAncestors == "" {
		c.FrameAncestors = defaultFrameAncestors
		// Append the control origin so the dashboard can embed the preview iframe.
		// Only when known (single-binary deployments thread it in); a bare 'self'
		// default still serves the published site itself fine when it is unset.
		if c.FrameAncestorsControlOrigin != "" {
			c.FrameAncestors = defaultFrameAncestors + " " + c.FrameAncestorsControlOrigin
		}
	}
	if c.CrossOriginResourcePolicy == "" {
		c.CrossOriginResourcePolicy = defaultCrossOriginResourcePolicy
	}
	if c.CSP == "" {
		c.CSP = buildCSP(c)
	}
	return c
}

// buildCSP assembles the Content-Security-Policy from its parts. The directives
// and their exact values are FROZEN by routing-and-serving.md §6.1: the policy is
// permissive toward the hosted app's own JS ('unsafe-inline'/'unsafe-eval' on
// script-src — there is no build step to inject nonces) but strict toward
// cross-origin (object-src/base-uri/form-action/frame-ancestors). The sandbox
// directive is appended only when TopLevelSandbox is set.
func buildCSP(c SecurityHeaderConfig) string {
	connect := c.ConnectSrc
	if connect == "" {
		connect = defaultConnectSrc
	}
	// frame-ancestors is config-aware: the served default is 'self' [+ control origin]
	// (NOT 'none'), so the dashboard PREVIEW iframe — control origin embedding a
	// sibling subdomain — keeps working. A 'none' here would break that preview.
	frameAncestors := c.FrameAncestors
	if frameAncestors == "" {
		frameAncestors = defaultFrameAncestors
		if c.FrameAncestorsControlOrigin != "" {
			frameAncestors = defaultFrameAncestors + " " + c.FrameAncestorsControlOrigin
		}
	}
	directives := []string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline' 'unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src " + connect,
		"media-src 'self' data: blob:",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors " + frameAncestors,
	}
	if c.TopLevelSandbox {
		directives = append(directives,
			"sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads allow-same-origin")
	}
	return strings.Join(directives, "; ")
}

// applySecurityHeaders writes the full data-plane security header set onto w for
// EVERY response (200, 301, 404) per routing-and-serving.md §6.1/§6.2. isSVG
// swaps in the stricter per-file CSP that neutralizes inline SVG script.
//
// It must be called BEFORE WriteHeader (Go flushes headers on the first write).
func (c SecurityHeaderConfig) applySecurityHeaders(w http.ResponseWriter, isSVG bool) {
	h := w.Header()
	csp := c.CSP
	if isSVG {
		csp = svgCSP
	}
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", c.ReferrerPolicy)
	h.Set("Permissions-Policy", c.PermissionsPolicy)
	// NO X-Frame-Options on served content: a DENY/SAMEORIGIN here would break the
	// dashboard PREVIEW iframe (control origin embedding a sibling subdomain). Framing
	// is governed by the CSP frame-ancestors directive (config-aware: 'self' + control
	// origin), which lets the preview through while still blocking foreign embedders.
	// COOP same-origin is safe for served content: it isolates the window's opener/
	// popup relationships (so a hosted page cannot get a handle to a control window),
	// and does NOT affect being embedded as an iframe — the preview is unaffected.
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	// CORP is config-aware, default "same-site" (NOT "same-origin"): same-site
	// embedders such as the control-origin dashboard preview iframe (sibling subdomain
	// of the SAME registrable site) may read the bytes, while a truly cross-site
	// malicious embedder cannot no-cors-read them.
	corp := c.CrossOriginResourcePolicy
	if corp == "" {
		corp = defaultCrossOriginResourcePolicy
	}
	h.Set("Cross-Origin-Resource-Policy", corp)
	// Replace any upstream Server banner detail (§6.2).
	h.Set("Server", serverBanner)
	// Escape hatch, applied last so operators can add/override extras.
	for k, v := range c.ExtraHeaders {
		h.Set(k, v)
	}
}
