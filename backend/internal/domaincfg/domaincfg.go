// Package domaincfg resolves the two WordPress-style runtime-configurable routing
// settings — the base domain and the external control base URL — with the LOCKED
// precedence: env OVERRIDES DB, DB overrides a per-request derived default.
//
//	env set (non-empty)  -> env WINS; the DB value is ignored; the GUI field is
//	                        READ-ONLY ("locked by environment"). Behavior is then
//	                        IDENTICAL to today: a STATIC value captured once, with
//	                        ZERO per-request DB reads (the live deployment, which
//	                        sets both envs, is 100% unchanged).
//	env empty, DB set    -> use the DB value (editable in the GUI). Cached in
//	                        memory; INVALIDATED when the admin PUTs a change.
//	env empty, DB empty  -> DERIVE from the incoming request (control_base_url =
//	                        scheme://Host; base_domain = host without port). Used
//	                        only on a fresh, unconfigured install.
//
// DI / testability: the provider depends on a tiny Store seam (GetDomainConfig)
// and is constructed from the env-captured static values, so it is unit-testable
// against a fake store with no database. All new dynamic behavior is isolated to
// the env-EMPTY path; the env-set path never touches the store.
package domaincfg

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/necorox-com/kotoji/backend/internal/db"
)

// Source records WHERE an effective value came from, surfaced to the admin GUI so
// it can render "set via environment / locked" vs an editable field.
type Source string

const (
	// SourceEnv means the value comes from the KOTOJI_* env var (locked, read-only).
	SourceEnv Source = "env"
	// SourceDB means the value comes from instance_settings (editable in the GUI).
	SourceDB Source = "db"
	// SourceDerived means neither env nor DB is set; the value is derived per
	// request from the incoming Host (fresh-install fallback).
	SourceDerived Source = "derived"
)

// Store is the narrow persistence seam the provider needs: read the DB-stored
// domain/URL config. *db.Store satisfies it directly. Kept minimal so tests
// inject a fake without a database.
type Store interface {
	GetDomainConfig(ctx context.Context) (db.DomainConfig, error)
}

// Effective is one resolved setting: its value plus the source it came from and
// whether it is locked by the environment (env-set => read-only in the GUI).
type Effective struct {
	Value  string
	Source Source
	Locked bool
}

// Resolved bundles both effective settings plus the control host (the bare host
// of the control base URL, no port) used as the host-only cookie domain and the
// OIDC-redirect/CORS base.
type Resolved struct {
	BaseDomain     Effective
	ControlBaseURL Effective
	// ControlHost is the bare hostname (no port) of ControlBaseURL.Value. It is
	// the host-only cookie Domain and the OIDC-redirect host; it must never be the
	// wildcard parent. Empty only when ControlBaseURL.Value is unparseable.
	ControlHost string
}

// Provider resolves the effective base domain + control base URL. It is safe for
// concurrent use. When BOTH envs are set it is a pure static fast path (no store,
// no cache, no lock contention beyond the load). When an env is empty it consults
// the store, caching the DB value until InvalidateCache is called.
type Provider struct {
	store Store

	// Static env-captured values (the SAME values config.Load baked at startup).
	// An empty string means the env var was UNSET (so the dynamic path applies).
	envBaseDomain     string
	envBaseDomainSet  bool
	envControlBaseURL string
	envControlSet     bool

	// cache holds the last DB read for the env-empty fields. Guarded by mu. The
	// admin PUT calls InvalidateCache so the next read reflects the change.
	mu        sync.RWMutex
	cache     db.DomainConfig
	cacheOK   bool // a DB read has been cached
	cacheErr  error
	cacheOnce bool // whether we have ever attempted a cached read
}

// Config constructs a Provider. envBaseDomain / envControlBaseURL are the raw env
// values (empty => unset). The *Set flags distinguish "env explicitly set" from
// "env empty" so the live deployment (both set) stays on the static fast path.
type Config struct {
	Store             Store
	EnvBaseDomain     string
	EnvBaseDomainSet  bool
	EnvControlBaseURL string
	EnvControlSet     bool
}

// New builds a Provider from the env-captured config + the store seam.
func New(cfg Config) *Provider {
	return &Provider{
		store:             cfg.Store,
		envBaseDomain:     strings.TrimSpace(cfg.EnvBaseDomain),
		envBaseDomainSet:  cfg.EnvBaseDomainSet,
		envControlBaseURL: strings.TrimSpace(cfg.EnvControlBaseURL),
		envControlSet:     cfg.EnvControlSet,
	}
}

// InvalidateCache drops the cached DB read so the next Resolve re-reads it. The
// admin PUT handler calls this on a successful persist so a runtime change applies
// without a restart. No-op on the static (env-set) fast path.
func (p *Provider) InvalidateCache() {
	p.mu.Lock()
	p.cacheOK = false
	p.cacheOnce = false
	p.cacheErr = nil
	p.mu.Unlock()
}

// baseDomainEnvLocked / controlEnvLocked report whether the respective field is
// pinned by the environment (env set non-empty => locked, read-only in the GUI).
func (p *Provider) baseDomainEnvLocked() bool { return p.envBaseDomainSet && p.envBaseDomain != "" }
func (p *Provider) controlEnvLocked() bool    { return p.envControlSet && p.envControlBaseURL != "" }

// needsDB reports whether ANY field falls through env (so the DB must be read).
// When both envs are set this is false and Resolve never touches the store.
func (p *Provider) needsDB() bool {
	return !p.baseDomainEnvLocked() || !p.controlEnvLocked()
}

// Resolve returns the effective settings for the given request. r may be nil
// (e.g. a startup probe / admin read with no incoming Host) — derivation then
// yields empty values, which the admin GUI surfaces as "not yet configured".
//
// The env-set fields are returned STATIC (no DB read). For any env-empty field
// the cached DB value is consulted; if that too is empty, the value is derived
// from r (scheme://Host for control; host-without-port for base domain).
func (p *Provider) Resolve(ctx context.Context, r *http.Request) Resolved {
	var dbcfg db.DomainConfig
	if p.needsDB() && p.store != nil {
		dbcfg = p.cachedDomainConfig(ctx)
	}

	scheme, host := requestSchemeHost(r)

	res := Resolved{
		BaseDomain:     p.resolveBaseDomain(dbcfg, host),
		ControlBaseURL: p.resolveControlBaseURL(dbcfg, scheme, host),
	}
	res.ControlHost = hostOf(res.ControlBaseURL.Value)
	return res
}

// resolveBaseDomain applies the precedence for the base domain field.
func (p *Provider) resolveBaseDomain(dbcfg db.DomainConfig, host string) Effective {
	if p.baseDomainEnvLocked() {
		return Effective{Value: p.envBaseDomain, Source: SourceEnv, Locked: true}
	}
	if dbcfg.BaseDomainSet && dbcfg.BaseDomain != "" {
		return Effective{Value: dbcfg.BaseDomain, Source: SourceDB, Locked: false}
	}
	// Derive: the control host without its port. Empty when no request host.
	return Effective{Value: hostWithoutPort(host), Source: SourceDerived, Locked: false}
}

// resolveControlBaseURL applies the precedence for the control base URL field.
func (p *Provider) resolveControlBaseURL(dbcfg db.DomainConfig, scheme, host string) Effective {
	if p.controlEnvLocked() {
		return Effective{Value: p.envControlBaseURL, Source: SourceEnv, Locked: true}
	}
	if dbcfg.ControlBaseURLSet && dbcfg.ControlBaseURL != "" {
		return Effective{Value: dbcfg.ControlBaseURL, Source: SourceDB, Locked: false}
	}
	// Derive: scheme://Host of the incoming control request. Empty when no host.
	derived := ""
	if host != "" {
		s := scheme
		if s == "" {
			s = "http"
		}
		derived = s + "://" + host
	}
	return Effective{Value: derived, Source: SourceDerived, Locked: false}
}

// cachedDomainConfig returns the DB config, reading + caching it once until
// InvalidateCache. A DB read error caches as "unset" for THIS resolve (fail safe:
// a transient blip falls through to derivation rather than crashing), and the
// error is remembered so callers that care can surface it.
func (p *Provider) cachedDomainConfig(ctx context.Context) db.DomainConfig {
	p.mu.RLock()
	if p.cacheOnce {
		c := p.cache
		p.mu.RUnlock()
		return c
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	// Re-check under the write lock (another goroutine may have filled it).
	if p.cacheOnce {
		return p.cache
	}
	cfg, err := p.store.GetDomainConfig(ctx)
	p.cacheOnce = true
	p.cacheErr = err
	if err != nil {
		// Treat a read failure as "no DB value" for this window; do not crash. The
		// derivation fallback keeps a fresh instance usable on a transient blip.
		p.cache = db.DomainConfig{}
		p.cacheOK = false
		return p.cache
	}
	p.cache = cfg
	p.cacheOK = true
	return cfg
}

// BaseDomainFor is a convenience returning just the effective base domain value
// for r (env > DB > derived). Satisfies the auth/resolver seam that only needs the
// single string.
func (p *Provider) BaseDomainFor(r *http.Request) string {
	return p.Resolve(r.Context(), r).BaseDomain.Value
}

// ControlBaseURLFor is a convenience returning just the effective control base URL
// value for r (env > DB > derived).
func (p *Provider) ControlBaseURLFor(r *http.Request) string {
	return p.Resolve(r.Context(), r).ControlBaseURL.Value
}

// EnvBaseDomainLocked reports whether the base-domain field is env-locked. The
// admin handler uses it to REJECT a write to a locked field (do not silently
// no-op).
func (p *Provider) EnvBaseDomainLocked() bool { return p.baseDomainEnvLocked() }

// EnvControlBaseURLLocked reports whether the control-base-URL field is env-locked.
func (p *Provider) EnvControlBaseURLLocked() bool { return p.controlEnvLocked() }

// requestSchemeHost extracts the request scheme + host, honoring the X-Forwarded-
// Proto / X-Forwarded-Host headers a reverse proxy sets (the documented topology
// terminates TLS at the edge). r may be nil.
func requestSchemeHost(r *http.Request) (scheme, host string) {
	if r == nil {
		return "", ""
	}
	host = r.Host
	if xfh := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xfh != "" {
		// Take the first hop of a comma list (the original client-facing host).
		if i := strings.IndexByte(xfh, ','); i >= 0 {
			xfh = strings.TrimSpace(xfh[:i])
		}
		host = xfh
	}
	scheme = "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xfp := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xfp != "" {
		if i := strings.IndexByte(xfp, ','); i >= 0 {
			xfp = strings.TrimSpace(xfp[:i])
		}
		scheme = strings.ToLower(xfp)
	}
	return scheme, host
}

// hostWithoutPort strips a trailing :port from a host:port (IPv6-safe).
func hostWithoutPort(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	// No port (or an unbracketed IPv6 literal): return as-is.
	return host
}

// hostOf returns the bare hostname (no port) of an absolute URL, or "" if it does
// not parse / has no host.
func hostOf(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}
