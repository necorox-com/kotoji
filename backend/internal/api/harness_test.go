package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/domaincfg"
	"github.com/necorox-com/kotoji/backend/internal/oidccfg"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// testEnv bundles everything a handler test needs: the assembled router plus the
// fakes it can seed/assert against.
type testEnv struct {
	t        *testing.T
	router   http.Handler
	svc      *site.FakeService
	store    *fakeMetaStore
	sessions *fakeSessionStore
	cfg      config.Config
	domain   *testDomainProvider
	oidc     *testOIDCProvider
}

// testConfig is the minimal config for the API surface in tests: dev auth, no
// secure cookies (so un-prefixed cookie names work over http), small handle
// bounds, a tiny upload cap for the guard tests.
func testConfig() config.Config {
	return config.Config{
		Env:                config.EnvDevelopment,
		Mode:               config.RunModeControl,
		AuthMode:           config.AuthModeNone,
		BaseDomain:         "hosting.localhost",
		ControlBaseURL:     "http://hosting.localhost:8080",
		SessionCookieName:  "kotoji_session",
		SessionTTL:         time.Hour,
		CSRFCookieName:     "kotoji_csrf",
		CookieSecure:       false,
		MCPPath:            "/mcp",
		CORSAllowedOrigins: []string{"http://hosting.localhost:8080"},
		HandleMinLen:       3,
		HandleMaxLen:       63,
		Zip: config.ZipLimits{
			MaxUploadBytes: 1 << 20, // 1 MiB cap for upload-guard tests
			MaxTotalBytes:  1 << 21,
			MaxFiles:       100,
			MaxRatio:       100,
			AllowedExt:     []string{".html", ".css", ".js", ".png"},
		},
	}
}

// newTestEnv assembles the router with the REAL auth middleware (the only thing
// that can populate auth.CurrentUser) over fake stores + the FakeService.
func newTestEnv(t *testing.T) *testEnv {
	return newTestEnvWith(t, nil)
}

// configForTest is the narrow set of config knobs a test may override (kept tiny
// so the harness does not leak the full config surface). The GitHub fields back
// the admin-github "env fallback" tests; the domain env flags back the admin-
// domain "env-locked" tests (env > DB precedence).
type configForTest struct {
	GitHubEnabled bool
	GitHubToken   string
	GitHubOrg     string

	// BaseDomainEnvSet / ControlBaseURLEnvSet simulate KOTOJI_BASE_DOMAIN /
	// KOTOJI_CONTROL_BASE_URL being explicitly set (locked) for the admin-domain
	// tests. When false the domain provider resolves the DB/derived value.
	BaseDomainEnvSet     bool
	ControlBaseURLEnvSet bool
	// BaseDomain / ControlBaseURL override the static env values used by the domain
	// provider (defaults come from testConfig when these are empty).
	BaseDomain     string
	ControlBaseURL string

	// OIDC env-set knobs for the admin-oidc "env-locked" tests. AuthModeEnvSet
	// simulates KOTOJI_AUTH_MODE being pinned; the per-field flags + values simulate
	// KOTOJI_OIDC_* being set (so the field is read-only). EnvAuthModes is the env set
	// the AuthMode lock applies (e.g. oidc,password).
	AuthModeEnvSet bool
	EnvAuthModes   []config.AuthMode
	OIDCEnvSet     config.OIDCEnvSet
	EnvOIDC        config.OIDCConfig
	// OIDCDiscoveryErr forces the fake OIDC builder to fail (issuer unreachable) so
	// the discovery-failure paths can be exercised.
	OIDCDiscoveryErr error
}

// newTestEnvWithConfig builds a testEnv after applying mutate to a configForTest,
// then folding those overrides onto the base testConfig.
func newTestEnvWithConfig(t *testing.T, mutate func(*configForTest)) *testEnv {
	t.Helper()
	c := configForTest{}
	if mutate != nil {
		mutate(&c)
	}
	return newTestEnvWith(t, &c)
}

// newTestEnvWith is the shared assembler; overrides may be nil (defaults).
func newTestEnvWith(t *testing.T, overrides *configForTest) *testEnv {
	t.Helper()
	cfg := testConfig()
	if overrides != nil {
		cfg.GitHub.Enabled = overrides.GitHubEnabled
		cfg.GitHub.Token = overrides.GitHubToken
		cfg.GitHub.Org = overrides.GitHubOrg
		if overrides.BaseDomain != "" {
			cfg.BaseDomain = overrides.BaseDomain
		}
		if overrides.ControlBaseURL != "" {
			cfg.ControlBaseURL = overrides.ControlBaseURL
		}
		cfg.BaseDomainEnvSet = overrides.BaseDomainEnvSet
		cfg.ControlBaseURLEnvSet = overrides.ControlBaseURLEnvSet
		cfg.OIDCEnvSet = overrides.OIDCEnvSet
		cfg.OIDCEnvSet.AuthMode = overrides.AuthModeEnvSet
		cfg.OIDC = overrides.EnvOIDC
		if len(overrides.EnvAuthModes) > 0 {
			cfg.AuthModes = overrides.EnvAuthModes
		}
	}
	svc := site.NewFakeService()
	store := newFakeMetaStore()
	sessions := newFakeSessionStore()

	provider := auth.NewDevProvider(cfg)
	authSvc := auth.New(cfg, sessions, provider)

	// Domain provider over the fake store (env flags from cfg). Wrapped by a tiny
	// test adapter so it satisfies api.DomainConfigProvider without importing the
	// app package's adapter (which would cycle).
	domain := newTestDomainProvider(cfg, store)
	authSvc.SetDomainResolver(domain)

	// OIDC provider over the fake store + a FAKE builder (no network discovery): the
	// builder yields a stub BuiltProvider, or fails when OIDCDiscoveryErr is set. It
	// satisfies api.OIDCConfigProvider without importing the app adapter (cycle).
	var discoErr error
	if overrides != nil {
		discoErr = overrides.OIDCDiscoveryErr
	}
	oidc := newTestOIDCProvider(cfg, store, domain, discoErr)

	router := NewRouter(Deps{
		Config: cfg,
		Site:   svc,
		Store:  store,
		Auth:   WrapAuth(authSvc),
		Domain: domain,
		OIDC:   oidc,
	})

	return &testEnv{t: t, router: router, svc: svc, store: store, sessions: sessions, cfg: cfg, domain: domain, oidc: oidc}
}

// testDomainProvider wraps a real *domaincfg.Provider and translates its Resolved
// onto the api boundary type, so it satisfies BOTH api.DomainConfigProvider (the
// admin handler) and auth.DomainResolver (/api/config) without importing the app
// package's adapter (which would create a test import cycle).
type testDomainProvider struct{ p *domaincfg.Provider }

func newTestDomainProvider(cfg config.Config, store domaincfg.Store) *testDomainProvider {
	return &testDomainProvider{p: domaincfg.New(domaincfg.Config{
		Store:             store,
		EnvBaseDomain:     cfg.BaseDomain,
		EnvBaseDomainSet:  cfg.BaseDomainEnvSet,
		EnvControlBaseURL: cfg.ControlBaseURL,
		EnvControlSet:     cfg.ControlBaseURLEnvSet,
	})}
}

func (d *testDomainProvider) Resolve(ctx context.Context, r *http.Request) DomainResolved {
	res := d.p.Resolve(ctx, r)
	return DomainResolved{
		BaseDomain:     DomainEffective{Value: res.BaseDomain.Value, Source: string(res.BaseDomain.Source), Locked: res.BaseDomain.Locked},
		ControlBaseURL: DomainEffective{Value: res.ControlBaseURL.Value, Source: string(res.ControlBaseURL.Source), Locked: res.ControlBaseURL.Locked},
	}
}

func (d *testDomainProvider) EnvBaseDomainLocked() bool     { return d.p.EnvBaseDomainLocked() }
func (d *testDomainProvider) EnvControlBaseURLLocked() bool { return d.p.EnvControlBaseURLLocked() }
func (d *testDomainProvider) InvalidateCache()              { d.p.InvalidateCache() }
func (d *testDomainProvider) BaseDomainFor(r *http.Request) string {
	return d.p.BaseDomainFor(r)
}
func (d *testDomainProvider) ControlBaseURLFor(r *http.Request) string {
	return d.p.ControlBaseURLFor(r)
}

// testOIDCProvider wraps a real *oidccfg.Provider (over the fake store + a FAKE
// discovery builder) and translates its EffectiveOIDC onto the api boundary type, so
// it satisfies api.OIDCConfigProvider without importing the app adapter (cycle). The
// builder records build attempts so the rebuild-cache tests can assert discovery ran.
type testOIDCProvider struct {
	p        *oidccfg.Provider
	builds   int
	discoErr error
}

// fakeBuiltProvider is a stub oidccfg.BuiltProvider the test builder yields (no
// network). Its Exchange is a no-op success — the admin OIDC tests exercise the
// CONFIG/cache paths, not the OIDC handshake (that is covered in internal/auth).
type fakeBuiltProvider struct{ key string }

func (f fakeBuiltProvider) Key() string                 { return f.key }
func (f fakeBuiltProvider) Interactive() bool           { return true }
func (f fakeBuiltProvider) Start(_, _, _ string) string { return "https://idp/auth" }
func (f fakeBuiltProvider) Exchange(context.Context, string, string, string) (oidccfg.Claims, error) {
	return oidccfg.Claims{}, nil
}

func newTestOIDCProvider(cfg config.Config, store oidccfg.Store, domain oidccfg.DomainResolver, discoErr error) *testOIDCProvider {
	tp := &testOIDCProvider{discoErr: discoErr}
	tp.p = oidccfg.New(oidccfg.Config{
		Store:  store,
		Domain: domain,
		Builder: func(_ context.Context, _ oidccfg.EffectiveOIDC) (oidccfg.BuiltProvider, error) {
			tp.builds++
			if tp.discoErr != nil {
				return nil, tp.discoErr
			}
			return fakeBuiltProvider{key: "oidc"}, nil
		},
		Env:          cfg.OIDC,
		EnvSet:       cfg.OIDCEnvSet,
		EnvAuthModes: cfg.AuthModes,
	})
	return tp
}

func (o *testOIDCProvider) Resolve(ctx context.Context, r *http.Request) OIDCResolved {
	eff := o.p.Resolve(ctx, r)
	return OIDCResolved{
		Enabled:         OIDCEffectiveBool{Value: eff.Enabled.Value, Source: string(eff.Enabled.Source), Locked: eff.Enabled.Locked},
		Issuer:          OIDCEffectiveString{Value: eff.Issuer.Value, Source: string(eff.Issuer.Source), Locked: eff.Issuer.Locked},
		ClientID:        OIDCEffectiveString{Value: eff.ClientID.Value, Source: string(eff.ClientID.Source), Locked: eff.ClientID.Locked},
		ClientSecretSet: eff.ClientSecret.Value != "",
		ClientSecretSrc: string(eff.ClientSecret.Source),
		ClientSecretLck: eff.ClientSecret.Locked,
		RedirectURL:     OIDCEffectiveString{Value: eff.RedirectURL.Value, Source: string(eff.RedirectURL.Source), Locked: eff.RedirectURL.Locked},
		AllowedEmails:   OIDCEffectiveList{Value: eff.AllowedEmails.Value, Source: string(eff.AllowedEmails.Source), Locked: eff.AllowedEmails.Locked},
		AllowedDomains:  OIDCEffectiveList{Value: eff.AllowedDomains.Value, Source: string(eff.AllowedDomains.Source), Locked: eff.AllowedDomains.Locked},
		AdminEmails:     OIDCEffectiveList{Value: eff.AdminEmails.Value, Source: string(eff.AdminEmails.Source), Locked: eff.AdminEmails.Locked},
	}
}

func (o *testOIDCProvider) Providers(ctx context.Context, r *http.Request) []string {
	return o.p.Providers(ctx, r)
}
func (o *testOIDCProvider) AuthModeEnvLocked() bool { return o.p.AuthModeEnvLocked() }
func (o *testOIDCProvider) ValidateDiscovery(ctx context.Context, r *http.Request) error {
	return o.p.ValidateDiscovery(ctx, o.p.Resolve(ctx, r))
}
func (o *testOIDCProvider) InvalidateCache()    { o.p.InvalidateCache() }
func (o *testOIDCProvider) InvalidateProvider() { o.p.InvalidateProvider() }

// ctx / bgReq are tiny helpers for tests that drive the OIDC provider directly
// (outside the router) to assert the rebuild cache.
func (e *testEnv) ctx() context.Context { return context.Background() }
func (e *testEnv) bgReq() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	r.Host = "example.com"
	return r
}

// user creates a seeded user with a live session and returns it plus the session
// cookie value. The user is registered in BOTH the session store (so the auth
// middleware authenticates the cookie) and the meta store (so role/member/admin
// lookups resolve).
type testUser struct {
	rec       gen.User
	sessionID string
}

func (e *testEnv) newUser(opts ...func(*gen.User)) testUser {
	e.t.Helper()
	u := gen.User{
		ID:             uuid.New(),
		Email:          uuid.NewString() + "@example.com",
		DisplayName:    "Test User",
		IsActive:       true,
		CanCreateSites: true,
		CreatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	for _, o := range opts {
		o(&u)
	}
	sid := uuid.NewString()
	e.sessions.seed(sid, u)
	e.store.addUser(u)
	return testUser{rec: u, sessionID: sid}
}

// withAdmin marks a created user as an instance superuser.
func withAdmin(u *gen.User) { u.IsAdmin = true }

// withNoCreate revokes the site-creation capability.
func withNoCreate(u *gen.User) { u.CanCreateSites = false }

// createSite seeds a site owned by owner (via the FakeService) and records the
// owner's role in the meta store. Extra members can be added with setRole.
func (e *testEnv) createSite(handle string, owner testUser) site.Site {
	return e.createSiteWith(handle, owner)
}

// createSiteWith is createSite with optional CreateSiteInput mutators (e.g. to
// set publish_mode/visibility on the Service-owned record).
func (e *testEnv) createSiteWith(handle string, owner testUser, opts ...func(*site.CreateSiteInput)) site.Site {
	e.t.Helper()
	in := site.CreateSiteInput{
		Handle:  site.Handle(handle),
		OwnerID: owner.rec.ID,
		Actor:   site.Actor{UserID: owner.rec.ID, Name: owner.rec.DisplayName, Email: owner.rec.Email, Via: site.SourceEditor},
	}
	for _, o := range opts {
		o(&in)
	}
	st, err := e.svc.CreateSite(context.Background(), in)
	if err != nil {
		e.t.Fatalf("seed site %q: %v", handle, err)
	}
	e.store.setRole(st.ID, owner.rec.ID, gen.SiteRoleOwner)
	return st
}

// ---- request helpers ----

// req is a fluent request builder that attaches auth + CSRF as configured.
type req struct {
	env    *testEnv
	method string
	path   string
	body   io.Reader
	ctype  string
	user   *testUser
	csrf   bool
}

func (e *testEnv) request(method, path string) *req {
	return &req{env: e, method: method, path: path, csrf: true}
}

func (r *req) as(u testUser) *req { r.user = &u; return r }

func (r *req) json(v any) *req {
	b, err := json.Marshal(v)
	if err != nil {
		r.env.t.Fatalf("marshal body: %v", err)
	}
	r.body = bytes.NewReader(b)
	r.ctype = "application/json"
	return r
}

func (r *req) raw(body io.Reader, ctype string) *req {
	r.body = body
	r.ctype = ctype
	return r
}

// noCSRF disables attaching the CSRF cookie+header (to exercise the guard).
func (r *req) noCSRF() *req { r.csrf = false; return r }

// do executes the request against the router and returns the recorder.
func (r *req) do() *httptest.ResponseRecorder {
	r.env.t.Helper()
	httpReq := httptest.NewRequest(r.method, r.path, r.body)
	if r.ctype != "" {
		httpReq.Header.Set("Content-Type", r.ctype)
	}
	if r.user != nil {
		httpReq.AddCookie(&http.Cookie{Name: r.env.cfg.SessionCookieName, Value: r.user.sessionID})
	}
	// Attach a matching CSRF cookie + header for mutating requests (the double-
	// submit guard requires them on non-safe methods for cookie auth).
	if r.csrf {
		const tok = "test-csrf-token"
		httpReq.AddCookie(&http.Cookie{Name: r.env.cfg.CSRFCookieName, Value: tok})
		httpReq.Header.Set("X-CSRF-Token", tok)
	}
	rec := httptest.NewRecorder()
	r.env.router.ServeHTTP(rec, httpReq)
	return rec
}

// decodeBody unmarshals a JSON response body into dst, failing the test on error.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
}

// errEnvelope decodes the response into an error envelope and returns the code.
func errEnvelope(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	decodeBody(t, rec, &env)
	return env
}

// mustJSON marshals v to bytes or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
