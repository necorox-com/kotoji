package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestAuth builds an Auth with the fake store + fake upserter wired in, in
// insecure (dev http) cookie mode so cookie names are un-prefixed for tests.
func newTestAuth(t *testing.T, p AuthProvider) (*Auth, *fakeStore, *fakeUpserter) {
	t.Helper()
	store := newFakeStore()
	a := New(testConfig(), store, p)
	up := &fakeUpserter{store: store}
	a.upserter = up // swap the store-backed upserter for the in-memory fake
	return a, store, up
}

func TestSessionAuth_LoadsUser(t *testing.T) {
	a, store, _ := newTestAuth(t, &fakeProvider{key: "dev"})
	u := activeUser(t, true)
	store.addUser(u)
	sid, err := a.sessions.Create(context.Background(), u.ID, "ua", "")
	require.NoError(t, err)

	var seen *SessionUser
	h := a.SessionAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cu, _ := CurrentUser(r.Context())
		seen = cu
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: sid})
	h.ServeHTTP(httptest.NewRecorder(), req)

	require.NotNil(t, seen)
	require.Equal(t, u.ID, seen.UserID)
	require.True(t, seen.IsAdmin)
}

func TestSessionAuth_Variants(t *testing.T) {
	tests := []struct {
		name     string
		cookie   func(a *Auth, store *fakeStore) *http.Cookie
		wantUser bool
	}{
		{
			name:     "no cookie -> anonymous",
			cookie:   func(a *Auth, store *fakeStore) *http.Cookie { return nil },
			wantUser: false,
		},
		{
			name: "expired session -> anonymous",
			cookie: func(a *Auth, store *fakeStore) *http.Cookie {
				u := activeUser(t, false)
				store.addUser(u)
				sid, _ := a.sessions.Create(context.Background(), u.ID, "ua", "")
				store.expireSession(sid)
				return &http.Cookie{Name: a.sessions.CookieName(), Value: sid}
			},
			wantUser: false,
		},
		{
			name: "store error -> anonymous (non-fatal)",
			cookie: func(a *Auth, store *fakeStore) *http.Cookie {
				store.failGet = true
				return &http.Cookie{Name: a.sessions.CookieName(), Value: "anything"}
			},
			wantUser: false,
		},
		{
			name: "valid session -> user present",
			cookie: func(a *Auth, store *fakeStore) *http.Cookie {
				u := activeUser(t, false)
				store.addUser(u)
				sid, _ := a.sessions.Create(context.Background(), u.ID, "ua", "")
				return &http.Cookie{Name: a.sessions.CookieName(), Value: sid}
			},
			wantUser: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, store, _ := newTestAuth(t, &fakeProvider{key: "dev"})
			ck := tc.cookie(a, store)

			var present bool
			h := a.SessionAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, present = CurrentUser(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
			if ck != nil {
				req.AddCookie(ck)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			require.Equal(t, tc.wantUser, present)
		})
	}
}

func TestRequireAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })

	// Anonymous -> 401.
	rec := httptest.NewRecorder()
	RequireAuth(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Authenticated -> passes through.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, &SessionUser{}))
	RequireAuth(next).ServeHTTP(rec, req)
	require.Equal(t, http.StatusTeapot, rec.Code)
}

func TestRequireAdmin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })

	tests := []struct {
		name string
		user *SessionUser
		want int
	}{
		{name: "anonymous -> 401", user: nil, want: http.StatusUnauthorized},
		{name: "non-admin -> 403", user: &SessionUser{IsAdmin: false}, want: http.StatusForbidden},
		{name: "admin -> pass", user: &SessionUser{IsAdmin: true}, want: http.StatusTeapot},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.user != nil {
				req = req.WithContext(context.WithValue(req.Context(), userCtxKey, tc.user))
			}
			rec := httptest.NewRecorder()
			RequireAdmin(next).ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestMiddlewareSeam(t *testing.T) {
	a, _, _ := newTestAuth(t, &fakeProvider{key: "dev"})
	// Middleware() must return a usable func(http.Handler) http.Handler.
	mw := a.Middleware()
	require.NotNil(t, mw)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	_ = time.Now
}
