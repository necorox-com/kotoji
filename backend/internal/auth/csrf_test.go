package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCSRF_IssueAndVerify(t *testing.T) {
	c := NewCSRF("kotoji_csrf", false)

	// Issue mints a readable (non-HttpOnly) cookie.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	tok, err := c.Issue(rec, req)
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, "kotoji_csrf", cookies[0].Name)
	require.False(t, cookies[0].HttpOnly, "CSRF cookie must be readable by the SPA")
	require.Equal(t, tok, cookies[0].Value)
}

func TestCSRF_Verify(t *testing.T) {
	c := NewCSRF("kotoji_csrf", false)
	const tok = "the-csrf-token"

	withCookie := func(r *http.Request) *http.Request {
		r.AddCookie(&http.Cookie{Name: "kotoji_csrf", Value: tok})
		return r
	}

	tests := []struct {
		name  string
		build func() *http.Request
		want  bool
	}{
		{
			name:  "safe GET always passes",
			build: func() *http.Request { return httptest.NewRequest(http.MethodGet, "/api/sites", nil) },
			want:  true,
		},
		{
			name: "POST with matching header+cookie passes",
			build: func() *http.Request {
				r := withCookie(httptest.NewRequest(http.MethodPost, "/api/sites", nil))
				r.Header.Set("X-CSRF-Token", tok)
				return r
			},
			want: true,
		},
		{
			name: "POST with mismatched header fails",
			build: func() *http.Request {
				r := withCookie(httptest.NewRequest(http.MethodPost, "/api/sites", nil))
				r.Header.Set("X-CSRF-Token", "wrong")
				return r
			},
			want: false,
		},
		{
			name: "POST with cookie but no header fails",
			build: func() *http.Request {
				return withCookie(httptest.NewRequest(http.MethodPost, "/api/sites", nil))
			},
			want: false,
		},
		{
			name: "POST with header but no cookie fails",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/api/sites", nil)
				r.Header.Set("X-CSRF-Token", tok)
				return r
			},
			want: false,
		},
		{
			name: "bearer-token POST bypasses CSRF",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/api/sites", nil)
				r.Header.Set("Authorization", "Bearer kotoji_pat_abc")
				return r
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, c.Verify(tc.build()))
		})
	}
}

func TestCSRF_Middleware(t *testing.T) {
	c := NewCSRF("kotoji_csrf", false)
	called := false
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	// Reject path: unsafe method without a token -> 403, handler not reached.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sites", nil))
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.False(t, called)

	// Accept path: matching token -> handler runs.
	called = false
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sites", nil)
	req.AddCookie(&http.Cookie{Name: "kotoji_csrf", Value: "t"})
	req.Header.Set("X-CSRF-Token", "t")
	h.ServeHTTP(rec, req)
	require.True(t, called)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestCSRF_HostPrefix(t *testing.T) {
	require.Equal(t, "__Host-kotoji_csrf", NewCSRF("kotoji_csrf", true).CookieName())
	require.Equal(t, "kotoji_csrf", NewCSRF("kotoji_csrf", false).CookieName())
}
