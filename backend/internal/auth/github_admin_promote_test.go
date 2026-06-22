package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
)

// TestPublicConfig_GithubMirrorEnabled_DBOverridesEnv proves the EFFECTIVE flag
// folds the DB config over env on both the enabled and token axes.
func TestPublicConfig_GithubMirrorEnabled_DBOverridesEnv(t *testing.T) {
	cases := []struct {
		name string
		env  config.GitHubMirror
		dbc  db.GitHubConfig
		want bool
	}{
		{
			name: "env enabled+token, DB silent -> env wins (true)",
			env:  config.GitHubMirror{Enabled: true, Token: "ghp_env"},
			dbc:  db.GitHubConfig{},
			want: true,
		},
		{
			name: "env disabled, DB enables + has token -> true",
			env:  config.GitHubMirror{Enabled: false, Token: ""},
			dbc:  db.GitHubConfig{Enabled: true, EnabledSet: true, TokenSet: true},
			want: true,
		},
		{
			name: "env enabled+token, DB explicitly disables -> false",
			env:  config.GitHubMirror{Enabled: true, Token: "ghp_env"},
			dbc:  db.GitHubConfig{Enabled: false, EnabledSet: true},
			want: false,
		},
		{
			name: "DB enables but no token anywhere -> false (half-configured)",
			env:  config.GitHubMirror{Enabled: false, Token: ""},
			dbc:  db.GitHubConfig{Enabled: true, EnabledSet: true, TokenSet: false},
			want: false,
		},
		{
			name: "env token only, DB enables -> true (token from env)",
			env:  config.GitHubMirror{Enabled: false, Token: "ghp_env"},
			dbc:  db.GitHubConfig{Enabled: true, EnabledSet: true},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, store, _ := newTestAuth(t, &fakeProvider{key: "dev"})
			a.cfg.GitHub = tc.env
			store.githubCfg = tc.dbc

			rec := httptest.NewRecorder()
			router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
			require.Equal(t, http.StatusOK, rec.Code)

			var got instanceConfigJSON
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, tc.want, got.GithubMirrorEnabled)
		})
	}
}

// TestPromoteAdmin_OnPasswordLogin: a successful password login promotes the
// admin user to is_admin (single-admin mode => the admin IS the instance admin).
func TestPromoteAdmin_OnPasswordLogin(t *testing.T) {
	store := newFakeStore()
	a := passwordAuth(t, store, "s3cret-pass")
	h := router(a)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postForm("/auth/login", url.Values{"password": {"s3cret-pass"}}))
	require.Equal(t, http.StatusFound, rec.Code)

	// The single upserted user must have been promoted exactly once.
	require.Len(t, store.users, 1)
	for id := range store.users {
		require.Equal(t, 1, store.promotedCount(id), "password login must promote the admin")
		require.True(t, store.users[id].IsAdmin)
	}
}

// TestPromoteAdmin_NotOnOIDC: an OIDC login must NEVER promote (IdP users are
// governed by the admin screen, not auto-promoted).
func TestPromoteAdmin_NotOnOIDC(t *testing.T) {
	cfg := testConfig()
	cfg.AuthMode = config.AuthModeOIDC
	fp := &fakeProvider{
		key: "oidc", interactive: true, startURL: "https://idp/auth",
		exchangeReply: Claims{Subject: "sub-1", Email: "alice@corp.com", Name: "Alice", EmailVerified: true},
	}
	store := newFakeStore()
	a := New(cfg, store, fp)
	a.upserter = &fakeUpserter{store: store}
	h := router(a)

	// Start -> state cookie, then callback with the matching state.
	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, httptest.NewRequest(http.MethodGet, "/auth/login?next=/dashboard", nil))
	stateCookie := findCookie(startRec.Result().Cookies(), a.loginStateCookieName())
	require.NotNil(t, stateCookie)

	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=good&state="+url.QueryEscape(fp.gotState), nil)
	cbReq.AddCookie(stateCookie)
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)
	require.Equal(t, http.StatusFound, cbRec.Code)

	// No promotion happened, and the user is not an admin.
	require.Len(t, store.users, 1)
	for id := range store.users {
		require.Equal(t, 0, store.promotedCount(id), "oidc login must NOT promote")
		require.False(t, store.users[id].IsAdmin)
	}
}

// TestPromoteAdmin_OnSetup: first-run setup promotes the freshly-created admin so
// /settings + the admin API are reachable immediately.
func TestPromoteAdmin_OnSetup(t *testing.T) {
	store := newFakeStore()
	a := passwordAuth(t, store, "") // no env password -> setup required
	h := router(a)

	body, _ := json.Marshal(map[string]string{"password": "hunter2-strong"})
	req := httptest.NewRequest(http.MethodPost, "/auth/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Len(t, store.users, 1)
	for id := range store.users {
		require.GreaterOrEqual(t, store.promotedCount(id), 1, "setup must promote the admin")
		require.True(t, store.users[id].IsAdmin)
	}
}
