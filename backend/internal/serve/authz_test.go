package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

func newGrantAuthz(t *testing.T, secure bool, now func() time.Time) *GrantAuthz {
	t.Helper()
	a, err := NewGrantAuthz(GrantAuthzConfig{
		Secret:       []byte("test-secret-key-0123456789"),
		CookieSecure: secure,
		CookieTTL:    time.Hour,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("NewGrantAuthz: %v", err)
	}
	return a
}

func TestNewGrantAuthz_RequiresSecret(t *testing.T) {
	if _, err := NewGrantAuthz(GrantAuthzConfig{}); err == nil {
		t.Fatal("expected error for empty secret (fail-closed)")
	}
}

func TestAuthz_PublishedNeverGated(t *testing.T) {
	// A DenyPreviewAuthz would 404 a preview; a published target must NOT be run
	// through authz at all, so it serves 200 even with a deny authorizer.
	files := fstest.MapFS{"index.html": {Data: []byte("<html><head></head>pub</html>")}}
	h := NewHandler(Deps{
		Resolver: staticResolver{target: publishedTarget("s")},
		Trees:    &fakeTreeProvider{byHandle: map[string]fakeSite{"s": {fsys: files}}},
		Authz:    DenyPreviewAuthz{},
		Config:   HandlerConfig{Now: func() time.Time { return fixedTime }},
	})
	w := do(h, http.MethodGet, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("published must not be gated, got %d", w.Code)
	}
}

func TestAuthz_PreviewRequiresCredential(t *testing.T) {
	files := fstest.MapFS{"index.html": {Data: []byte("<html><head></head>draft</html>")}}

	t.Run("default_404", func(t *testing.T) {
		h := NewHandler(Deps{
			Resolver: staticResolver{target: previewTarget("s", "draft")},
			Trees:    &fakeTreeProvider{byHandle: map[string]fakeSite{"s": {fsys: files}}},
			Authz:    DenyPreviewAuthz{},
			Config:   HandlerConfig{Now: func() time.Time { return fixedTime }},
		})
		w := do(h, http.MethodGet, "/")
		if w.Code != http.StatusNotFound {
			t.Fatalf("unauth preview default want 404, got %d", w.Code)
		}
	})

	t.Run("debug_401", func(t *testing.T) {
		h := NewHandler(Deps{
			Resolver: staticResolver{target: previewTarget("s", "draft")},
			Trees:    &fakeTreeProvider{byHandle: map[string]fakeSite{"s": {fsys: files}}},
			Authz:    DenyPreviewAuthz{},
			Config:   HandlerConfig{PreviewUnauthStatus: http.StatusUnauthorized, Now: func() time.Time { return fixedTime }},
		})
		w := do(h, http.MethodGet, "/")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauth preview debug want 401, got %d", w.Code)
		}
	})
}

func TestAuthz_PreviewGrantCookieFlow(t *testing.T) {
	now := func() time.Time { return fixedTime }
	a := newGrantAuthz(t, false, now)
	sid := uuid.New()
	files := fstest.MapFS{"index.html": {Data: []byte("<html><head></head>draft</html>")}}
	tp := &fakeTreeProvider{byHandle: map[string]fakeSite{"s": {fsys: files, siteID: sid}}}
	h := NewHandler(Deps{
		Resolver: staticResolver{target: previewTarget("s", "draft")},
		Trees:    tp,
		Authz:    a,
		Config:   HandlerConfig{Now: now},
	})

	t.Run("valid_kpt_sets_cookie_and_302", func(t *testing.T) {
		grant := a.SignGrant(sid, "draft", fixedTime.Add(10*time.Minute))
		r := httptest.NewRequest(http.MethodGet, "http://s/?kpt="+grant+"&keep=1", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusFound {
			t.Fatalf("want 302 after grant, got %d", w.Code)
		}
		// kpt stripped, other params kept.
		if loc := w.Header().Get("Location"); loc != "/?keep=1" {
			t.Fatalf("redirect should strip kpt, got %q", loc)
		}
		var ck *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == PreviewCookieName {
				ck = c
			}
		}
		if ck == nil {
			t.Fatal("expected kotoji_preview cookie to be set")
		}
		if ck.Domain != "" {
			t.Fatalf("cookie must be host-only (no Domain), got %q", ck.Domain)
		}
		if !ck.HttpOnly {
			t.Fatal("cookie must be HttpOnly")
		}

		// Subsequent request with the cookie passes.
		r2 := httptest.NewRequest(http.MethodGet, "http://s/", nil)
		r2.AddCookie(ck)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, r2)
		if w2.Code != http.StatusOK {
			t.Fatalf("cookie should grant access, got %d", w2.Code)
		}
	})

	t.Run("tampered_kpt_404", func(t *testing.T) {
		grant := a.SignGrant(sid, "draft", fixedTime.Add(10*time.Minute))
		r := httptest.NewRequest(http.MethodGet, "http://s/?kpt="+grant+"x", nil) // tamper sig
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("tampered grant want 404, got %d", w.Code)
		}
	})

	t.Run("expired_kpt_404", func(t *testing.T) {
		grant := a.SignGrant(sid, "draft", fixedTime.Add(-time.Minute)) // already expired
		r := httptest.NewRequest(http.MethodGet, "http://s/?kpt="+grant, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expired grant want 404, got %d", w.Code)
		}
	})

	t.Run("cookie_for_site_A_rejected_on_site_B", func(t *testing.T) {
		otherSite := uuid.New()
		// Cookie signed for a DIFFERENT site id.
		cookieVal := a.SignGrant(otherSite, "draft", fixedTime.Add(time.Hour))
		r := httptest.NewRequest(http.MethodGet, "http://s/", nil)
		r.AddCookie(&http.Cookie{Name: PreviewCookieName, Value: cookieVal})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("wrong-site cookie want 404, got %d", w.Code)
		}
	})

	t.Run("cookie_for_wrong_branch_rejected", func(t *testing.T) {
		cookieVal := a.SignGrant(sid, "other-branch", fixedTime.Add(time.Hour))
		r := httptest.NewRequest(http.MethodGet, "http://s/", nil)
		r.AddCookie(&http.Cookie{Name: PreviewCookieName, Value: cookieVal})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("wrong-branch cookie want 404, got %d", w.Code)
		}
	})
}

// fakeBearer accepts exactly one (token, siteID) pair.
type fakeBearer struct {
	token  string
	siteID uuid.UUID
}

func (f fakeBearer) ValidateBearer(_ context.Context, token string, siteID uuid.UUID) error {
	if token != f.token {
		return ErrUnauthorized
	}
	if siteID != f.siteID {
		return ErrForbidden
	}
	return nil
}

func TestAuthz_BearerToken(t *testing.T) {
	now := func() time.Time { return fixedTime }
	sid := uuid.New()
	a, err := NewGrantAuthz(GrantAuthzConfig{
		Secret: []byte("k"),
		Now:    now,
		Bearer: fakeBearer{token: "good-token", siteID: sid},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"index.html": {Data: []byte("<html><head></head>draft</html>")}}
	tp := &fakeTreeProvider{byHandle: map[string]fakeSite{"s": {fsys: files, siteID: sid}}}
	h := NewHandler(Deps{
		Resolver: staticResolver{target: previewTarget("s", "draft")},
		Trees:    tp,
		Authz:    a,
		Config:   HandlerConfig{Now: now},
	})

	t.Run("valid_bearer_ok", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://s/", nil)
		r.Header.Set("Authorization", "Bearer good-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("valid bearer want 200, got %d", w.Code)
		}
	})

	t.Run("wrong_token_404", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://s/", nil)
		r.Header.Set("Authorization", "Bearer nope")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("wrong bearer want 404, got %d", w.Code)
		}
	})
}

func TestAuthz_Modes_NoAuthOpen(t *testing.T) {
	files := fstest.MapFS{"index.html": {Data: []byte("<html><head></head>draft</html>")}}
	h := NewHandler(Deps{
		Resolver: staticResolver{target: previewTarget("s", "draft")},
		Trees:    &fakeTreeProvider{byHandle: map[string]fakeSite{"s": {fsys: files}}},
		Authz:    OpenPreviewAuthz{},
		Config:   HandlerConfig{Now: func() time.Time { return fixedTime }},
	})
	w := do(h, http.MethodGet, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("no-auth mode should open previews, got %d", w.Code)
	}
}

func TestAuthz_NoCookieDomainLeak(t *testing.T) {
	now := func() time.Time { return fixedTime }
	a := newGrantAuthz(t, true, now) // Secure=true
	sid := uuid.New()
	grant := a.SignGrant(sid, "draft", fixedTime.Add(time.Minute))
	r := httptest.NewRequest(http.MethodGet, "http://s/?kpt="+grant, nil)
	act, err := a.Authorize(context.Background(), previewTarget("s", "draft"), r, sid)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if act.SetCookie == nil {
		t.Fatal("expected a cookie action")
	}
	c := act.SetCookie
	if c.Domain != "" {
		t.Fatalf("cookie must have NO Domain (host-only), got %q", c.Domain)
	}
	if !c.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Fatal("cookie must be Secure when configured")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie SameSite want Lax, got %v", c.SameSite)
	}
}

// TestAuthz_DirectAuthorize covers the PreviewAuthz interface contract directly
// (ErrUnauthorized vs ErrForbidden), independent of the handler's 404 collapsing.
func TestAuthz_DirectAuthorize(t *testing.T) {
	now := func() time.Time { return fixedTime }
	a := newGrantAuthz(t, false, now)
	sid := uuid.New()

	t.Run("no_credential_unauthorized", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://s/", nil)
		_, err := a.Authorize(context.Background(), previewTarget("s", "draft"), r, sid)
		if err != ErrUnauthorized {
			t.Fatalf("want ErrUnauthorized, got %v", err)
		}
	})

	t.Run("grant_wrong_site_forbidden", func(t *testing.T) {
		grant := a.SignGrant(uuid.New(), "draft", fixedTime.Add(time.Minute))
		r := httptest.NewRequest(http.MethodGet, "http://s/?kpt="+grant, nil)
		_, err := a.Authorize(context.Background(), previewTarget("s", "draft"), r, sid)
		if err != ErrForbidden {
			t.Fatalf("want ErrForbidden, got %v", err)
		}
	})
}

// silence unused import in builds where resolve is only referenced via helpers.
var _ = resolve.SourceHost
