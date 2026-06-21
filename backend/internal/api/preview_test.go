package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/resolve"
	"github.com/necorox-com/kotoji/backend/internal/serve"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// newPreviewEnv builds a test env whose router wires a REAL serve.GrantAuthz as the
// preview signer, and returns that authorizer so the test can act as the data plane
// (verify the issued grant) — exercising the ONE-codec property end to end.
func newPreviewEnv(t *testing.T) (*testEnv, *serve.GrantAuthz) {
	t.Helper()
	cfg := testConfig()
	svc := site.NewFakeService()
	store := newFakeMetaStore()
	sessions := newFakeSessionStore()

	provider := auth.NewDevProvider(cfg)
	authSvc := auth.New(cfg, sessions, provider)

	gz, err := serve.NewGrantAuthz(serve.GrantAuthzConfig{
		Secret:       []byte("shared-preview-secret-32-bytes!!"),
		CookieSecure: false,
	})
	if err != nil {
		t.Fatalf("grant authz: %v", err)
	}

	router := NewRouter(Deps{
		Config:       cfg,
		Site:         svc,
		Store:        store,
		Auth:         WrapAuth(authSvc),
		PreviewGrant: gz,
	})
	env := &testEnv{t: t, router: router, svc: svc, store: store, sessions: sessions, cfg: cfg}
	return env, gz
}

// extractGrant pulls the kpt grant param out of a previewUrl.
func extractGrant(t *testing.T, previewURL string) string {
	t.Helper()
	u, err := url.Parse(previewURL)
	if err != nil {
		t.Fatalf("parse preview url %q: %v", previewURL, err)
	}
	g := u.Query().Get(serve.PreviewGrantQueryParam)
	if g == "" {
		t.Fatalf("preview url %q has no %s param", previewURL, serve.PreviewGrantQueryParam)
	}
	return g
}

// seedPreviewBranch creates a site owned by `owner` plus a non-published preview
// branch, returning the site.
func (e *testEnv) seedPreviewBranch(handle string, owner testUser, branch string) site.Site {
	e.t.Helper()
	st := e.createSite(handle, owner)
	if _, err := e.svc.CreateBranch(context.Background(), st.ID, site.BranchName(branch), string(site.BranchDraft)); err != nil {
		e.t.Fatalf("create preview branch: %v", err)
	}
	return st
}

func TestPreviewGrant_AuthorizedIssuesAcceptedGrant(t *testing.T) {
	env, gz := newPreviewEnv(t)
	owner := env.newUser()
	const branch = "feature-bob-fix"
	st := env.seedPreviewBranch("preview-site", owner, branch)

	// A viewer (read-only) may preview (CANONICAL §6.1).
	viewer := env.newUser()
	env.store.setRole(st.ID, viewer.rec.ID, gen.SiteRoleViewer)

	rec := env.request(http.MethodPost, "/api/sites/preview-site/branches/"+branch+"/preview-grant").as(viewer).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body previewGrantResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Branch != branch {
		t.Fatalf("branch = %q want %q", body.Branch, branch)
	}

	// Act as the data plane: the SAME serve.GrantAuthz verifier must accept the
	// issued grant for the right site+branch.
	grant := extractGrant(t, body.PreviewUrl)
	target := resolve.Target{Handle: "preview-site", Branch: branch, IsPreview: true}
	r := httptest.NewRequest(http.MethodGet, "/?"+serve.PreviewGrantQueryParam+"="+grant, nil)
	act, err := gz.Authorize(context.Background(), target, r, st.ID)
	if err != nil {
		t.Fatalf("data plane rejected a valid grant: %v", err)
	}
	if act.SetCookie == nil || act.SetCookie.Name != serve.PreviewCookieName {
		t.Fatalf("expected the verifier to set the host-only preview cookie; act=%+v", act)
	}
	// Host-only: no Domain attribute (per-origin isolation, decision #7).
	if act.SetCookie.Domain != "" {
		t.Fatalf("preview cookie must be host-only (no Domain); got %q", act.SetCookie.Domain)
	}
}

func TestPreviewGrant_UnauthorizedRejected(t *testing.T) {
	env, _ := newPreviewEnv(t)
	owner := env.newUser()
	const branch = "feature-x"
	env.seedPreviewBranch("locked-site", owner, branch)

	// A logged-in NON-member gets 404 (do not disclose the site's existence —
	// resolveAccess maps non-membership to 404 before the capability check).
	stranger := env.newUser()
	rec := env.request(http.MethodPost, "/api/sites/locked-site/branches/"+branch+"/preview-grant").as(stranger).do()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-member status = %d want 404", rec.Code)
	}

	// An anonymous request gets 401.
	recAnon := env.request(http.MethodPost, "/api/sites/locked-site/branches/"+branch+"/preview-grant").do()
	if recAnon.Code != http.StatusUnauthorized {
		t.Fatalf("anon status = %d want 401", recAnon.Code)
	}
}

func TestPreviewGrant_WrongSiteGrantRejectedByDataPlane(t *testing.T) {
	env, gz := newPreviewEnv(t)
	owner := env.newUser()
	const branch = "feature-a"
	siteA := env.seedPreviewBranch("site-a", owner, branch)
	siteB := env.seedPreviewBranch("site-b", owner, branch)

	rec := env.request(http.MethodPost, "/api/sites/site-a/branches/"+branch+"/preview-grant").as(owner).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body previewGrantResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	grant := extractGrant(t, body.PreviewUrl)

	// Present site A's grant on site B: the data-plane verifier must reject it
	// (ErrForbidden) — a grant is bound to its site (decision #7 / §8.2).
	_ = siteA
	target := resolve.Target{Handle: "site-b", Branch: branch, IsPreview: true}
	r := httptest.NewRequest(http.MethodGet, "/?"+serve.PreviewGrantQueryParam+"="+grant, nil)
	_, err := gz.Authorize(context.Background(), target, r, siteB.ID)
	if err == nil {
		t.Fatal("site-A grant accepted on site-B; expected rejection")
	}
}

func TestPreviewGrant_PublishedNotPreviewable(t *testing.T) {
	env, _ := newPreviewEnv(t)
	owner := env.newUser()
	env.createSite("pub-site", owner)
	rec := env.request(http.MethodPost, "/api/sites/pub-site/branches/published/preview-grant").as(owner).do()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("published grant status = %d want 422", rec.Code)
	}
}

func TestPreviewGrant_DisabledWhenNoSigner(t *testing.T) {
	// The default harness wires no PreviewGrant signer -> 404.
	env := newTestEnv(t)
	owner := env.newUser()
	const branch = "feature-y"
	env.seedPreviewBranch("nosigner", owner, branch)
	rec := env.request(http.MethodPost, "/api/sites/nosigner/branches/"+branch+"/preview-grant").as(owner).do()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404 (signer disabled)", rec.Code)
	}
}
