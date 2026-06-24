package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/oidccfg"
)

// stubOIDCRuntime is a programmable OIDCRuntime: it returns a provided interactive
// provider (or an error) and a fixed effective provider list, so the runtime-seam
// login/config paths can be exercised without oidccfg + go-oidc.
type stubOIDCRuntime struct {
	prov      AuthProvider
	provErr   error
	providers []string
	enabled   bool
}

func (s *stubOIDCRuntime) InteractiveProvider(_ context.Context, _ *http.Request) (AuthProvider, error) {
	if s.provErr != nil {
		return nil, s.provErr
	}
	return s.prov, nil
}
func (s *stubOIDCRuntime) Providers(_ context.Context, _ *http.Request) []string { return s.providers }
func (s *stubOIDCRuntime) OIDCEnabledEffective(_ context.Context, _ *http.Request) bool {
	return s.enabled
}

// runtimeBreakGlassAuth wires an Auth with the RUNTIME OIDC seam (a stub) plus a
// real password provider — exactly the zero-env composition (AUTH_MODE unset =>
// password default + runtime-enabled oidc).
func runtimeBreakGlassAuth(t *testing.T, oidc *fakeProvider, providers []string, envPassword string) (*Auth, *fakeStore) {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthModePassword
	cfg.AuthModes = []config.AuthMode{config.AuthModePassword}
	cfg.AdminEmail = "admin@kotoji.local"
	cfg.AdminPassword = envPassword

	store := newFakeStore()
	pw, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)

	a := NewWithProviders(cfg, store, pw)
	a.upserter = &fakeUpserter{store: store}
	a.SetOIDCRuntime(&stubOIDCRuntime{prov: oidc, providers: providers, enabled: oidc != nil})
	return a, store
}

// TestRuntime_ConfigAdvertisesEffectiveProviders: /api/config.authProviders reflects
// the EFFECTIVE set from the runtime seam (oidc + password break-glass), not the
// static config (which is password-only).
func TestRuntime_ConfigAdvertisesEffectiveProviders(t *testing.T) {
	oidc := &fakeProvider{key: oidcProviderKey, interactive: true, startURL: "https://idp/auth"}
	a, _ := runtimeBreakGlassAuth(t, oidc, []string{"oidc", "password"}, "break-glass-pw-1234")

	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got instanceConfigJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"oidc", "password"}, got.AuthProviders)
}

// TestRuntime_OIDCStartRedirectsViaRuntime: GET /auth/login drives the runtime OIDC
// provider (the primary human path) and sets the login-state cookie.
func TestRuntime_OIDCStartRedirectsViaRuntime(t *testing.T) {
	oidc := &fakeProvider{key: oidcProviderKey, interactive: true, startURL: "https://idp.example/auth?z=1"}
	a, _ := runtimeBreakGlassAuth(t, oidc, []string{"oidc", "password"}, "break-glass-pw-1234")
	h := router(a)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?next=/dashboard", nil))
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://idp.example/auth?z=1", rec.Header().Get("Location"))
	require.NotEmpty(t, oidc.gotState)
	require.NotNil(t, findCookie(rec.Result().Cookies(), a.loginStateCookieName()))
}

// TestRuntime_PasswordBreakGlassAlwaysWorks: even with OIDC enabled via the runtime
// seam, the password POST login still authenticates (decision #2: enabling OIDC
// never removes the break-glass).
func TestRuntime_PasswordBreakGlassAlwaysWorks(t *testing.T) {
	oidc := &fakeProvider{key: oidcProviderKey, interactive: true, startURL: "https://idp/auth"}
	a, store := runtimeBreakGlassAuth(t, oidc, []string{"oidc", "password"}, "break-glass-pw-1234")
	h := router(a)

	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, postForm("/auth/login", url.Values{"password": {"break-glass-pw-1234"}}))
	require.Equal(t, http.StatusFound, ok.Code)
	require.Equal(t, 1, store.sessionCount())
}

// TestRuntime_OIDCDisabledFallsBackToPasswordPost: when the runtime seam reports OIDC
// not configured, GET /auth/login returns 400 (POST instead) and the password POST
// still works — i.e. a fresh install boots password-first-run.
func TestRuntime_OIDCDisabledFallsBackToPasswordPost(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModePassword
	cfg.AuthModes = []config.AuthMode{config.AuthModePassword}
	cfg.AdminPassword = "only-password-1234"
	store := newFakeStore()
	pw, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)
	a := NewWithProviders(cfg, store, pw)
	a.upserter = &fakeUpserter{store: store}
	// Runtime seam present but OIDC NOT configured (ErrOIDCNotConfigured).
	a.SetOIDCRuntime(&stubOIDCRuntime{provErr: oidccfg.ErrOIDCNotConfigured, providers: []string{"password"}})
	h := router(a)

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	require.Equal(t, http.StatusBadRequest, get.Code, "no GET-capable provider => 400 (POST instead)")

	post := httptest.NewRecorder()
	h.ServeHTTP(post, postForm("/auth/login", url.Values{"password": {"only-password-1234"}}))
	require.Equal(t, http.StatusFound, post.Code)
	require.Equal(t, 1, store.sessionCount())

	// /api/config advertises password only.
	cfgRec := httptest.NewRecorder()
	h.ServeHTTP(cfgRec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got instanceConfigJSON
	require.NoError(t, json.Unmarshal(cfgRec.Body.Bytes(), &got))
	require.Equal(t, []string{"password"}, got.AuthProviders)
}

// TestRuntime_OIDCDiscoveryErrorSurfaces: a discovery failure from the runtime seam
// becomes a clear 502 on GET /auth/login (not a crash); the password POST still works.
func TestRuntime_OIDCDiscoveryErrorSurfaces(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModePassword
	cfg.AuthModes = []config.AuthMode{config.AuthModePassword}
	cfg.AdminPassword = "break-glass-pw-1234"
	store := newFakeStore()
	pw, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)
	a := NewWithProviders(cfg, store, pw)
	a.upserter = &fakeUpserter{store: store}
	a.SetOIDCRuntime(&stubOIDCRuntime{provErr: errors.New("issuer unreachable"), providers: []string{"oidc", "password"}})
	h := router(a)

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	require.Equal(t, http.StatusBadGateway, get.Code)

	// Break-glass still works after a broken-OIDC GET.
	post := httptest.NewRecorder()
	h.ServeHTTP(post, postForm("/auth/login", url.Values{"password": {"break-glass-pw-1234"}}))
	require.Equal(t, http.StatusFound, post.Code)
	require.Equal(t, 1, store.sessionCount())
}

// TestRuntime_CallbackUsesRuntimeProvider: /auth/callback completes login via the
// runtime OIDC provider (admin-email claim promotes the user; decision #3).
func TestRuntime_CallbackUsesRuntimeProvider(t *testing.T) {
	oidc := &fakeProvider{
		key:           oidcProviderKey,
		interactive:   true,
		startURL:      "https://idp/auth",
		exchangeReply: Claims{Subject: "g-1", Email: "boss@corp.com", Name: "Boss", EmailVerified: true, IsAdmin: true},
	}
	a, store := runtimeBreakGlassAuth(t, oidc, []string{"oidc", "password"}, "break-glass-pw-1234")
	h := router(a)

	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, httptest.NewRequest(http.MethodGet, "/auth/login?next=/dashboard", nil))
	stateCookie := findCookie(startRec.Result().Cookies(), a.loginStateCookieName())
	require.NotNil(t, stateCookie)

	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=good&state="+url.QueryEscape(oidc.gotState), nil)
	cbReq.AddCookie(stateCookie)
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)
	require.Equal(t, http.StatusFound, cbRec.Code)

	require.Len(t, store.users, 1)
	for id := range store.users {
		require.Equal(t, 1, store.promotedCount(id), "admin-email OIDC login must promote")
		require.True(t, store.users[id].IsAdmin)
	}
}
