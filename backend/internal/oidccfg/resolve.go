package oidccfg

import (
	"context"
	"net/http"
	"strings"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
)

// Resolve returns the EFFECTIVE OIDC config for r (env > DB > default/derived) with
// per-field provenance. r may be nil (a startup probe / admin read with no Host):
// the derived redirect is then empty, which the admin GUI shows as "not configured".
//
// The env-set fields are returned STATIC (the value config.Load baked). For any
// env-empty field the cached DB value is consulted; if that too is unset the package
// default applies (the Google issuer, or the redirect derived from the effective
// control base URL). The client secret is carried in the result (server-side only)
// so the build has it; the admin layer never serializes it.
func (p *Provider) Resolve(ctx context.Context, r *http.Request) EffectiveOIDC {
	dbcfg := p.cachedDBConfig(ctx)

	var eff EffectiveOIDC
	eff.Enabled = p.resolveEnabled(dbcfg)
	eff.Issuer = p.resolveIssuer(dbcfg)
	eff.ClientID = p.resolveClientID(dbcfg)
	eff.ClientSecret = p.resolveClientSecret(dbcfg)
	eff.AllowedEmails = p.resolveList(dbcfg.AllowedEmails, dbcfg.AllowedEmailsSet, p.env.AllowedEmails, p.envSet.AllowedEmails)
	eff.AllowedDomains = p.resolveList(dbcfg.AllowedDomains, dbcfg.AllowedDomainsSet, p.env.AllowedDomains, p.envSet.AllowedDomains)
	eff.AdminEmails = p.resolveList(dbcfg.AdminEmails, dbcfg.AdminEmailsSet, p.env.AdminEmails, p.envSet.AdminEmails)
	eff.RedirectURL, eff.RedirectExplicitlySet = p.resolveRedirect(dbcfg, r)
	return eff
}

// resolveEnabled applies env > DB precedence to the OIDC enabled flag.
//
// When KOTOJI_AUTH_MODE is set (env-locked) the env decides whether oidc is enabled
// (it is in the pinned set), and the field is locked. Otherwise the DB flag applies
// (editable); absent => disabled (a fresh install has oidc off until the admin
// enables it). The effective "is OIDC actually usable" gate (enabled AND credentials
// present) is computed by OIDCUsable, not here — this is purely the configured flag.
func (p *Provider) resolveEnabled(dbcfg db.OIDCConfig) FieldBool {
	if p.envAuthModeLocked {
		return FieldBool{Value: p.envOIDCEnabled, Source: SourceEnv, Locked: true}
	}
	if dbcfg.EnabledSet {
		return FieldBool{Value: dbcfg.Enabled, Source: SourceDB, Locked: false}
	}
	return FieldBool{Value: false, Source: SourceDerived, Locked: false}
}

// resolveIssuer applies env > DB > default (Google) precedence to the issuer.
func (p *Provider) resolveIssuer(dbcfg db.OIDCConfig) FieldString {
	if p.envSet.Issuer {
		return FieldString{Value: p.env.Issuer, Source: SourceEnv, Locked: true}
	}
	if dbcfg.IssuerSet && dbcfg.Issuer != "" {
		return FieldString{Value: dbcfg.Issuer, Source: SourceDB, Locked: false}
	}
	// Default to the package's Google issuer (config bakes it into env even when the
	// env var is unset; we surface it as "derived" so the GUI shows the default).
	return FieldString{Value: defaultIssuer(p.env.Issuer), Source: SourceDerived, Locked: false}
}

// resolveClientID applies env > DB precedence to the client id.
func (p *Provider) resolveClientID(dbcfg db.OIDCConfig) FieldString {
	if p.envSet.ClientID {
		return FieldString{Value: p.env.ClientID, Source: SourceEnv, Locked: true}
	}
	if dbcfg.ClientIDSet && dbcfg.ClientID != "" {
		return FieldString{Value: dbcfg.ClientID, Source: SourceDB, Locked: false}
	}
	return FieldString{Value: "", Source: SourceDerived, Locked: false}
}

// resolveClientSecret applies env > DB precedence to the client secret. The VALUE is
// the decrypted secret (server-side only); the admin layer reduces it to a boolean.
func (p *Provider) resolveClientSecret(dbcfg db.OIDCConfig) FieldString {
	if p.envSet.ClientSecret {
		return FieldString{Value: p.env.ClientSecret, Source: SourceEnv, Locked: true}
	}
	if dbcfg.ClientSecretSet && dbcfg.ClientSecret != "" {
		return FieldString{Value: dbcfg.ClientSecret, Source: SourceDB, Locked: false}
	}
	return FieldString{Value: "", Source: SourceDerived, Locked: false}
}

// resolveList applies env > DB precedence to a CSV list field, normalizing both
// sides (lowercase + trim + dedup) so the access policy sees a clean set.
func (p *Provider) resolveList(dbRaw string, dbSet bool, envVal []string, envIsSet bool) FieldList {
	if envIsSet {
		return FieldList{Value: lowerList(envVal), Source: SourceEnv, Locked: true}
	}
	if dbSet && strings.TrimSpace(dbRaw) != "" {
		return FieldList{Value: lowerList(splitCSV(dbRaw)), Source: SourceDB, Locked: false}
	}
	return FieldList{Value: nil, Source: SourceDerived, Locked: false}
}

// resolveRedirect applies env > DB > derived precedence. The derived value is the
// effective control base URL + /auth/callback (decision #4). Returns the effective
// redirect and whether it was explicitly configured (env/DB) vs. derived.
func (p *Provider) resolveRedirect(dbcfg db.OIDCConfig, r *http.Request) (FieldString, bool) {
	if p.envSet.RedirectURL {
		return FieldString{Value: p.env.RedirectURL, Source: SourceEnv, Locked: true}, true
	}
	if dbcfg.RedirectURLSet && dbcfg.RedirectURL != "" {
		return FieldString{Value: dbcfg.RedirectURL, Source: SourceDB, Locked: false}, true
	}
	// Derive from the effective control base URL (env > DB > request-derived).
	base := ""
	if p.domain != nil {
		base = p.domain.ControlBaseURLFor(r)
	}
	derived := ""
	if base != "" {
		derived = strings.TrimRight(base, "/") + callbackPath
	}
	return FieldString{Value: derived, Source: SourceDerived, Locked: false}, false
}

// EffectiveOIDCConfig folds the EffectiveOIDC into a config.OIDCConfig the build /
// access policy consume. It is the scalar projection used to construct the runtime
// provider (issuer/client/secret/redirect + the normalized allowlists).
func (eff EffectiveOIDC) EffectiveOIDCConfig() config.OIDCConfig {
	return config.OIDCConfig{
		Issuer:         eff.Issuer.Value,
		ClientID:       eff.ClientID.Value,
		ClientSecret:   eff.ClientSecret.Value,
		RedirectURL:    eff.RedirectURL.Value,
		AllowedEmails:  eff.AllowedEmails.Value,
		AllowedDomains: eff.AllowedDomains.Value,
		AdminEmails:    eff.AdminEmails.Value,
	}
}

// OIDCUsable reports whether OIDC is EFFECTIVELY usable for sign-in: the configured
// enabled flag is on AND a client id + secret are present. It is the gate the
// effective-provider list keys off (decision #2: oidc iff enabled AND credentials).
func (eff EffectiveOIDC) OIDCUsable() bool {
	return eff.Enabled.Value && eff.ClientID.Value != "" && eff.ClientSecret.Value != ""
}

// AccessGated reports whether the effective config has at least one access gate
// (an email allowlist OR a domain allowlist). With neither, every sign-in is denied
// (fail-closed); the admin save-validation rejects enabling OIDC in that state.
func (eff EffectiveOIDC) AccessGated() bool {
	return len(eff.AllowedEmails.Value) > 0 || len(eff.AllowedDomains.Value) > 0
}

// Providers returns the EFFECTIVE enabled auth-provider set in normalized order
// (oidc, password) for r. When KOTOJI_AUTH_MODE is env-set the env set is returned
// verbatim (locked). Otherwise: password is ALWAYS present (break-glass) and oidc is
// added IFF OIDCUsable() — so enabling OIDC never removes the password break-glass
// (decision #2). The dev no-auth provider is only ever present via the env set.
func (p *Provider) Providers(ctx context.Context, r *http.Request) []string {
	if p.envAuthModeLocked {
		// Env-pinned: return the env set in normalized order (oidc, password, none).
		var out []string
		if p.envOIDCEnabled {
			out = append(out, providerOIDC)
		}
		if p.envPasswordOn {
			out = append(out, providerPassword)
		}
		if p.envNoneOn {
			out = append(out, "none")
		}
		return out
	}
	// Runtime-configurable: password always, oidc iff usable.
	eff := p.Resolve(ctx, r)
	var out []string
	if eff.OIDCUsable() {
		out = append(out, providerOIDC)
	}
	out = append(out, providerPassword)
	return out
}

// OIDCEnabledEffective reports whether OIDC is effectively enabled+usable for r,
// honoring the env-pinned set. Used by the login/callback paths to decide whether to
// build the provider at all.
func (p *Provider) OIDCEnabledEffective(ctx context.Context, r *http.Request) bool {
	if p.envAuthModeLocked {
		return p.envOIDCEnabled
	}
	return p.Resolve(ctx, r).OIDCUsable()
}

// AuthModeEnvLocked reports whether the enabled-provider set is pinned by
// KOTOJI_AUTH_MODE (so the GUI cannot toggle OIDC on/off).
func (p *Provider) AuthModeEnvLocked() bool { return p.envAuthModeLocked }

// ---- small helpers (no external deps; mirror config's CSV normalization) ----

// defaultIssuer returns the env issuer when non-empty (config bakes the Google
// default there even when unset), else the Google issuer constant as a last resort.
func defaultIssuer(envIssuer string) string {
	if strings.TrimSpace(envIssuer) != "" {
		return envIssuer
	}
	return "https://accounts.google.com"
}

// splitCSV trims + drops empty fields from a comma-separated string.
func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// lowerList lowercases + trims every entry, dropping empties/dups (first-seen order).
func lowerList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
