package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/necorox-com/kotoji/backend/internal/oidccfg"
)

// OIDCRuntime is the seam the Auth surface uses to obtain the RUNTIME-configurable
// OIDC provider and the effective auth-provider set. *oidccfg.Provider satisfies it
// (via the small bridge below): the OIDC provider is built lazily from the effective
// (env > DB) config and rebuilt when the admin saves a change, so the login/callback
// handlers no longer hold a static OIDC provider. nil keeps the legacy static-provider
// behavior (the env-set fast path / unit tests that wire a single provider directly).
//
// DI / testability: it is an interface so the auth package does not import oidccfg's
// concrete provider in its tests; a stub returns a fakeProvider + a fixed list.
type OIDCRuntime interface {
	// InteractiveProvider returns the runtime OIDC provider for r, built lazily +
	// cached. It returns (nil, ErrOIDCNotConfigured) when OIDC is not effectively
	// enabled/usable, and a discovery error when the issuer is unreachable — the
	// caller surfaces either as a clear login error, never a crash.
	InteractiveProvider(ctx context.Context, r *http.Request) (AuthProvider, error)
	// Providers returns the effective enabled auth-provider set for r (normalized
	// order), e.g. ["oidc","password"] — what /api/config advertises.
	Providers(ctx context.Context, r *http.Request) []string
	// OIDCEnabledEffective reports whether OIDC is effectively enabled+usable for r
	// (so the callback path can 403 cleanly when it is off).
	OIDCEnabledEffective(ctx context.Context, r *http.Request) bool
}

// SetOIDCRuntime installs the runtime OIDC seam. Optional: when set, GET /auth/login
// (the OIDC redirect) + /auth/callback + /api/config.authProviders all consult the
// EFFECTIVE config (env > DB) instead of the static interactive provider. The
// composition root wires it; tests that drive a static provider leave it nil.
func (a *Auth) SetOIDCRuntime(rt OIDCRuntime) { a.oidcRuntime = rt }

// builtProviderAdapter adapts an oidccfg.BuiltProvider onto auth.AuthProvider,
// translating oidccfg.Claims (a structural copy of auth.Claims) back into auth.Claims
// so completeLogin's existing path is unchanged. It exists only because oidccfg may
// not import auth (auth imports oidccfg for the lazy seam — the reverse would cycle).
type builtProviderAdapter struct{ inner oidccfg.BuiltProvider }

func (b builtProviderAdapter) Key() string       { return b.inner.Key() }
func (b builtProviderAdapter) Interactive() bool { return b.inner.Interactive() }
func (b builtProviderAdapter) Start(state, nonce, verifier string) string {
	return b.inner.Start(state, nonce, verifier)
}

func (b builtProviderAdapter) Exchange(ctx context.Context, code, verifier, nonce string) (Claims, error) {
	c, err := b.inner.Exchange(ctx, code, verifier, nonce)
	if err != nil {
		return Claims{}, err
	}
	return Claims{
		Subject:       c.Subject,
		Email:         c.Email,
		EmailVerified: c.EmailVerified,
		Name:          c.Name,
		HostedDomain:  c.HostedDomain,
		IsAdmin:       c.IsAdmin,
	}, nil
}

// oidcRuntimeAdapter bridges *oidccfg.Provider onto the auth.OIDCRuntime seam,
// wrapping the built oidccfg provider in builtProviderAdapter. The composition root
// uses NewOIDCRuntimeAdapter so app.go does not duplicate the adapter glue.
type oidcRuntimeAdapter struct{ p *oidccfg.Provider }

// NewOIDCRuntimeAdapter wraps an *oidccfg.Provider as an auth.OIDCRuntime. It is the
// single place the oidccfg <-> auth type bridge lives (Claims + BuiltProvider).
func NewOIDCRuntimeAdapter(p *oidccfg.Provider) OIDCRuntime { return oidcRuntimeAdapter{p: p} }

func (a oidcRuntimeAdapter) InteractiveProvider(ctx context.Context, r *http.Request) (AuthProvider, error) {
	built, err := a.p.ProviderFor(ctx, r)
	if err != nil {
		return nil, err
	}
	return builtProviderAdapter{inner: built}, nil
}

func (a oidcRuntimeAdapter) Providers(ctx context.Context, r *http.Request) []string {
	return a.p.Providers(ctx, r)
}

func (a oidcRuntimeAdapter) OIDCEnabledEffective(ctx context.Context, r *http.Request) bool {
	return a.p.OIDCEnabledEffective(ctx, r)
}

// NewOIDCBuilder returns an oidccfg.Builder that performs OIDC discovery and yields a
// runtime provider for the resolved effective config. The composition root passes it
// to oidccfg.New so the discovery code (NewOIDCProvider) stays in the auth package
// and oidccfg never imports go-oidc. A discovery failure is returned verbatim (the
// caller maps it to a clear admin error), never panicked.
func NewOIDCBuilder() oidccfg.Builder {
	return func(ctx context.Context, eff oidccfg.EffectiveOIDC) (oidccfg.BuiltProvider, error) {
		prov, err := NewOIDCProvider(ctx, eff.EffectiveOIDCConfig())
		if err != nil {
			return nil, err
		}
		return oidcBuiltProvider{p: prov}, nil
	}
}

// oidcBuiltProvider adapts *OIDCProvider onto oidccfg.BuiltProvider (auth.Claims ->
// oidccfg.Claims). It is the inverse of builtProviderAdapter, used only inside the
// builder so the runtime provider satisfies the oidccfg seam.
type oidcBuiltProvider struct{ p *OIDCProvider }

func (b oidcBuiltProvider) Key() string       { return b.p.Key() }
func (b oidcBuiltProvider) Interactive() bool { return b.p.Interactive() }
func (b oidcBuiltProvider) Start(state, nonce, verifier string) string {
	return b.p.Start(state, nonce, verifier)
}

func (b oidcBuiltProvider) Exchange(ctx context.Context, code, verifier, nonce string) (oidccfg.Claims, error) {
	c, err := b.p.Exchange(ctx, code, verifier, nonce)
	if err != nil {
		return oidccfg.Claims{}, err
	}
	return oidccfg.Claims{
		Subject:       c.Subject,
		Email:         c.Email,
		EmailVerified: c.EmailVerified,
		Name:          c.Name,
		HostedDomain:  c.HostedDomain,
		IsAdmin:       c.IsAdmin,
	}, nil
}

// resolveInteractiveOIDC returns the effective interactive OIDC provider for r. When
// the runtime seam is wired it builds/returns the lazy provider (env > DB config);
// otherwise it falls back to the static a.interactive (the legacy single-provider
// path). The returned error is non-nil only when OIDC is configured-but-broken (a
// discovery failure) so the caller surfaces a clear message; a cleanly-disabled OIDC
// returns (nil, nil) so GET /auth/login can fall back to telling the caller to POST.
func (a *Auth) resolveInteractiveOIDC(ctx context.Context, r *http.Request) (AuthProvider, error) {
	if a.oidcRuntime == nil {
		return a.interactive, nil
	}
	prov, err := a.oidcRuntime.InteractiveProvider(ctx, r)
	if err != nil {
		if errors.Is(err, oidccfg.ErrOIDCNotConfigured) {
			return nil, nil // cleanly disabled, not an error
		}
		return nil, err // a real discovery failure: surface it
	}
	return prov, nil
}

// effectiveProviders returns the auth-provider set advertised by /api/config for r.
// With the runtime seam wired it is the effective set (env-pinned OR password-always
// + oidc-iff-usable); otherwise the static legacy set from config.
func (a *Auth) effectiveProviders(ctx context.Context, r *http.Request) []string {
	if a.oidcRuntime != nil {
		return a.oidcRuntime.Providers(ctx, r)
	}
	return a.cfg.AuthModeStrings()
}

// compile-time guarantee the bridge satisfies the seam.
var _ OIDCRuntime = oidcRuntimeAdapter{}
