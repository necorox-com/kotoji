package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SINGLE-DOMAIN ISOLATION REGRESSION SUITE.
//
// kotoji serves hosted sites on subdomains of the SAME registrable domain as the
// control plane (one DNS wildcard — a deliberate operational choice). A hosted
// subdomain can therefore set a bare-name cookie on the shared parent domain
// ("cookie tossing"). These tests pin the single guarantee that closes the
// session/CSRF attack surface of that model: in PRODUCTION (CookieSecure=true)
// the control plane reads ONLY the `__Host-`-prefixed cookie and is BLIND to any
// tossed bare-name cookie, so a tossed cookie can neither shadow a real session
// nor fixate the CSRF token. In DEV (http) the bare name is still accepted
// because browsers reject `__Host-` without Secure.

// bareSessionName / bareCSRFName are the un-prefixed names a malicious hosted
// subdomain would toss onto the shared parent domain. They match the config
// defaults so the test reflects the real wire names an attacker would guess.
const (
	bareSessionName = "kotoji_session"
	bareCSRFName    = "kotoji_csrf"
)

// --- Session read path -------------------------------------------------------

func TestSession_ReadCookie_IgnoresTossedBareNameInProd(t *testing.T) {
	// Production manager: secure=true => on-the-wire name is __Host-kotoji_session.
	m := NewSessionManager(newFakeStore(), bareSessionName, time.Hour, true)
	require.Equal(t, "__Host-"+bareSessionName, m.CookieName())

	t.Run("tossed bare cookie alone is ignored", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		// Attacker tosses ONLY the bare-name cookie (no __Host- one).
		r.AddCookie(&http.Cookie{Name: bareSessionName, Value: "attacker-fixed-sid"})

		got := m.readCookie(r)
		require.Empty(t, got, "prod must not read the tossed bare-name session cookie")
	})

	t.Run("legit __Host- cookie wins even when a conflicting bare cookie is present", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		// Attacker tosses a conflicting bare cookie; the real client also carries the
		// legitimate __Host- cookie. The control plane must read the __Host- value.
		r.AddCookie(&http.Cookie{Name: bareSessionName, Value: "attacker-fixed-sid"})
		r.AddCookie(&http.Cookie{Name: "__Host-" + bareSessionName, Value: "real-sid"})

		got := m.readCookie(r)
		require.Equal(t, "real-sid", got, "the un-tossable __Host- cookie must be the one read")
	})

	t.Run("legit __Host- cookie alone is read", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		r.AddCookie(&http.Cookie{Name: "__Host-" + bareSessionName, Value: "real-sid"})

		require.Equal(t, "real-sid", m.readCookie(r))
	})
}

func TestSession_ReadCookie_AcceptsBareNameInDev(t *testing.T) {
	// Dev manager: secure=false => on-the-wire name is the bare name because
	// browsers reject __Host- over http. The bare cookie is the legitimate one.
	m := NewSessionManager(newFakeStore(), bareSessionName, time.Hour, false)
	require.Equal(t, bareSessionName, m.CookieName())

	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.AddCookie(&http.Cookie{Name: bareSessionName, Value: "dev-sid"})

	require.Equal(t, "dev-sid", m.readCookie(r), "dev must accept the bare-name session cookie")
}

// SessionAuth (the middleware) must end up anonymous when only a tossed bare
// cookie is present in prod: the tossed value is never looked up, so no user is
// loaded and RequireAuth downstream returns 401.
func TestSessionAuth_TossedBareCookieStaysAnonymousInProd(t *testing.T) {
	store := newFakeStore()
	u := activeUser(t, false)
	store.addUser(u)
	m := NewSessionManager(store, bareSessionName, time.Hour, true)
	// Seed a REAL session id in the store, then toss a cookie under the BARE name
	// whose value is that very id. If the server (wrongly) read the bare name, it
	// would resolve to a live session — proving the toss "worked". It must not.
	realID, err := m.Create(t.Context(), u.ID, "ua", "")
	require.NoError(t, err)

	a := &Auth{sessions: m}
	// RequireAuth after SessionAuth: protected, returns 401 when anonymous.
	h := a.SessionAuth(RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	t.Run("tossed bare cookie carrying a real id is still rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
		req.AddCookie(&http.Cookie{Name: bareSessionName, Value: realID}) // tossed
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code,
			"a tossed bare-name cookie must not authenticate in prod")
	})

	t.Run("the legitimate __Host- cookie authenticates", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
		req.AddCookie(&http.Cookie{Name: "__Host-" + bareSessionName, Value: realID})
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code,
			"the un-tossable __Host- cookie must authenticate normally")
	})
}

// --- CSRF double-submit verify path ------------------------------------------

func TestCSRF_Verify_IgnoresTossedBareCookieInProd(t *testing.T) {
	// Prod CSRF guard: reads only __Host-kotoji_csrf.
	c := NewCSRF(bareCSRFName, true)
	require.Equal(t, "__Host-"+bareCSRFName, c.CookieName())

	t.Run("attacker fixates CSRF via tossed bare cookie -> verify fails", func(t *testing.T) {
		// The attacker tosses a bare-name CSRF cookie with a value they also place in
		// the X-CSRF-Token header (a forged double-submit). Because the server reads
		// ONLY the __Host- cookie — which is absent here — the cookie side is empty
		// and verification fails. The toss cannot satisfy double-submit.
		r := httptest.NewRequest(http.MethodPost, "/api/sites", nil)
		r.AddCookie(&http.Cookie{Name: bareCSRFName, Value: "forged-token"})
		r.Header.Set(csrfHeader, "forged-token")

		require.False(t, c.Verify(r), "prod must not honor a tossed bare-name CSRF cookie")
	})

	t.Run("tossed bare cookie cannot override the real __Host- token", func(t *testing.T) {
		// Real client carries the legitimate __Host- CSRF cookie + matching header.
		// An attacker-tossed bare cookie with a different value must not shadow it:
		// the server reads the __Host- value, which matches the header => passes.
		r := httptest.NewRequest(http.MethodPost, "/api/sites", nil)
		r.AddCookie(&http.Cookie{Name: bareCSRFName, Value: "attacker-token"})
		r.AddCookie(&http.Cookie{Name: "__Host-" + bareCSRFName, Value: "real-token"})
		r.Header.Set(csrfHeader, "real-token")

		require.True(t, c.Verify(r), "the __Host- token must drive double-submit, not the tossed one")
	})

	t.Run("mismatched header against real __Host- token fails", func(t *testing.T) {
		// Sanity: a tossed bare cookie's value in the header cannot pass against the
		// real __Host- cookie value.
		r := httptest.NewRequest(http.MethodPost, "/api/sites", nil)
		r.AddCookie(&http.Cookie{Name: bareCSRFName, Value: "attacker-token"})
		r.AddCookie(&http.Cookie{Name: "__Host-" + bareCSRFName, Value: "real-token"})
		r.Header.Set(csrfHeader, "attacker-token")

		require.False(t, c.Verify(r))
	})
}

func TestCSRF_Verify_AcceptsBareCookieInDev(t *testing.T) {
	// Dev: __Host- impossible over http, so the bare name is the legitimate one.
	c := NewCSRF(bareCSRFName, false)
	require.Equal(t, bareCSRFName, c.CookieName())

	r := httptest.NewRequest(http.MethodPost, "/api/sites", nil)
	r.AddCookie(&http.Cookie{Name: bareCSRFName, Value: "dev-token"})
	r.Header.Set(csrfHeader, "dev-token")

	require.True(t, c.Verify(r), "dev must accept the bare-name CSRF cookie for double-submit")
}

// --- Host-only / no-Domain regression (Task 2) -------------------------------

// Every kotoji-set control cookie MUST be host-only: no Domain attribute, and the
// `__Host-` prefix in prod (which a browser only accepts when Secure + Path=/ +
// no Domain). This pins that NONE of kotoji's auth/CSRF cookies depend on a
// parent-domain (Domain=.example.com) cookie — the property that makes them
// un-tossable across sibling subdomains.
func TestControlCookies_AreHostOnly_NoDomain_Prod(t *testing.T) {
	t.Run("session SetCookie", func(t *testing.T) {
		m := NewSessionManager(newFakeStore(), bareSessionName, time.Hour, true)
		rec := httptest.NewRecorder()
		m.SetCookie(rec, "sid")
		c := lastCookieNamed(t, rec, "__Host-"+bareSessionName)
		require.Empty(t, c.Domain, "session cookie must be host-only (no Domain)")
		require.True(t, c.Secure)
		require.Equal(t, "/", c.Path)
	})

	t.Run("session ClearCookie", func(t *testing.T) {
		m := NewSessionManager(newFakeStore(), bareSessionName, time.Hour, true)
		rec := httptest.NewRecorder()
		m.ClearCookie(rec)
		c := lastCookieNamed(t, rec, "__Host-"+bareSessionName)
		require.Empty(t, c.Domain, "session clear cookie must be host-only (no Domain)")
		require.True(t, c.Secure)
	})

	t.Run("CSRF Issue", func(t *testing.T) {
		c := NewCSRF(bareCSRFName, true)
		rec := httptest.NewRecorder()
		_, err := c.Issue(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
		require.NoError(t, err)
		ck := lastCookieNamed(t, rec, "__Host-"+bareCSRFName)
		require.Empty(t, ck.Domain, "CSRF cookie must be host-only (no Domain)")
		require.True(t, ck.Secure)
		require.Equal(t, "/", ck.Path)
		require.False(t, ck.HttpOnly, "CSRF cookie stays readable for the SPA double-submit")
	})

	t.Run("CSRF clearCookie", func(t *testing.T) {
		c := NewCSRF(bareCSRFName, true)
		rec := httptest.NewRecorder()
		c.clearCookie(rec)
		ck := lastCookieNamed(t, rec, "__Host-"+bareCSRFName)
		require.Empty(t, ck.Domain, "CSRF clear cookie must be host-only (no Domain)")
	})
}

// lastCookieNamed returns the cookie with the given name from the response, or
// fails the test. It guards the host-only assertions against a silent rename.
func lastCookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no cookie named %q in response", name)
	return nil
}
