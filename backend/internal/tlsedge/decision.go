// Package tlsedge implements kotoji-native, on-demand TLS termination (the third,
// opt-in deployment mode — KOTOJI_TLS_MODE=auto). It wraps CertMagic's on-demand
// issuance so that an admin who has merely pointed DNS at the server gets working
// HTTPS with NO external proxy, NO wildcard cert, NO DNS-01 token, and NO ACME
// secret in env: when a TLS handshake arrives for host H, a per-host certificate
// is issued/renewed on the fly via TLS-ALPN-01 / HTTP-01.
//
// The security-critical heart is the DecisionFunc (this file): on-demand issuance
// is GATED so kotoji only ever asks the CA for a cert for a host it is actually
// authoritative for — the effective control host, or a host the existing resolve
// layer classifies to a REAL (existing) hosted site / preview. Any other host is
// refused with NO issuance attempt, which both prevents an attacker from making
// kotoji burn ACME rate limit on arbitrary names AND avoids minting certs for
// names kotoji cannot serve.
//
// DI / testability: the gate depends only on small seams (a control-host getter,
// a resolve.Resolver, and a site-existence lookup), so it is unit-testable with
// fakes and no database. The engine constructor (engine.go) is parameterized over
// the ACME CA so the integration test can point it at a local pebble server.
package tlsedge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

// ControlHostFunc returns the EFFECTIVE control host (bare hostname, no port) the
// instance answers the control plane on. It is consulted per handshake, so it must
// be cheap; the composition root supplies the domaincfg-resolved value (static on
// the env-locked fast path). An empty return simply means "no control host yet"
// (a fresh, unconfigured instance) — the gate then falls through to the site check.
type ControlHostFunc func() string

// SiteExistsFunc reports whether handle maps to a servable site on THIS instance:
// a CURRENT live site OR a FORMER handle that 301-redirects to one (so the cert
// for the old host can be issued and the redirect served over HTTPS). It returns
// an error on a backing-store failure; the gate treats that as REFUSE (fail-closed
// — never issue on uncertainty). It MUST be bounded in latency (it runs inside the
// handshake): the production wiring uses an indexed primary-key/handle lookup.
type SiteExistsFunc func(ctx context.Context, handle string) (bool, error)

// Decider is the on-demand issuance gate. It answers ONE question — "may this
// instance obtain a certificate for host H?" — by reusing the live routing
// classifier (resolve.Resolver) plus the control-host getter and the site-exists
// lookup. It holds no mutable state and is safe for concurrent use.
type Decider struct {
	controlHost ControlHostFunc
	resolver    resolve.Resolver
	siteExists  SiteExistsFunc
}

// NewDecider builds the gate from its three seams. All are REQUIRED; a nil seam is
// a programming error the composition root must not make, so we fail loudly at
// construction rather than silently allow/deny at handshake time.
func NewDecider(controlHost ControlHostFunc, resolver resolve.Resolver, siteExists SiteExistsFunc) (*Decider, error) {
	if controlHost == nil {
		return nil, errors.New("tlsedge: NewDecider requires a non-nil ControlHostFunc")
	}
	if resolver == nil {
		return nil, errors.New("tlsedge: NewDecider requires a non-nil resolve.Resolver")
	}
	if siteExists == nil {
		return nil, errors.New("tlsedge: NewDecider requires a non-nil SiteExistsFunc")
	}
	return &Decider{controlHost: controlHost, resolver: resolver, siteExists: siteExists}, nil
}

// errRefused is the sentinel the gate returns to DENY issuance. CertMagic only
// cares that the error is non-nil; the message aids operator log triage.
var errRefused = errors.New("tlsedge: on-demand issuance refused (host not servable by this instance)")

// Allow is the DecisionFunc CertMagic invokes during a handshake for an as-yet
// uncached host. CONTRACT:
//   - name is the SNI host, already lowercased + punycode-encoded by CertMagic.
//   - return nil  => kotoji is authoritative for name; CertMagic may obtain/renew.
//   - return err  => REFUSE (no issuance attempt). Returned for: an empty name, a
//     name that is NOT the control host and does NOT classify to an EXISTING site/
//     preview, and ANY backing-store error (fail-closed).
//
// It is intentionally fast and side-effect free: at most one resolver classify
// (pure, no I/O) and at most one indexed existence lookup.
func (d *Decider) Allow(ctx context.Context, name string) error {
	host := normalizeHost(name)
	if host == "" {
		return fmt.Errorf("%w: empty host", errRefused)
	}

	// 1) The effective control host is always ours. Compared case-insensitively;
	// controlHost may be empty on a fresh instance, which simply skips this branch.
	if ch := normalizeHost(d.controlHost()); ch != "" && host == ch {
		return nil
	}

	// 2) Classify the host via the SAME resolver the data plane uses. We build a
	// minimal synthetic request carrying only the Host (no proxy headers, no path)
	// so classification keys purely on the handshake SNI — an attacker cannot smuggle
	// an X-Forwarded-Host past the gate because there are no headers on this request.
	target, err := d.resolver.Resolve(syntheticRequest(ctx, host))
	if err != nil {
		// A structural resolve failure (foreign host, bad grammar, reserved label)
		// means this is not one of our project hosts. Refuse — no issuance.
		return fmt.Errorf("%w: %v", errRefused, err)
	}
	if target.IsControl {
		// The resolver also recognizes the control host (e.g. when controlHost() is
		// empty/unconfigured but BASE_DOMAIN is the control host). Treat as ours.
		return nil
	}
	if target.Handle == "" {
		return fmt.Errorf("%w: host %q is not a project host", errRefused, host)
	}

	// 3) Existence gate: only issue for a handle that actually maps to a live site
	// or a former-handle redirect. A store error is REFUSE (fail-closed) so a
	// transient DB blip can never become an unbounded issuance vector.
	exists, err := d.siteExists(ctx, target.Handle)
	if err != nil {
		return fmt.Errorf("%w: existence check failed for %q: %v", errRefused, target.Handle, err)
	}
	if !exists {
		return fmt.Errorf("%w: no site for handle %q", errRefused, target.Handle)
	}
	return nil
}

// normalizeHost lowercases, trims, and strips a trailing :port from a host. It is
// defensive: CertMagic passes a bare SNI host, but the control-host getter may
// yield a host:port, so we normalize both ends before comparison.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return ""
	}
	// IPv6 literal in brackets: drop only the port after the closing bracket.
	if strings.HasPrefix(h, "[") {
		if end := strings.IndexByte(h, ']'); end >= 0 {
			return h[:end+1]
		}
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		// Only strip when the tail is a port (no extra colon => not a bare IPv6).
		if !strings.Contains(h[:i], ":") {
			return h[:i]
		}
	}
	return h
}

// syntheticRequest builds the minimal *http.Request the resolver needs to classify
// a host: an https GET "/" with Host set and NO headers. The https scheme + nil
// body are inert for classification (the resolver reads Host + URL.Path only); the
// absence of headers guarantees X-Forwarded-Host cannot influence the decision.
func syntheticRequest(ctx context.Context, host string) *http.Request {
	r := &http.Request{
		Method: http.MethodGet,
		Host:   host,
		Header: make(http.Header),
		URL:    &url.URL{Scheme: "https", Host: host, Path: "/"},
	}
	return r.WithContext(ctx)
}
