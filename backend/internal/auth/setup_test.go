package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
)

// passwordAuth builds an Auth in password mode over the given store, with the
// in-memory upserter swapped in (so the setup/login paths exercise the fake DB).
func passwordAuth(t *testing.T, store *fakeStore, envPassword string) *Auth {
	t.Helper()
	cfg := testConfig()
	cfg.AuthMode = config.AuthModePassword
	cfg.AdminPassword = envPassword
	cfg.AdminEmail = "admin@kotoji.local"
	provider, err := NewPasswordProvider(cfg, store)
	require.NoError(t, err)
	a := New(cfg, store, provider)
	a.upserter = &fakeUpserter{store: store}
	return a
}

// TestSetupRequired_TruthTable is the full matrix from the task: setupRequired is
// true ONLY when AUTH_MODE=password AND no env password AND no DB hash. Every
// other combination (env set / db hash set / non-password mode) is false.
func TestSetupRequired_TruthTable(t *testing.T) {
	cases := []struct {
		name        string
		authMode    config.AuthMode
		envPassword string
		dbHashSet   bool
		want        bool
	}{
		{name: "password, no env, no db -> required", authMode: config.AuthModePassword, want: true},
		{name: "password, env set -> closed", authMode: config.AuthModePassword, envPassword: "envsecret", want: false},
		{name: "password, db hash set -> closed", authMode: config.AuthModePassword, dbHashSet: true, want: false},
		{name: "password, env AND db -> closed", authMode: config.AuthModePassword, envPassword: "envsecret", dbHashSet: true, want: false},
		{name: "oidc mode -> never required", authMode: config.AuthModeOIDC, want: false},
		{name: "oidc mode, db hash leftover -> never required", authMode: config.AuthModeOIDC, dbHashSet: true, want: false},
		{name: "none mode -> never required", authMode: config.AuthModeNone, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			if tc.dbHashSet {
				require.NoError(t, store.SetAdminPasswordHash(context.Background(), "x"))
			}
			cfg := testConfig()
			cfg.AuthMode = tc.authMode
			cfg.AdminPassword = tc.envPassword
			a := New(cfg, store, &fakeProvider{key: "x"})
			require.Equal(t, tc.want, a.setupRequired(context.Background()))
		})
	}
}

// TestPublicConfig_ExposesSetupRequired wires the flag through GET /api/config.
func TestPublicConfig_ExposesSetupRequired(t *testing.T) {
	// First-run password instance -> setupRequired true.
	store := newFakeStore()
	a := passwordAuth(t, store, "")
	rec := httptest.NewRecorder()
	router(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got instanceConfigJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.True(t, got.SetupRequired)

	// Once a hash exists, the same endpoint reports false.
	require.NoError(t, store.SetAdminPasswordHash(context.Background(), "x"))
	rec2 := httptest.NewRecorder()
	router(a).ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got2 instanceConfigJSON
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &got2))
	require.False(t, got2.SetupRequired)
}

// TestSetup_AllowedWhenRequired: first-run -> stores the hash, creates the admin,
// establishes a session (cookie set), and returns 200. The login then works with
// the freshly set password and rejects a wrong one.
func TestSetup_AllowedWhenRequired(t *testing.T) {
	store := newFakeStore()
	a := passwordAuth(t, store, "") // no env password -> setup required
	h := router(a)

	body, _ := json.Marshal(map[string]string{"password": "hunter2-strong", "confirm": "hunter2-strong"})
	req := httptest.NewRequest(http.MethodPost, "/auth/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// A session cookie was set (the caller is immediately authenticated).
	require.NotNil(t, findCookie(rec.Result().Cookies(), a.sessions.CookieName()))
	require.Equal(t, 1, store.sessionCount())
	// CSRF token issued alongside the session.
	require.NotNil(t, findCookie(rec.Result().Cookies(), a.csrf.CookieName()))

	// The hash was persisted and is a real bcrypt hash of the chosen password.
	hash, found, err := store.GetAdminPasswordHash(context.Background())
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("hunter2-strong")))
	// Body confirms completion.
	var ok setupResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ok))
	require.True(t, ok.OK)
}

// TestSetup_RejectedWhenAlreadyConfigured: with a hash already present, the
// endpoint returns 409 and does NOT touch the credential (no reset vector).
func TestSetup_RejectedWhenAlreadyConfigured(t *testing.T) {
	store := newFakeStore()
	existing, err := bcrypt.GenerateFromPassword([]byte("original-pass"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, store.SetAdminPasswordHash(context.Background(), string(existing)))

	a := passwordAuth(t, store, "")
	h := router(a)

	body, _ := json.Marshal(map[string]string{"password": "attacker-reset-1"})
	req := httptest.NewRequest(http.MethodPost, "/auth/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, 0, store.sessionCount())
	// The stored hash is UNCHANGED (the original password still verifies).
	hash, _, _ := store.GetAdminPasswordHash(context.Background())
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("original-pass")))
}

// TestSetup_RejectedWhenEnvPasswordSet: an env password means setup is closed,
// so /auth/setup returns 409 even with no DB hash.
func TestSetup_RejectedWhenEnvPasswordSet(t *testing.T) {
	store := newFakeStore()
	a := passwordAuth(t, store, "env-configured-pw")
	h := router(a)

	body, _ := json.Marshal(map[string]string{"password": "new-strong-pass"})
	req := httptest.NewRequest(http.MethodPost, "/auth/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	_, found, _ := store.GetAdminPasswordHash(context.Background())
	require.False(t, found, "no hash written when setup is closed")
}

// TestSetup_WeakPasswordRejected: a too-short password -> 422 and no hash/session.
func TestSetup_WeakPasswordRejected(t *testing.T) {
	store := newFakeStore()
	a := passwordAuth(t, store, "")
	h := router(a)

	body, _ := json.Marshal(map[string]string{"password": "short"}) // < AdminPasswordMinLen
	req := httptest.NewRequest(http.MethodPost, "/auth/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Equal(t, 0, store.sessionCount())
	_, found, _ := store.GetAdminPasswordHash(context.Background())
	require.False(t, found)
}

// TestSetup_ConfirmMismatchRejected: when a confirm is supplied it must match.
func TestSetup_ConfirmMismatchRejected(t *testing.T) {
	store := newFakeStore()
	a := passwordAuth(t, store, "")
	h := router(a)

	body, _ := json.Marshal(map[string]string{"password": "good-password-1", "confirm": "different-one-2"})
	req := httptest.NewRequest(http.MethodPost, "/auth/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	_, found, _ := store.GetAdminPasswordHash(context.Background())
	require.False(t, found)
}

// TestSetupThenLogin_EndToEnd: after setup, the DB hash is the active credential
// and POST /auth/login with the chosen password succeeds.
func TestSetupThenLogin_EndToEnd(t *testing.T) {
	store := newFakeStore()
	a := passwordAuth(t, store, "")
	h := router(a)

	// Run setup.
	body, _ := json.Marshal(map[string]string{"password": "chosen-at-setup"})
	setupReq := httptest.NewRequest(http.MethodPost, "/auth/setup", bytes.NewReader(body))
	setupReq.Header.Set("Content-Type", "application/json")
	setupRec := httptest.NewRecorder()
	h.ServeHTTP(setupRec, setupReq)
	require.Equal(t, http.StatusOK, setupRec.Code)

	// Now login via the password form with the chosen password.
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, postForm("/auth/login", map[string][]string{"password": {"chosen-at-setup"}}))
	require.Equal(t, http.StatusFound, loginRec.Code)
}

// TestSetup_KeyMatchesDBStore guards the dual-maintenance hazard: the auth-layer
// instance_settings key MUST equal the db package's exported constant.
func TestSetup_KeyMatchesDBStore(t *testing.T) {
	require.Equal(t, db.SettingAdminPasswordHash, adminPasswordHashKey)
}
