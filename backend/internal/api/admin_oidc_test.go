package api

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
)

// TestAdminOIDCConfig covers the /api/admin/oidc contract: admin-only access, the
// secret-safe GET/PUT shapes (the client secret is NEVER echoed), the write-only
// secret semantics, fail-closed save validation (422), env-locked rejection (409),
// the effective provider list, and cache invalidation on a successful PUT.
func TestAdminOIDCConfig(t *testing.T) {
	t.Run("anonymous GET is unauthenticated", func(t *testing.T) {
		e := newTestEnv(t)
		rec := e.request(http.MethodGet, "/api/admin/oidc").do()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("non-admin GET is forbidden", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		rec := e.request(http.MethodGet, "/api/admin/oidc").as(u).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("non-admin PUT is forbidden and writes nothing", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(u).
			json(map[string]any{"clientSecret": "s"}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if len(e.store.setOIDCInputs) != 0 {
			t.Fatalf("non-admin PUT must not write config")
		}
	})

	t.Run("admin GET on a fresh install: disabled, derived redirect, password-only", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodGet, "/api/admin/oidc").as(admin).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got oidcAdminConfig
		decodeBody(t, rec, &got)
		if got.Enabled || got.ClientIDSet || got.ClientSecretSet {
			t.Fatalf("fresh install should be unconfigured: %+v", got)
		}
		// The redirect derives from the request control base URL (httptest: example.com).
		if got.RedirectURLEffective != "http://example.com/auth/callback" {
			t.Fatalf("derived redirect = %q", got.RedirectURLEffective)
		}
		if got.RedirectURL != "" {
			t.Fatalf("configured redirect should be empty when derived: %q", got.RedirectURL)
		}
		if len(got.Providers) != 1 || got.Providers[0] != "password" {
			t.Fatalf("providers = %v, want [password]", got.Providers)
		}
	})

	t.Run("admin GET never echoes the client secret", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		e.store.oidc = db.OIDCConfig{
			Enabled: true, EnabledSet: true,
			ClientID: "client-xyz", ClientIDSet: true,
			ClientSecret: "TOP-SECRET-VALUE", ClientSecretSet: true,
			AllowedDomains: "corp.com", AllowedDomainsSet: true,
		}
		rec := e.request(http.MethodGet, "/api/admin/oidc").as(admin).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "TOP-SECRET-VALUE") {
			t.Fatalf("response leaked the client secret: %s", rec.Body.String())
		}
		var got oidcAdminConfig
		decodeBody(t, rec, &got)
		if !got.ClientSecretSet {
			t.Fatalf("clientSecretSet should be true")
		}
		if got.ClientID != "client-xyz" || !got.ClientIDSet {
			t.Fatalf("clientId should be surfaced: %+v", got)
		}
		// Effective provider list now includes oidc.
		if len(got.Providers) != 2 || got.Providers[0] != "oidc" {
			t.Fatalf("providers = %v, want [oidc password]", got.Providers)
		}
	})

	t.Run("PUT full config enables OIDC, persists, and invalidates caches", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		// Prime the provider cache so we can prove invalidation occurs.
		_ = e.request(http.MethodGet, "/api/admin/oidc").as(admin).do()

		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{
				"enabled":        true,
				"issuer":         "https://accounts.google.com",
				"clientId":       "client-abc",
				"clientSecret":   "secret-abc",
				"allowedDomains": []string{"corp.com"},
				"adminEmails":    []string{"boss@corp.com"},
			}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got oidcAdminConfig
		decodeBody(t, rec, &got)
		if !got.Enabled || !got.ClientSecretSet || got.ClientID != "client-abc" {
			t.Fatalf("post-PUT config wrong: %+v", got)
		}
		if got.EnabledSource != "db" {
			t.Fatalf("enabled source = %q, want db", got.EnabledSource)
		}
		// The secret was stored (write path) but never returned.
		if strings.Contains(rec.Body.String(), "secret-abc") {
			t.Fatalf("PUT response leaked the secret: %s", rec.Body.String())
		}
		if !e.store.oidc.ClientSecretSet || e.store.oidc.ClientSecret != "secret-abc" {
			t.Fatalf("secret not persisted: %+v", e.store.oidc)
		}
		// allowedDomains stored as CSV.
		if e.store.oidc.AllowedDomains != "corp.com" {
			t.Fatalf("allowed domains CSV = %q", e.store.oidc.AllowedDomains)
		}
	})

	t.Run("PUT empty secret KEEPS the existing one (write-only)", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		e.store.oidc = db.OIDCConfig{
			Enabled: true, EnabledSet: true,
			ClientID: "c", ClientIDSet: true,
			ClientSecret: "keep-me", ClientSecretSet: true,
			AllowedDomains: "corp.com", AllowedDomainsSet: true,
		}
		// Re-save with no secret field => keeps it.
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{"issuer": "https://idp.example"}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if e.store.oidc.ClientSecret != "keep-me" {
			t.Fatalf("empty secret must keep existing: %q", e.store.oidc.ClientSecret)
		}
	})

	t.Run("PUT clearClientSecret removes it", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		e.store.oidc = db.OIDCConfig{
			ClientID: "c", ClientIDSet: true,
			ClientSecret: "gone", ClientSecretSet: true,
		}
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{"clearClientSecret": true}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if e.store.oidc.ClientSecretSet {
			t.Fatalf("clearClientSecret should remove the secret")
		}
	})

	t.Run("enabling OIDC without credentials is 422 (fail-closed)", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{
				"enabled":        true,
				"allowedDomains": []string{"corp.com"},
			}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
		if len(e.store.setOIDCInputs) != 0 {
			t.Fatalf("rejected PUT must not write")
		}
	})

	t.Run("enabling OIDC without an allowlist is 422 (fail-closed)", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{
				"enabled":      true,
				"clientId":     "c",
				"clientSecret": "s",
				// NO allowedEmails / allowedDomains => fail-closed reject.
			}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
		env := errEnvelope(t, rec)
		if env.Error.Code != codeValidation {
			t.Fatalf("error code = %q, want validation", env.Error.Code)
		}
		if len(e.store.setOIDCInputs) != 0 {
			t.Fatalf("rejected PUT must not write")
		}
	})

	t.Run("enable succeeds when credentials+gate already configured in DB", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		// Pre-configure creds + gate but disabled; enabling alone should now pass.
		e.store.oidc = db.OIDCConfig{
			ClientID: "c", ClientIDSet: true,
			ClientSecret: "s", ClientSecretSet: true,
			AllowedEmails: "vip@corp.com", AllowedEmailsSet: true,
		}
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{"enabled": true}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if !e.store.oidc.Enabled {
			t.Fatalf("enabled not persisted")
		}
	})

	t.Run("PUT invalid redirect URL is 422", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{"redirectUrl": "not-a-url"}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// S2 (SSRF): the issuer is fetched server-side by discovery, so an internal /
	// non-https / IP-literal issuer must be rejected at the admin-write boundary and
	// MUST NOT be persisted.
	t.Run("PUT internal/non-https issuer is 422 and not written", func(t *testing.T) {
		bad := []string{
			"http://accounts.example.com",    // non-https
			"https://localhost/issuer",       // loopback name
			"https://127.0.0.1",              // loopback IP literal
			"https://169.254.169.254/latest", // link-local metadata IP
			"https://10.0.0.5",               // private IP literal
			"https://[::1]",                  // IPv6 loopback literal
			"file:///etc/passwd",             // non-http scheme
			"https://foo.localhost",          // .localhost suffix
		}
		for _, iss := range bad {
			e := newTestEnv(t)
			admin := e.newUser(withAdmin)
			rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
				json(map[string]any{"issuer": iss}).do()
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("issuer %q: status = %d, want 422 (body=%s)", iss, rec.Code, rec.Body.String())
			}
			env := errEnvelope(t, rec)
			if env.Error.Code != codeValidation {
				t.Fatalf("issuer %q: error code = %q, want validation", iss, env.Error.Code)
			}
			if len(e.store.setOIDCInputs) != 0 {
				t.Fatalf("issuer %q: rejected PUT must not write", iss)
			}
		}
	})

	t.Run("PUT valid https public issuer is accepted", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{"issuer": "https://accounts.google.com"}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if e.store.oidc.Issuer != "https://accounts.google.com" {
			t.Fatalf("valid issuer not persisted: %q", e.store.oidc.Issuer)
		}
	})

	t.Run("PUT to an env-locked field is 409", func(t *testing.T) {
		// KOTOJI_OIDC_CLIENT_ID env-set => clientId locked.
		e := newTestEnvWithConfig(t, func(c *configForTest) {
			c.OIDCEnvSet = config.OIDCEnvSet{ClientID: true}
			c.EnvOIDC = config.OIDCConfig{ClientID: "env-client"}
		})
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{"clientId": "override"}).do()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
		if len(e.store.setOIDCInputs) != 0 {
			t.Fatalf("locked-field PUT must not write")
		}
	})

	t.Run("PUT enabled while AUTH_MODE env-pinned is 409", func(t *testing.T) {
		e := newTestEnvWithConfig(t, func(c *configForTest) {
			c.AuthModeEnvSet = true
			c.EnvAuthModes = []config.AuthMode{config.AuthModeOIDC, config.AuthModePassword}
		})
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{"enabled": false}).do()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty PUT body writes nothing", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if len(e.store.setOIDCInputs) != 0 {
			t.Fatalf("no-field PUT must not write")
		}
	})

	t.Run("disabling OIDC needs no credentials/allowlist", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		// Even with nothing configured, disabling is always allowed (no fail-closed).
		rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
			json(map[string]any{"enabled": false}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestAdminOIDCProviderRebuild pins that a successful PUT invalidates the built
// provider cache so a subsequent login rebuilds discovery (decision #3). It asserts
// via the testOIDCProvider's build counter that InvalidateProvider was called.
func TestAdminOIDCProviderRebuild(t *testing.T) {
	e := newTestEnv(t)
	admin := e.newUser(withAdmin)
	e.store.oidc = db.OIDCConfig{
		Enabled: true, EnabledSet: true,
		ClientID: "c", ClientIDSet: true,
		ClientSecret: "s", ClientSecretSet: true,
		AllowedDomains: "corp.com", AllowedDomainsSet: true,
	}
	// Build once (force the cache to populate).
	if _, err := e.oidc.p.ProviderFor(e.ctx(), e.bgReq()); err != nil {
		t.Fatalf("prime build: %v", err)
	}
	buildsBefore := e.oidc.builds

	rec := e.request(http.MethodPut, "/api/admin/oidc").as(admin).
		json(map[string]any{"adminEmails": []string{"boss@corp.com"}}).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// After the PUT the provider cache was invalidated; a fresh build runs again.
	if _, err := e.oidc.p.ProviderFor(e.ctx(), e.bgReq()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if e.oidc.builds <= buildsBefore {
		t.Fatalf("PUT should have invalidated the provider cache (builds %d -> %d)", buildsBefore, e.oidc.builds)
	}
}

// TestAdminOIDCDiscoveryValidate exercises ValidateDiscovery surfacing a discovery
// error (the pre-flight path the frontend can use before enabling).
func TestAdminOIDCDiscoveryValidate(t *testing.T) {
	e := newTestEnvWithConfig(t, func(c *configForTest) {
		c.OIDCDiscoveryErr = errors.New("issuer unreachable")
	})
	e.store.oidc = db.OIDCConfig{
		Enabled: true, EnabledSet: true,
		ClientID: "c", ClientIDSet: true,
		ClientSecret: "s", ClientSecretSet: true,
		AllowedDomains: "corp.com", AllowedDomainsSet: true,
	}
	if err := e.oidc.ValidateDiscovery(e.ctx(), e.bgReq()); err == nil {
		t.Fatal("ValidateDiscovery should surface the discovery error")
	}
}

// TestValidateOIDCIssuer is the pure S2 issuer validator: https-only, hostname
// (not IP literal), and not an internal/loopback/private host.
func TestValidateOIDCIssuer(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "valid public https", in: "https://accounts.google.com", wantErr: false},
		{name: "valid public https with path", in: "https://login.microsoftonline.com/common/v2.0", wantErr: false},
		{name: "http rejected", in: "http://accounts.google.com", wantErr: true},
		{name: "loopback name rejected", in: "https://localhost", wantErr: true},
		{name: "dot-localhost rejected", in: "https://idp.localhost", wantErr: true},
		{name: "loopback ipv4 literal rejected", in: "https://127.0.0.1", wantErr: true},
		{name: "metadata ip rejected", in: "https://169.254.169.254/latest/meta-data", wantErr: true},
		{name: "private ipv4 literal rejected", in: "https://10.1.2.3", wantErr: true},
		{name: "private ipv4 172 literal rejected", in: "https://172.16.0.1", wantErr: true},
		{name: "ipv6 loopback literal rejected", in: "https://[::1]", wantErr: true},
		{name: "public ip literal still rejected (must be a hostname)", in: "https://8.8.8.8", wantErr: true},
		{name: "file scheme rejected", in: "file:///etc/passwd", wantErr: true},
		{name: "no host rejected", in: "https://", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := validateOIDCIssuer(tc.in)
			if tc.wantErr && reason == "" {
				t.Fatalf("validateOIDCIssuer(%q) = ok, want a rejection reason", tc.in)
			}
			if !tc.wantErr && reason != "" {
				t.Fatalf("validateOIDCIssuer(%q) = %q, want ok", tc.in, reason)
			}
		})
	}
}

// TestIsGlobalUnicast pins the internal-IP predicate used by both the issuer
// validator and (mirrored) the discovery dialer.
func TestIsGlobalUnicast(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{ip: "8.8.8.8", want: true},
		{ip: "1.1.1.1", want: true},
		{ip: "2606:4700:4700::1111", want: true},
		{ip: "127.0.0.1", want: false},
		{ip: "::1", want: false},
		{ip: "169.254.169.254", want: false}, // link-local (cloud metadata)
		{ip: "10.0.0.1", want: false},
		{ip: "192.168.1.1", want: false},
		{ip: "172.16.5.4", want: false},
		{ip: "fc00::1", want: false},   // ULA
		{ip: "0.0.0.0", want: false},   // unspecified
		{ip: "224.0.0.1", want: false}, // multicast
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", tc.ip)
		}
		if got := isGlobalUnicast(ip); got != tc.want {
			t.Fatalf("isGlobalUnicast(%s) = %v want %v", tc.ip, got, tc.want)
		}
	}
}
