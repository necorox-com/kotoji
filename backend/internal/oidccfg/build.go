package oidccfg

import (
	"context"
	"errors"
	"net/http"
)

// ErrOIDCNotConfigured is returned by ProviderFor when OIDC is not effectively
// usable for the request (disabled, or missing client id/secret). The login/callback
// handlers map it to a clear admin-facing error rather than building a doomed
// provider.
var ErrOIDCNotConfigured = errors.New("oidccfg: oidc is not configured or not enabled")

// ProviderFor returns the runtime OIDC provider for r, building it LAZILY from the
// effective config on first use and CACHING it keyed by the effective (issuer |
// clientID | clientSecret | redirect) tuple. The cache is rebuilt automatically when
// any of those change (the key differs) and explicitly when InvalidateProvider is
// called (the admin PUT). Discovery errors are CACHED for the same key so a broken
// issuer does not hammer the network on every login; InvalidateProvider clears the
// error so the admin can retry after fixing the config.
//
// It returns ErrOIDCNotConfigured when OIDC is not effectively usable for r (so the
// caller surfaces a clear "OIDC is not enabled" message), and the discovery error
// (e.g. an unreachable issuer) when the build itself fails — NEVER a panic/crash.
func (p *Provider) ProviderFor(ctx context.Context, r *http.Request) (BuiltProvider, error) {
	eff := p.Resolve(ctx, r)
	if !eff.OIDCUsable() {
		return nil, ErrOIDCNotConfigured
	}

	key := cacheKey{
		issuer:       eff.Issuer.Value,
		clientID:     eff.ClientID.Value,
		clientSecret: eff.ClientSecret.Value,
		redirect:     eff.RedirectURL.Value,
	}

	p.provMu.Lock()
	defer p.provMu.Unlock()
	// Cache hit for the SAME effective key: return the built provider (or its cached
	// discovery error). A key change (config edited) falls through to a rebuild.
	if p.builtOK && p.builtKey == key {
		if p.builtErr != nil {
			return nil, p.builtErr
		}
		return p.built, nil
	}

	// Build (network discovery) under the lock. The injected builder owns the
	// round-trip; we only cache its result keyed by the effective tuple.
	if p.builder == nil {
		return nil, errors.New("oidccfg: no provider builder configured")
	}
	built, err := p.builder(ctx, eff)
	p.builtKey = key
	p.builtOK = true
	if err != nil {
		// Cache the error for this key so repeated logins do not re-run a failing
		// discovery on every request. InvalidateProvider clears it after a config fix.
		p.built = nil
		p.builtErr = err
		return nil, err
	}
	p.built = built
	p.builtErr = nil
	return built, nil
}

// ValidateDiscovery builds the provider for the given effective config WITHOUT using
// or populating the cache. The admin PUT uses it to optionally pre-flight the issuer
// (so a bad issuer is reported as a 422 at save time rather than silently failing the
// next login). A nil builder or an unusable config returns an error the caller maps
// to a validation failure.
func (p *Provider) ValidateDiscovery(ctx context.Context, eff EffectiveOIDC) error {
	if !eff.OIDCUsable() {
		return ErrOIDCNotConfigured
	}
	if p.builder == nil {
		return errors.New("oidccfg: no provider builder configured")
	}
	_, err := p.builder(ctx, eff)
	return err
}
