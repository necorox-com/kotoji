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
	// ExtraHeaders is an escape hatch applied last (overrides nothing it does not name).
	ExtraHeaders map[string]string
}

// Defaults for the security header config (routing-and-serving.md §6.1).
const (
	defaultConnectSrc        = "*"
	defaultReferrerPolicy    = "strict-origin-when-cross-origin"
	defaultPermissionsPolicy = "geolocation=(), microphone=(), camera=(), payment=(), usb=(), interest-cohort=()"
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
		ConnectSrc:        defaultConnectSrc,
		TopLevelSandbox:   false,
		ReferrerPolicy:    defaultReferrerPolicy,
		PermissionsPolicy: defaultPermissionsPolicy,
	}
	c.CSP = buildCSP(c)
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
		"frame-ancestors 'none'",
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
	h.Set("X-Frame-Options", "DENY") // legacy mirror of frame-ancestors 'none'
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	// Replace any upstream Server banner detail (§6.2).
	h.Set("Server", serverBanner)
	// Escape hatch, applied last so operators can add/override extras.
	for k, v := range c.ExtraHeaders {
		h.Set(k, v)
	}
}
