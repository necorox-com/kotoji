package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

// testConfig is a dev-mode (insecure cookies) config used across handler tests.
func testConfig() config.Config {
	return config.Config{
		Env:               config.EnvDevelopment,
		Mode:              config.RunModeControl,
		AuthMode:          config.AuthModeNone,
		BaseDomain:        "hosting.localhost",
		ControlBaseURL:    "http://hosting.localhost:8080",
		SessionCookieName: "kotoji_session",
		SessionTTL:        time.Hour,
		CSRFCookieName:    "kotoji_csrf",
		CookieSecure:      false,
		HandleMinLen:      3,
		HandleMaxLen:      63,
		Zip: config.ZipLimits{
			MaxUploadBytes: 1000,
			MaxTotalBytes:  2000,
			MaxFiles:       10,
			MaxRatio:       100,
			AllowedExt:     []string{".html", ".css"},
		},
	}
}

// router builds a chi router with the Auth routes mounted + SessionAuth so /api/me
// can read the cookie set by login.
func router(a *Auth) http.Handler {
	r := chi.NewRouter()
	r.Use(a.SessionAuth)
	a.RegisterRoutes(r)
	return r
}

func TestPublicConfig(t *testing.T) {
	a, _, _ := newTestAuth(t, &fakeProvider{key: "dev"})
	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var got instanceConfigJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "hosting.localhost", got.BaseDomain)
	require.Equal(t, "none", got.AuthMode)
	require.Equal(t, 3, got.HandleMinLen)
	require.Contains(t, got.ReservedHandles, "draft")
	require.Equal(t, "direct", got.DefaultPublishMode)
	// Default config has the mirror disabled and no token => not advertised.
	require.False(t, got.GithubMirrorEnabled)
}

// TestPublicConfig_GithubMirrorEnabled is the config-plumbing test for the
// instance mirror flag: it is true ONLY when the feature is enabled AND a push
// token is configured (a half-configured instance must report false).
func TestPublicConfig_GithubMirrorEnabled(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		token   string
		want    bool
	}{
		{name: "disabled, no token", enabled: false, token: "", want: false},
		{name: "enabled but no token (half-configured)", enabled: true, token: "", want: false},
		{name: "disabled but token present", enabled: false, token: "ghp_x", want: false},
		{name: "enabled and token present", enabled: true, token: "ghp_x", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := newTestAuth(t, &fakeProvider{key: "dev"})
			a.cfg.GitHub = config.GitHubMirror{Enabled: tc.enabled, Token: tc.token}
			rec := httptest.NewRecorder()
			router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
			require.Equal(t, http.StatusOK, rec.Code)
			var got instanceConfigJSON
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, tc.want, got.GithubMirrorEnabled)
		})
	}
}

func TestMe_Unauthenticated(t *testing.T) {
	a, _, _ := newTestAuth(t, &fakeProvider{key: "dev"})
	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestDevLogin_RoundTrip(t *testing.T) {
	// Dev no-auth: GET /auth/login logs in immediately, sets a session cookie,
	// and /api/me then returns the user.
	a, store, up := newTestAuth(t, &fakeProvider{
		key:         "dev",
		interactive: false,
		exchangeReply: Claims{
			Subject: devSubject, Email: "admin@kotoji.local", Name: "Local Admin", EmailVerified: true,
		},
	})
	// AuthMode must be none for the GET instant-login branch.
	a.cfg.AuthMode = config.AuthModeNone

	h := router(a)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?next=/dashboard", nil))
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/dashboard", rec.Header().Get("Location"))
	require.Equal(t, 1, up.calls)
	require.Equal(t, 1, store.sessionCount())

	// Grab the session cookie and call /api/me.
	sessionCookie := findCookie(rec.Result().Cookies(), a.sessions.CookieName())
	require.NotNil(t, sessionCookie)

	meRec := httptest.NewRecorder()
	meReq := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	meReq.AddCookie(sessionCookie)
	h.ServeHTTP(meRec, meReq)

	require.Equal(t, http.StatusOK, meRec.Code)
	var me meResponse
	require.NoError(t, json.Unmarshal(meRec.Body.Bytes(), &me))
	require.Equal(t, "admin@kotoji.local", me.User.Email)
	require.Equal(t, "none", me.AuthMode)
}

func TestPasswordLogin(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModePassword
	cfg.AdminPassword = "s3cret-pass"
	cfg.AdminEmail = "admin@kotoji.local"

	store := newFakeStore()
	provider, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)

	a := New(cfg, store, provider)
	a.upserter = &fakeUpserter{store: store}
	h := router(a)

	// Wrong password -> 401.
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, postForm("/auth/login", url.Values{"password": {"wrong"}}))
	require.Equal(t, http.StatusUnauthorized, bad.Code)
	require.Equal(t, 0, store.sessionCount())

	// Correct password -> 302 + session created.
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, postForm("/auth/login", url.Values{"password": {"s3cret-pass"}}))
	require.Equal(t, http.StatusFound, ok.Code)
	require.Equal(t, 1, store.sessionCount())
	require.NotNil(t, findCookie(ok.Result().Cookies(), a.sessions.CookieName()))
}

// TestPasswordLogin_DBHashVerifiedBeforeEnv proves the precedence at the HTTP
// layer: with a DB hash present, the DB password logs in and the (superseded) env
// password is rejected.
func TestPasswordLogin_DBHashVerifiedBeforeEnv(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModePassword
	cfg.AdminPassword = "env-pass-1234"
	cfg.AdminEmail = "admin@kotoji.local"

	dbHash, err := bcrypt.GenerateFromPassword([]byte("db-pass-1234"), bcrypt.MinCost)
	require.NoError(t, err)
	store := newFakeStore()
	require.NoError(t, store.SetAdminPasswordHash(context.Background(), string(dbHash)))

	provider, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)
	a := New(cfg, store, provider)
	a.upserter = &fakeUpserter{store: store}
	h := router(a)

	// Env password (now superseded by the DB hash) -> 401.
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, postForm("/auth/login", url.Values{"password": {"env-pass-1234"}}))
	require.Equal(t, http.StatusUnauthorized, bad.Code)

	// DB password -> 302 + session.
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, postForm("/auth/login", url.Values{"password": {"db-pass-1234"}}))
	require.Equal(t, http.StatusFound, ok.Code)
	require.Equal(t, 1, store.sessionCount())
}

func TestOIDCLogin_StartSetsStateAndRedirects(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC
	fp := &fakeProvider{key: "oidc", interactive: true, startURL: "https://idp.example.com/auth?x=1"}
	store := newFakeStore()
	a := New(cfg, store, fp)
	a.upserter = &fakeUpserter{store: store}

	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?next=/sites", nil))

	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://idp.example.com/auth?x=1", rec.Header().Get("Location"))
	// The state cookie must be set and the provider must have received the same state.
	stateCookie := findCookie(rec.Result().Cookies(), a.loginStateCookieName())
	require.NotNil(t, stateCookie)
	require.NotEmpty(t, fp.gotState)
	require.NotEmpty(t, fp.gotNonce)
	require.NotEmpty(t, fp.gotVerifier)
}

func TestOIDCCallback_StateMismatchRejected(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC
	fp := &fakeProvider{key: "oidc", interactive: true, startURL: "https://idp/auth"}
	store := newFakeStore()
	a := New(cfg, store, fp)
	a.upserter = &fakeUpserter{store: store}
	h := router(a)

	// Start to obtain a valid state cookie.
	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	stateCookie := findCookie(startRec.Result().Cookies(), a.loginStateCookieName())
	require.NotNil(t, stateCookie)

	// Callback with a WRONG state value -> 403, no session.
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state=WRONG", nil)
	cbReq.AddCookie(stateCookie)
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)

	require.Equal(t, http.StatusForbidden, cbRec.Code)
	require.Equal(t, 0, store.sessionCount())
}

func TestOIDCCallback_MissingStateCookieRejected(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC
	a := New(cfg, newFakeStore(), &fakeProvider{key: "oidc", interactive: true})
	a.upserter = &fakeUpserter{store: newFakeStore()}

	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state=x", nil))
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestOIDCCallback_SuccessRotatesSession(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC
	fp := &fakeProvider{
		key: "oidc", interactive: true, startURL: "https://idp/auth",
		exchangeReply: Claims{Subject: "sub-123", Email: "alice@corp.com", Name: "Alice", EmailVerified: true},
	}
	store := newFakeStore()
	a := New(cfg, store, fp)
	up := &fakeUpserter{store: store}
	a.upserter = up
	h := router(a)

	// 1. Start -> state cookie.
	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, httptest.NewRequest(http.MethodGet, "/auth/login?next=/dashboard", nil))
	stateCookie := findCookie(startRec.Result().Cookies(), a.loginStateCookieName())
	require.NotNil(t, stateCookie)
	state := fp.gotState

	// 2. Pre-existing (attacker-fixed) session cookie should NOT be reused. We
	//    capture the post-login session id and assert it differs from any input.
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=good&state="+url.QueryEscape(state), nil)
	cbReq.AddCookie(stateCookie)
	cbReq.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: "attacker-fixed-id"})
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)

	require.Equal(t, http.StatusFound, cbRec.Code)
	require.Equal(t, "/dashboard", cbRec.Header().Get("Location"))
	require.Equal(t, 1, up.calls)
	require.Equal(t, "sub-123", up.lastInput.Subject)
	require.Equal(t, "alice@corp.com", up.lastInput.Email)

	newSession := findCookie(cbRec.Result().Cookies(), a.sessions.CookieName())
	require.NotNil(t, newSession)
	require.NotEqual(t, "attacker-fixed-id", newSession.Value, "session id must be rotated on login")
	require.Equal(t, 1, store.sessionCount())

	// The freshly-set cookie must resolve to a live session for the user.
	su, err := a.sessions.Get(context.Background(), newSession.Value)
	require.NoError(t, err)
	require.Equal(t, "alice@corp.com", su.Email)
}

func TestOIDCCallback_ExchangeRejected(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC
	fp := &fakeProvider{
		key: "oidc", interactive: true, startURL: "https://idp/auth",
		exchangeErr: errors.New("allowlist reject"),
	}
	store := newFakeStore()
	a := New(cfg, store, fp)
	a.upserter = &fakeUpserter{store: store}
	h := router(a)

	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	stateCookie := findCookie(startRec.Result().Cookies(), a.loginStateCookieName())
	state := fp.gotState

	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=good&state="+url.QueryEscape(state), nil)
	cbReq.AddCookie(stateCookie)
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)

	// A provider Exchange failure (e.g. allowlist reject) -> 403, no session.
	require.Equal(t, http.StatusForbidden, cbRec.Code)
	require.Equal(t, 0, store.sessionCount())
}

func TestLogout(t *testing.T) {
	a, store, _ := newTestAuth(t, &fakeProvider{key: "dev"})
	u := activeUser(t, false)
	store.addUser(u)
	sid, err := a.sessions.Create(context.Background(), u.ID, "ua", "")
	require.NoError(t, err)
	require.Equal(t, 1, store.sessionCount())

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: sid})
	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, 0, store.sessionCount())
	// Cookie must be cleared (MaxAge < 0).
	cleared := findCookie(rec.Result().Cookies(), a.sessions.CookieName())
	require.NotNil(t, cleared)
	require.True(t, cleared.MaxAge < 0)
}

func TestSafeNext(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "/dashboard"},
		{"/sites/abc", "/sites/abc"},
		{"//evil.com", "/dashboard"},
		{"http://evil.com", "/dashboard"},
		{"https://evil.com", "/dashboard"},
		{"relative", "/dashboard"},
	}
	for _, tc := range tests {
		require.Equal(t, tc.want, safeNext(tc.in), "next=%q", tc.in)
	}
}

// --- helpers ---

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func postForm(target string, vals url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}
