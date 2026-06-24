package oidccfg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
)

// fakeStore is an in-memory oidccfg.Store recording how many times the DB was read
// so the cache (one read) and InvalidateCache (re-read) behavior are pinned.
type fakeStore struct {
	cfg   db.OIDCConfig
	reads int32
	err   error
}

func (f *fakeStore) GetOIDCConfig(_ context.Context) (db.OIDCConfig, error) {
	atomic.AddInt32(&f.reads, 1)
	if f.err != nil {
		return db.OIDCConfig{}, f.err
	}
	return f.cfg, nil
}

func (f *fakeStore) readCount() int { return int(atomic.LoadInt32(&f.reads)) }

// fakeDomain is a fixed control-base-URL resolver for the redirect derivation.
type fakeDomain struct{ base string }

func (d fakeDomain) ControlBaseURLFor(*http.Request) string { return d.base }

// fakeBuilt is a stub BuiltProvider; its only job is to be cache-identity-checkable.
type fakeBuilt struct{ tag string }

func (b *fakeBuilt) Key() string                 { return "oidc" }
func (b *fakeBuilt) Interactive() bool           { return true }
func (b *fakeBuilt) Start(_, _, _ string) string { return "https://idp/auth?" + b.tag }
func (b *fakeBuilt) Exchange(context.Context, string, string, string) (Claims, error) {
	return Claims{}, nil
}

// countingBuilder yields a fresh *fakeBuilt per call and counts builds so the rebuild
// tests can assert discovery ran exactly when the effective key changed.
func countingBuilder(count *int32, err error) Builder {
	return func(_ context.Context, eff EffectiveOIDC) (BuiltProvider, error) {
		atomic.AddInt32(count, 1)
		if err != nil {
			return nil, err
		}
		return &fakeBuilt{tag: eff.ClientID.Value}, nil
	}
}

func req() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	r.Host = "kotoji.example.com"
	return r
}

// dbConfigured is a fully-usable DB OIDC config (enabled + creds + a domain gate).
func dbConfigured() db.OIDCConfig {
	return db.OIDCConfig{
		Enabled: true, EnabledSet: true,
		Issuer: "https://accounts.google.com", IssuerSet: true,
		ClientID: "client-123", ClientIDSet: true,
		ClientSecret: "secret-xyz", ClientSecretSet: true,
		AllowedDomains: "corp.com", AllowedDomainsSet: true,
	}
}

// TestEffectivePrecedence covers env > DB > derived per field + the locked flags.
func TestEffectivePrecedence(t *testing.T) {
	ctx := context.Background()

	t.Run("env-set fields win and are locked", func(t *testing.T) {
		store := &fakeStore{cfg: dbConfigured()}
		p := New(Config{
			Store:  store,
			Domain: fakeDomain{base: "https://kotoji.example.com"},
			Env: config.OIDCConfig{
				Issuer:       "https://env-issuer.example",
				ClientID:     "env-client",
				ClientSecret: "env-secret",
			},
			EnvSet: config.OIDCEnvSet{Issuer: true, ClientID: true, ClientSecret: true},
		})
		eff := p.Resolve(ctx, req())
		if eff.Issuer.Source != SourceEnv || !eff.Issuer.Locked || eff.Issuer.Value != "https://env-issuer.example" {
			t.Fatalf("issuer not env-locked: %+v", eff.Issuer)
		}
		if eff.ClientID.Source != SourceEnv || !eff.ClientID.Locked || eff.ClientID.Value != "env-client" {
			t.Fatalf("clientID not env-locked: %+v", eff.ClientID)
		}
		if eff.ClientSecret.Source != SourceEnv || !eff.ClientSecret.Locked || eff.ClientSecret.Value != "env-secret" {
			t.Fatalf("clientSecret not env-locked: %+v", eff.ClientSecret)
		}
	})

	t.Run("env-empty fields fall through to DB (editable)", func(t *testing.T) {
		store := &fakeStore{cfg: dbConfigured()}
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com"}})
		eff := p.Resolve(ctx, req())
		if eff.ClientID.Source != SourceDB || eff.ClientID.Locked || eff.ClientID.Value != "client-123" {
			t.Fatalf("clientID should be db/editable: %+v", eff.ClientID)
		}
		if eff.Enabled.Source != SourceDB || !eff.Enabled.Value {
			t.Fatalf("enabled should be db/true: %+v", eff.Enabled)
		}
		if len(eff.AllowedDomains.Value) != 1 || eff.AllowedDomains.Value[0] != "corp.com" {
			t.Fatalf("allowed domains: %+v", eff.AllowedDomains)
		}
	})

	t.Run("redirect derives from control base URL when unset", func(t *testing.T) {
		store := &fakeStore{cfg: dbConfigured()}
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com/"}})
		eff := p.Resolve(ctx, req())
		if eff.RedirectURL.Source != SourceDerived || eff.RedirectExplicitlySet {
			t.Fatalf("redirect should be derived: %+v explicit=%v", eff.RedirectURL, eff.RedirectExplicitlySet)
		}
		if eff.RedirectURL.Value != "https://kotoji.example.com/auth/callback" {
			t.Fatalf("derived redirect = %q", eff.RedirectURL.Value)
		}
	})

	t.Run("explicit DB redirect wins over derived", func(t *testing.T) {
		c := dbConfigured()
		c.RedirectURL, c.RedirectURLSet = "https://custom.example/cb", true
		store := &fakeStore{cfg: c}
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com"}})
		eff := p.Resolve(ctx, req())
		if eff.RedirectURL.Source != SourceDB || !eff.RedirectExplicitlySet || eff.RedirectURL.Value != "https://custom.example/cb" {
			t.Fatalf("explicit redirect not honored: %+v", eff.RedirectURL)
		}
	})

	t.Run("DB read cached once until InvalidateCache", func(t *testing.T) {
		store := &fakeStore{cfg: dbConfigured()}
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com"}})
		_ = p.Resolve(ctx, req())
		_ = p.Resolve(ctx, req())
		if store.readCount() != 1 {
			t.Fatalf("expected 1 cached DB read, got %d", store.readCount())
		}
		p.InvalidateCache()
		_ = p.Resolve(ctx, req())
		if store.readCount() != 2 {
			t.Fatalf("expected re-read after invalidate, got %d", store.readCount())
		}
	})
}

// TestProvidersBreakGlass pins decision #2: password is ALWAYS present (break-glass)
// and oidc is added iff effectively usable; the env-pinned set is returned verbatim.
func TestProvidersBreakGlass(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh install (no auth env, oidc off): password only", func(t *testing.T) {
		store := &fakeStore{} // empty DB => oidc disabled
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com"}})
		got := p.Providers(ctx, req())
		if len(got) != 1 || got[0] != "password" {
			t.Fatalf("fresh install providers = %v, want [password]", got)
		}
	})

	t.Run("oidc enabled via DB with creds: oidc + password", func(t *testing.T) {
		store := &fakeStore{cfg: dbConfigured()}
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com"}})
		got := p.Providers(ctx, req())
		if len(got) != 2 || got[0] != "oidc" || got[1] != "password" {
			t.Fatalf("providers = %v, want [oidc password]", got)
		}
	})

	t.Run("oidc enabled but missing secret: NOT usable, password only", func(t *testing.T) {
		c := dbConfigured()
		c.ClientSecret, c.ClientSecretSet = "", false
		store := &fakeStore{cfg: c}
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com"}})
		got := p.Providers(ctx, req())
		if len(got) != 1 || got[0] != "password" {
			t.Fatalf("half-configured providers = %v, want [password]", got)
		}
	})

	t.Run("env-pinned AUTH_MODE returns the env set verbatim", func(t *testing.T) {
		store := &fakeStore{cfg: dbConfigured()}
		p := New(Config{
			Store:        store,
			Domain:       fakeDomain{base: "https://kotoji.example.com"},
			EnvSet:       config.OIDCEnvSet{AuthMode: true},
			EnvAuthModes: []config.AuthMode{config.AuthModeOIDC, config.AuthModePassword},
		})
		got := p.Providers(ctx, req())
		if len(got) != 2 || got[0] != "oidc" || got[1] != "password" {
			t.Fatalf("env-pinned providers = %v", got)
		}
		if !p.AuthModeEnvLocked() {
			t.Fatalf("AuthModeEnvLocked should be true when env-pinned")
		}
	})
}

// TestProviderRebuildOnChange pins the rebuildable cache: the provider is built once
// for a given effective key, reused on subsequent calls, REBUILT when the config
// changes (key differs) or InvalidateProvider is called.
func TestProviderRebuildOnChange(t *testing.T) {
	ctx := context.Background()
	var builds int32
	store := &fakeStore{cfg: dbConfigured()}
	p := New(Config{
		Store:   store,
		Domain:  fakeDomain{base: "https://kotoji.example.com"},
		Builder: countingBuilder(&builds, nil),
	})

	// First use builds once.
	b1, err := p.ProviderFor(ctx, req())
	if err != nil {
		t.Fatalf("build 1: %v", err)
	}
	// Second use with the SAME config reuses the cache (no rebuild).
	b2, err := p.ProviderFor(ctx, req())
	if err != nil {
		t.Fatalf("build 2: %v", err)
	}
	if builds != 1 {
		t.Fatalf("expected 1 build for stable config, got %d", builds)
	}
	if b1.Start("", "", "") != b2.Start("", "", "") {
		t.Fatalf("cache should return the same built provider")
	}

	// Change the client id in the DB + invalidate the DB cache => effective key
	// changes => the NEXT ProviderFor rebuilds.
	store.cfg.ClientID = "client-ROTATED"
	p.InvalidateCache()
	if _, err := p.ProviderFor(ctx, req()); err != nil {
		t.Fatalf("build 3: %v", err)
	}
	if builds != 2 {
		t.Fatalf("config change should rebuild: builds=%d", builds)
	}

	// InvalidateProvider forces a rebuild even with the same config.
	p.InvalidateProvider()
	if _, err := p.ProviderFor(ctx, req()); err != nil {
		t.Fatalf("build 4: %v", err)
	}
	if builds != 3 {
		t.Fatalf("InvalidateProvider should rebuild: builds=%d", builds)
	}
}

// TestProviderForNotConfigured returns ErrOIDCNotConfigured when OIDC is not usable.
func TestProviderForNotConfigured(t *testing.T) {
	ctx := context.Background()
	var builds int32
	store := &fakeStore{} // empty => disabled
	p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com"}, Builder: countingBuilder(&builds, nil)})
	_, err := p.ProviderFor(ctx, req())
	if !errors.Is(err, ErrOIDCNotConfigured) {
		t.Fatalf("want ErrOIDCNotConfigured, got %v", err)
	}
	if builds != 0 {
		t.Fatalf("must not build when not configured: builds=%d", builds)
	}
}

// TestProviderForDiscoveryError surfaces (and caches) a discovery failure; the cache
// is cleared by InvalidateProvider so the admin can retry after a fix.
func TestProviderForDiscoveryError(t *testing.T) {
	ctx := context.Background()
	var builds int32
	discErr := errors.New("issuer unreachable")
	store := &fakeStore{cfg: dbConfigured()}
	p := New(Config{Store: store, Domain: fakeDomain{base: "https://kotoji.example.com"}, Builder: countingBuilder(&builds, discErr)})

	_, err := p.ProviderFor(ctx, req())
	if err == nil || err.Error() != "issuer unreachable" {
		t.Fatalf("want discovery error, got %v", err)
	}
	// The error is cached for the same key (no re-run on the next login).
	_, _ = p.ProviderFor(ctx, req())
	if builds != 1 {
		t.Fatalf("discovery error should be cached: builds=%d", builds)
	}
	// After InvalidateProvider the build is retried.
	p.InvalidateProvider()
	_, _ = p.ProviderFor(ctx, req())
	if builds != 2 {
		t.Fatalf("InvalidateProvider should clear the cached error: builds=%d", builds)
	}
}

// TestAccessGatedAndUsable pins the small effective predicates the save-validation
// and provider list key off.
func TestAccessGatedAndUsable(t *testing.T) {
	ctx := context.Background()
	t.Run("usable requires enabled + id + secret", func(t *testing.T) {
		store := &fakeStore{cfg: dbConfigured()}
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://k"}})
		if !p.Resolve(ctx, req()).OIDCUsable() {
			t.Fatal("fully configured should be usable")
		}
	})
	t.Run("access gate requires at least one allowlist", func(t *testing.T) {
		c := dbConfigured()
		c.AllowedDomains, c.AllowedDomainsSet = "", false
		store := &fakeStore{cfg: c}
		p := New(Config{Store: store, Domain: fakeDomain{base: "https://k"}})
		if p.Resolve(ctx, req()).AccessGated() {
			t.Fatal("no allowlist => not access-gated")
		}
	})
}
