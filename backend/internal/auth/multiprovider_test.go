package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

// breakGlassConfig returns a dev-cookie config with BOTH oidc and password
// enabled (the locked break-glass decision).
func breakGlassConfig() config.Config {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC // legacy representative
	cfg.AuthModes = []config.AuthMode{config.AuthModeOIDC, config.AuthModePassword}
	cfg.AdminEmail = "admin@kotoji.local"
	return cfg
}

// newBreakGlassAuth wires an Auth with a fake interactive OIDC provider AND a real
// password provider over the same fake store, exactly as composition would for
// AUTH_MODE="oidc,password".
func newBreakGlassAuth(t *testing.T, oidcReply Claims, oidcErr error, envPassword string) (*Auth, *fakeStore, *fakeProvider) {
	t.Helper()
	cfg := breakGlassConfig()
	cfg.AdminPassword = envPassword

	store := newFakeStore()
	oidc := &fakeProvider{
		key:           oidcProviderKey,
		interactive:   true,
		startURL:      "https://idp.example.com/auth?x=1",
		exchangeReply: oidcReply,
		exchangeErr:   oidcErr,
	}
	pw, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)

	a := NewWithProviders(cfg, store, oidc, pw)
	a.upserter = &fakeUpserter{store: store}
	return a, store, oidc
}

// TestMultiProvider_PasswordStillWorksWhenOIDCEnabled is the headline coexistence
// guarantee: with oidc + password enabled, the password break-glass POST login
// still authenticates (OIDC being on must NOT disable it).
func TestMultiProvider_PasswordStillWorksWhenOIDCEnabled(t *testing.T) {
	a, store, _ := newBreakGlassAuth(t, Claims{}, nil, "break-glass-pw-1234")
	h := router(a)

	// Wrong password -> 401.
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, postForm("/auth/login", url.Values{"password": {"nope"}}))
	require.Equal(t, http.StatusUnauthorized, bad.Code)
	require.Equal(t, 0, store.sessionCount())

	// Correct break-glass password -> 302 + session, even though OIDC is enabled.
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, postForm("/auth/login", url.Values{"password": {"break-glass-pw-1234"}}))
	require.Equal(t, http.StatusFound, ok.Code)
	require.Equal(t, 1, store.sessionCount())
}

// TestMultiProvider_OIDCStartStillRedirectsWhenPasswordEnabled: GET /auth/login
// drives the interactive OIDC provider (the primary human path) even though the
// password provider is also enabled.
func TestMultiProvider_OIDCStartStillRedirectsWhenPasswordEnabled(t *testing.T) {
	a, _, oidc := newBreakGlassAuth(t, Claims{}, nil, "break-glass-pw-1234")
	h := router(a)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?next=/sites", nil))
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://idp.example.com/auth?x=1", rec.Header().Get("Location"))
	require.NotEmpty(t, oidc.gotState)
	require.NotNil(t, findCookie(rec.Result().Cookies(), a.loginStateCookieName()))
}

// TestMultiProvider_ConfigExposesBothProviders: /api/config advertises the full
// enabled set in authProviders so the login page renders each one, while keeping
// the legacy single authMode for back-compat.
func TestMultiProvider_ConfigExposesBothProviders(t *testing.T) {
	a, _, _ := newBreakGlassAuth(t, Claims{}, nil, "break-glass-pw-1234")
	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got instanceConfigJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "oidc", got.AuthMode, "legacy representative stays oidc")
	require.Equal(t, []string{"oidc", "password"}, got.AuthProviders)
}

// TestMultiProvider_OIDCAdminEmailPromotesAtLogin: when the verified OIDC email
// is an admin email, the callback promotes the user (decision #3) — and the
// identity is linked under the OIDC provider key, not password.
func TestMultiProvider_OIDCAdminEmailPromotesAtLogin(t *testing.T) {
	a, store, oidc := newBreakGlassAuth(t,
		Claims{Subject: "g-1", Email: "boss@corp.com", Name: "Boss", EmailVerified: true, IsAdmin: true},
		nil, "break-glass-pw-1234")
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

// TestMultiProvider_OIDCNonAdminEmailNotPromoted: an allowed-but-not-admin OIDC
// user is upserted as a normal user (claims.IsAdmin=false => no promotion).
func TestMultiProvider_OIDCNonAdminEmailNotPromoted(t *testing.T) {
	a, store, oidc := newBreakGlassAuth(t,
		Claims{Subject: "g-2", Email: "worker@corp.com", Name: "Worker", EmailVerified: true, IsAdmin: false},
		nil, "break-glass-pw-1234")
	h := router(a)

	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	stateCookie := findCookie(startRec.Result().Cookies(), a.loginStateCookieName())

	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=good&state="+url.QueryEscape(oidc.gotState), nil)
	cbReq.AddCookie(stateCookie)
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)
	require.Equal(t, http.StatusFound, cbRec.Code)

	require.Len(t, store.users, 1)
	for id := range store.users {
		require.Equal(t, 0, store.promotedCount(id), "non-admin OIDC login must NOT promote")
		require.False(t, store.users[id].IsAdmin)
	}
}

// TestMultiProvider_OIDCDeniedNoSession: an OIDC Exchange that returns an
// access-policy rejection yields 403 and creates NO session (the password path is
// unaffected and remains usable).
func TestMultiProvider_OIDCDeniedNoSession(t *testing.T) {
	a, store, oidc := newBreakGlassAuth(t, Claims{}, ErrNotAllowed, "break-glass-pw-1234")
	h := router(a)

	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	stateCookie := findCookie(startRec.Result().Cookies(), a.loginStateCookieName())

	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=good&state="+url.QueryEscape(oidc.gotState), nil)
	cbReq.AddCookie(stateCookie)
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)

	require.Equal(t, http.StatusForbidden, cbRec.Code)
	require.Equal(t, 0, store.sessionCount())

	// And break-glass still works after a denied OIDC attempt.
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, postForm("/auth/login", url.Values{"password": {"break-glass-pw-1234"}}))
	require.Equal(t, http.StatusFound, ok.Code)
	require.Equal(t, 1, store.sessionCount())
}

// TestOIDCOnly_PasswordPostRejected: with ONLY oidc enabled there is no
// non-interactive provider, so POST /auth/login returns 400 (use the redirect),
// while GET /auth/login redirects to the IdP. This pins that the two paths are
// independently gated by which providers are enabled.
func TestOIDCOnly_PasswordPostRejected(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC
	cfg.AuthModes = []config.AuthMode{config.AuthModeOIDC}
	store := newFakeStore()
	oidc := &fakeProvider{key: oidcProviderKey, interactive: true, startURL: "https://idp/auth"}
	a := NewWithProviders(cfg, store, oidc)
	a.upserter = &fakeUpserter{store: store}
	h := router(a)

	// POST password -> 400 (no non-interactive provider).
	post := httptest.NewRecorder()
	h.ServeHTTP(post, postForm("/auth/login", url.Values{"password": {"x"}}))
	require.Equal(t, http.StatusBadRequest, post.Code)

	// GET -> 302 to the IdP.
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	require.Equal(t, http.StatusFound, get.Code)
}

// TestPasswordOnly_GetLoginTellsCallerToPost: with ONLY password enabled, GET
// /auth/login has no GET-capable provider and returns 400 (POST instead), without
// disabling the POST path.
func TestPasswordOnly_GetLoginTellsCallerToPost(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModePassword
	cfg.AuthModes = []config.AuthMode{config.AuthModePassword}
	cfg.AdminPassword = "only-password-1234"
	store := newFakeStore()
	pw, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)
	a := NewWithProviders(cfg, store, pw)
	a.upserter = &fakeUpserter{store: store}
	h := router(a)

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	require.Equal(t, http.StatusBadRequest, get.Code)

	// POST still works.
	post := httptest.NewRecorder()
	h.ServeHTTP(post, postForm("/auth/login", url.Values{"password": {"only-password-1234"}}))
	require.Equal(t, http.StatusFound, post.Code)
}
