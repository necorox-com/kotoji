// Package oidccfg resolves the EFFECTIVE OIDC (Google) provider configuration with
// the WordPress-style precedence — env OVERRIDES DB, per field — and owns the
// RUNTIME-REBUILDABLE OIDC provider: the go-oidc discovery + verifier are built
// LAZILY on first OIDC use and CACHED keyed by the effective (issuer | clientID |
// clientSecret | redirect) tuple, then rebuilt when the admin saves a config change
// (the admin PUT calls InvalidateProvider). It mirrors internal/domaincfg.
//
//	env set (non-empty)  -> env WINS; the DB value is ignored; the GUI field is
//	                        READ-ONLY ("locked by environment"). The env-set path is
//	                        equivalent to today (the value config.Load baked).
//	env empty, DB set    -> use the DB value (editable in the GUI). Cached; the
//	                        admin PUT calls InvalidateCache so a change applies live.
//	env empty, DB empty  -> the package default (e.g. the Google issuer) / derived
//	                        redirect from the effective control base URL.
//
// PROVIDER ENABLEMENT (decision #2): the enabled-provider SET is env-pinned when
// KOTOJI_AUTH_MODE is set (locked). Otherwise it is { password ALWAYS (break-glass)
// + oidc IFF the DB enables it AND a client id + secret are present }. Enabling OIDC
// never removes the password break-glass.
//
// DI / testability: the provider depends on a tiny Store seam (GetOIDCConfig), the
// domain resolver (for redirect derivation), the env OIDCConfig + per-field env-set
// flags, and an INJECTED Builder that performs discovery — so unit tests build the
// effective config + exercise the rebuild cache against a fake discovery/verifier
// with no network and no import of the auth package (which would cycle).
package oidccfg

import (
	"context"
	"net/http"
	"sync"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
)

// Source records WHERE an effective value came from, surfaced to the admin GUI.
type Source string

const (
	// SourceEnv means the value comes from a KOTOJI_* env var (locked, read-only).
	SourceEnv Source = "env"
	// SourceDB means the value comes from instance_settings (editable in the GUI).
	SourceDB Source = "db"
	// SourceDerived means neither env nor DB is set; a default/derived value applies
	// (the package default issuer, or the redirect derived from the control base URL).
	SourceDerived Source = "derived"
)

// Provider key strings (mirror the auth package's keys without importing it, so the
// effective provider list can be exposed in the public config / admin view).
const (
	providerPassword = "password"
	providerOIDC     = "oidc"
)

// callbackPath is appended to the effective control base URL to derive the OIDC
// redirect URL when none is explicitly configured (matches config.defaultRedirect).
const callbackPath = "/auth/callback"

// BuiltProvider is the runtime OIDC provider the lazy factory yields. It is exactly
// the auth.AuthProvider surface, re-declared here so oidccfg does not import the
// auth package (which depends on oidccfg for the lazy seam — that would cycle).
type BuiltProvider interface {
	Key() string
	Start(state, nonce, pkceVerifier string) (authURL string)
	Interactive() bool
	Exchange(ctx context.Context, code, pkceVerifier, expectedNonce string) (Claims, error)
}

// Claims mirrors auth.Claims at the oidccfg boundary so BuiltProvider has a concrete
// return type without importing auth. The composition-root builder adapts the real
// *auth.OIDCProvider onto this interface (its Claims has the SAME field names).
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	HostedDomain  string
	IsAdmin       bool
}

// Builder performs OIDC discovery (network) and returns a BuiltProvider for the
// resolved effective config. It is injected so tests use a fake (no network) and so
// oidccfg never imports auth. The composition root wires the real builder
// (auth.NewOIDCProvider behind an adapter). A discovery error is returned verbatim
// and surfaced to the admin (login attempt -> clear error; PUT validate -> 422),
// never panicked.
type Builder func(ctx context.Context, eff EffectiveOIDC) (BuiltProvider, error)

// Store is the narrow persistence seam: read the DB-stored OIDC config. *db.Store
// satisfies it directly. Kept minimal so tests inject a fake without a database.
type Store interface {
	GetOIDCConfig(ctx context.Context) (db.OIDCConfig, error)
}

// DomainResolver supplies the effective control base URL the redirect derives from
// when no redirect is explicitly configured. *domaincfg.Provider satisfies it.
type DomainResolver interface {
	ControlBaseURLFor(r *http.Request) string
}

// FieldString is one resolved string setting: value + source + env-locked flag.
type FieldString struct {
	Value  string
	Source Source
	Locked bool
}

// FieldList is one resolved list setting (emails/domains): the normalized slice +
// source + env-locked flag.
type FieldList struct {
	Value  []string
	Source Source
	Locked bool
}

// FieldBool is the resolved enabled flag: value + source + env-locked flag.
type FieldBool struct {
	Value  bool
	Source Source
	Locked bool
}

// EffectiveOIDC is the fully-resolved OIDC config for a request (env > DB > default/
// derived), with per-field provenance for the admin GUI. The bare scalar accessors
// (Issuer/ClientID/ClientSecret/RedirectURL/AllowedEmails/...) drive the build; the
// Field* members carry the source/locked metadata the admin view renders.
type EffectiveOIDC struct {
	Enabled        FieldBool
	Issuer         FieldString
	ClientID       FieldString
	ClientSecret   FieldString // value present server-side; NEVER serialized to the admin
	RedirectURL    FieldString // the EFFECTIVE redirect (explicit or derived)
	AllowedEmails  FieldList
	AllowedDomains FieldList
	AdminEmails    FieldList

	// RedirectExplicitlySet reports whether the redirect came from env/DB (true) vs.
	// being derived from the control base URL (false). The admin GET surfaces both
	// the configured redirect (may be empty) and the effective one.
	RedirectExplicitlySet bool
}

// cacheKey is the build identity: discovery is keyed by exactly the inputs that
// change the built oauth2 config + verifier. A change in ANY of these rebuilds.
type cacheKey struct {
	issuer       string
	clientID     string
	clientSecret string
	redirect     string
}

// Config constructs a Provider.
type Config struct {
	Store   Store
	Domain  DomainResolver
	Builder Builder
	// Env is the env-captured OIDC config (the values config.Load baked). Used for
	// the env-locked fields and as the fallback when a field's env var is unset but
	// the field is not in the DB either.
	Env config.OIDCConfig
	// EnvSet records which OIDC fields + AUTH_MODE were explicitly set in the env.
	EnvSet config.OIDCEnvSet
	// EnvAuthModes is the env-parsed enabled provider set (config.AuthModes). It is
	// AUTHORITATIVE only when EnvSet.AuthMode is true (KOTOJI_AUTH_MODE pinned).
	EnvAuthModes []config.AuthMode
}

// Provider resolves the effective OIDC config + auth-provider set and owns the lazy,
// rebuildable OIDC provider cache. Safe for concurrent use.
type Provider struct {
	store   Store
	domain  DomainResolver
	builder Builder
	env     config.OIDCConfig
	envSet  config.OIDCEnvSet

	// envAuthModeLocked / envOIDCEnabled capture the env auth-mode decision: when
	// KOTOJI_AUTH_MODE is set the provider set is pinned (locked) and envOIDCEnabled
	// records whether oidc was in that env set.
	envAuthModeLocked bool
	envOIDCEnabled    bool
	envPasswordOn     bool
	envNoneOn         bool

	// dbMu guards the cached DB read (mirrors domaincfg). Invalidated on admin PUT.
	dbMu      sync.RWMutex
	dbCache   db.OIDCConfig
	dbOnce    bool
	dbCacheOK bool

	// provMu guards the built-provider cache. The provider is rebuilt when the
	// effective cacheKey changes or InvalidateProvider is called.
	provMu   sync.Mutex
	built    BuiltProvider
	builtKey cacheKey
	builtErr error
	builtOK  bool
}

// New builds a Provider from the env-captured config + the seams.
func New(cfg Config) *Provider {
	p := &Provider{
		store:   cfg.Store,
		domain:  cfg.Domain,
		builder: cfg.Builder,
		env:     cfg.Env,
		envSet:  cfg.EnvSet,
	}
	p.envAuthModeLocked = cfg.EnvSet.AuthMode
	for _, m := range cfg.EnvAuthModes {
		switch m {
		case config.AuthModeOIDC:
			p.envOIDCEnabled = true
		case config.AuthModePassword:
			p.envPasswordOn = true
		case config.AuthModeNone:
			p.envNoneOn = true
		}
	}
	return p
}

// InvalidateCache drops the cached DB read so the next Resolve re-reads it (the
// admin PUT calls it on a successful persist). It does NOT touch the built provider.
func (p *Provider) InvalidateCache() {
	p.dbMu.Lock()
	p.dbOnce = false
	p.dbCacheOK = false
	p.dbMu.Unlock()
}

// InvalidateProvider drops the cached built OIDC provider so the next ProviderFor
// rebuilds it (re-runs discovery). The admin PUT calls BOTH InvalidateCache and
// InvalidateProvider so a config change applies without a restart.
func (p *Provider) InvalidateProvider() {
	p.provMu.Lock()
	p.built = nil
	p.builtOK = false
	p.builtErr = nil
	p.builtKey = cacheKey{}
	p.provMu.Unlock()
}

// cachedDBConfig returns the DB OIDC config, reading + caching it once until
// InvalidateCache. A read error caches as "unset" for this window (fail safe: a
// transient blip falls through to env, never crashes), mirroring domaincfg.
func (p *Provider) cachedDBConfig(ctx context.Context) db.OIDCConfig {
	p.dbMu.RLock()
	if p.dbOnce {
		c := p.dbCache
		p.dbMu.RUnlock()
		return c
	}
	p.dbMu.RUnlock()

	p.dbMu.Lock()
	defer p.dbMu.Unlock()
	if p.dbOnce {
		return p.dbCache
	}
	if p.store == nil {
		p.dbOnce = true
		p.dbCache = db.OIDCConfig{}
		return p.dbCache
	}
	cfg, err := p.store.GetOIDCConfig(ctx)
	p.dbOnce = true
	if err != nil {
		p.dbCache = db.OIDCConfig{}
		p.dbCacheOK = false
		return p.dbCache
	}
	p.dbCache = cfg
	p.dbCacheOK = true
	return cfg
}
