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
	"sync"
	"time"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

const (
	// existsLookupTimeout bounds the site-existence DB lookup INSIDE the TLS handshake
	// (D2). It is independent of the handshake context so a slow DB cannot stretch the
	// handshake, and short enough that many attacker SNI probes cannot each hold a DB
	// connection. On timeout the gate fails closed (refuse — no issuance).
	existsLookupTimeout = 3 * time.Second
	// existsCacheTTL is how long a positive existence result is trusted. Short so a
	// freshly-created site becomes issuable quickly and a deleted one stops being
	// issuable soon, while still collapsing repeated probes for the same host.
	existsCacheTTL = 30 * time.Second
	// existsNegCacheTTL is how long a NEGATIVE (no such site) result is trusted. This
	// is the anti-abuse lever: repeated unknown-host probes (the attack D2 targets)
	// are answered from cache for this window instead of hammering the DB each time.
	existsNegCacheTTL = 10 * time.Second
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

// existsCacheEntry is a cached existence result with its expiry. exists records
// whether the handle was found; expires bounds how long the entry is trusted.
type existsCacheEntry struct {
	exists  bool
	expires time.Time
}

// Decider is the on-demand issuance gate. It answers ONE question — "may this
// instance obtain a certificate for host H?" — by reusing the live routing
// classifier (resolve.Resolver) plus the control-host getter and the site-exists
// lookup. The existence lookup is bounded by its own deadline and memoized in a
// small TTL cache (D2) so repeated unknown-host probes cannot hammer the DB during
// the handshake. It is safe for concurrent use.
type Decider struct {
	controlHost ControlHostFunc
	resolver    resolve.Resolver
	siteExists  SiteExistsFunc

	// now is the injected clock (defaults to time.Now) so the TTL cache is testable.
	now func() time.Time

	// cacheMu guards cache. A plain map + mutex is sufficient: entries are tiny and
	// the handshake path is not hot enough to need a sharded/lock-free structure.
	cacheMu sync.Mutex
	cache   map[string]existsCacheEntry
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
	return &Decider{
		controlHost: controlHost,
		resolver:    resolver,
		siteExists:  siteExists,
		now:         time.Now,
		cache:       make(map[string]existsCacheEntry),
	}, nil
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
	// or a former-handle redirect. The lookup is bounded + cached (D2) so repeated
	// unknown-host probes cannot exhaust DB/handshake capacity. A store error or
	// timeout is REFUSE (fail-closed) so a transient DB blip can never become an
	// unbounded issuance vector.
	exists, err := d.existsCached(ctx, target.Handle)
	if err != nil {
		return fmt.Errorf("%w: existence check failed for %q: %v", errRefused, target.Handle, err)
	}
	if !exists {
		return fmt.Errorf("%w: no site for handle %q", errRefused, target.Handle)
	}
	return nil
}

// existsCached resolves handle existence through a small TTL cache backed by the
// bounded siteExists lookup (D2). The cache collapses repeated probes for the same
// handle — both positive (a real, busy site) and, crucially, NEGATIVE (an attacker
// spraying unknown SNI names) — so the DB is touched at most once per handle per TTL
// window rather than once per handshake. A cache MISS triggers a single lookup under
// its OWN short deadline (existsLookupTimeout), independent of the handshake ctx, so a
// slow DB cannot stretch the handshake; the result (including a definitive negative)
// is cached, while transient errors/timeouts are NOT cached and fail closed.
func (d *Decider) existsCached(ctx context.Context, handle string) (bool, error) {
	now := d.now()

	// Fast path: a fresh cached entry answers without touching the DB.
	d.cacheMu.Lock()
	if e, ok := d.cache[handle]; ok && now.Before(e.expires) {
		d.cacheMu.Unlock()
		return e.exists, nil
	}
	d.cacheMu.Unlock()

	// MISS (or expired): one bounded lookup. The deadline derives from a fresh
	// background context, NOT the handshake ctx, so the cap is honored even when the
	// handshake ctx has a longer (or no) deadline — a slow DB therefore cannot stretch
	// the handshake. Handshake cancellation is honored separately by the ctx.Err()
	// check below (we deliberately do NOT parent lookupCtx on ctx, to keep the timeout
	// strictly independent of whatever deadline the handshake ctx may carry).
	lookupCtx, cancel := context.WithTimeout(context.Background(), existsLookupTimeout)
	defer cancel()

	// Honor an already-cancelled handshake (e.g. client hung up) without a DB hit.
	if err := ctx.Err(); err != nil {
		return false, err
	}

	exists, err := d.siteExists(lookupCtx, handle)
	if err != nil {
		// Do NOT cache transient failures/timeouts — fail closed for THIS handshake
		// only, so a recovered DB immediately resumes correct decisions.
		return false, err
	}

	// Cache the definitive result. Negative results get a shorter TTL: it is the
	// anti-abuse lever (unknown-host spray) yet must expire fast enough that a newly
	// created site becomes issuable promptly.
	ttl := existsCacheTTL
	if !exists {
		ttl = existsNegCacheTTL
	}
	d.cacheMu.Lock()
	d.cache[handle] = existsCacheEntry{exists: exists, expires: now.Add(ttl)}
	d.cacheMu.Unlock()

	return exists, nil
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
