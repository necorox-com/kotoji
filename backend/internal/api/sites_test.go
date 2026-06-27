package api

import (
	"net/http"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

func TestCreateSite(t *testing.T) {
	t.Run("owner with can_create_sites creates a draft", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		rec := e.request(http.MethodPost, "/api/sites").as(u).json(openapi.CreateSiteRequest{Handle: "my-site"}).do()
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.Site
		decodeBody(t, rec, &got)
		if got.Handle != "my-site" {
			t.Fatalf("handle = %q, want my-site", got.Handle)
		}
		if got.OwnerId != u.rec.ID {
			t.Fatalf("ownerId = %v, want %v", got.OwnerId, u.rec.ID)
		}
		// site.create must be audited.
		if !contains(e.store.auditActions(), "site.create") {
			t.Fatalf("audit actions = %v, want site.create", e.store.auditActions())
		}
	})

	t.Run("user without can_create_sites is forbidden", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser(withNoCreate)
		rec := e.request(http.MethodPost, "/api/sites").as(u).json(openapi.CreateSiteRequest{Handle: "nope-site"}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if env := errEnvelope(t, rec); env.Error.Code != codeForbidden {
			t.Fatalf("code = %q, want forbidden", env.Error.Code)
		}
	})

	t.Run("anonymous is unauthenticated", func(t *testing.T) {
		e := newTestEnv(t)
		rec := e.request(http.MethodPost, "/api/sites").json(openapi.CreateSiteRequest{Handle: "anon-site"}).do()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("reserved handle is 422 validation", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		rec := e.request(http.MethodPost, "/api/sites").as(u).json(openapi.CreateSiteRequest{Handle: "admin"}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
		if env := errEnvelope(t, rec); env.Error.Code != codeValidation {
			t.Fatalf("code = %q, want validation", env.Error.Code)
		}
	})

	t.Run("duplicate handle is 409 handle_taken", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		e.createSite("dupe-site", u)
		rec := e.request(http.MethodPost, "/api/sites").as(u).json(openapi.CreateSiteRequest{Handle: "dupe-site"}).do()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if env := errEnvelope(t, rec); env.Error.Code != codeHandleTaken {
			t.Fatalf("code = %q, want handle_taken", env.Error.Code)
		}
	})
}

func TestGetSite(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	st := e.createSite("get-site", owner)

	t.Run("member reads detail", func(t *testing.T) {
		rec := e.request(http.MethodGet, "/api/sites/get-site").as(owner).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.Site
		decodeBody(t, rec, &got)
		if got.Id != st.ID {
			t.Fatalf("id = %v, want %v", got.Id, st.ID)
		}
	})

	t.Run("non-member gets 404 (no existence disclosure)", func(t *testing.T) {
		stranger := e.newUser()
		rec := e.request(http.MethodGet, "/api/sites/get-site").as(stranger).do()
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("unknown handle is 404", func(t *testing.T) {
		rec := e.request(http.MethodGet, "/api/sites/does-not-exist").as(owner).do()
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("instance admin reads any site without membership", func(t *testing.T) {
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodGet, "/api/sites/get-site").as(admin).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("admin status = %d, want 200", rec.Code)
		}
	})
}

func TestListSites(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("list-a", owner)
	e.createSite("list-b", owner)

	rec := e.request(http.MethodGet, "/api/sites").as(owner).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Sites []openapi.SiteSummary `json:"sites"`
	}
	decodeBody(t, rec, &got)
	if len(got.Sites) != 2 {
		t.Fatalf("sites = %d, want 2", len(got.Sites))
	}
	for _, s := range got.Sites {
		if s.Role != openapi.SiteRoleOwner {
			t.Fatalf("role = %q, want owner", s.Role)
		}
	}
}

func TestRenameAndDeleteSite(t *testing.T) {
	t.Run("owner renames", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("rename-me", owner)
		rec := e.request(http.MethodPost, "/api/sites/rename-me/rename").as(owner).
			json(openapi.RenameSiteJSONRequestBody{NewHandle: "renamed-ok"}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.Site
		decodeBody(t, rec, &got)
		if got.Handle != "renamed-ok" {
			t.Fatalf("handle = %q, want renamed-ok", got.Handle)
		}
	})

	t.Run("editor cannot rename (403)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		editor := e.newUser()
		st := e.createSite("rename-deny", owner)
		e.store.setRole(st.ID, editor.rec.ID, gen.SiteRoleEditor)
		rec := e.request(http.MethodPost, "/api/sites/rename-deny/rename").as(editor).
			json(openapi.RenameSiteJSONRequestBody{NewHandle: "nope-rename"}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("owner soft-deletes", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("delete-me", owner)
		rec := e.request(http.MethodDelete, "/api/sites/delete-me").as(owner).do()
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	})
}

func TestUpdateSite(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("settings-site", owner)

	vis := openapi.Members
	rec := e.request(http.MethodPatch, "/api/sites/settings-site").as(owner).
		json(openapi.UpdateSiteRequest{Visibility: &vis}).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(e.store.settingsUpdates) != 1 {
		t.Fatalf("settings updates = %d, want 1", len(e.store.settingsUpdates))
	}
	if got := e.store.settingsUpdates[0].Visibility; got != gen.SiteVisibilityMembers {
		t.Fatalf("visibility = %q, want members", got)
	}
}

func TestMirrorSite(t *testing.T) {
	// withRepo links the seeded site to a GitHub repo so the mirror has a target.
	withRepo := func(in *site.CreateSiteInput) { in.GitHubRepo = "necorox-com/example" }

	t.Run("owner triggers a successful mirror push", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSiteWith("mirror-ok", owner, withRepo)
		rec := e.request(http.MethodPost, "/api/sites/mirror-ok/mirror").as(owner).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.MirrorResult
		decodeBody(t, rec, &got)
		if !got.Ok || !got.Pushed {
			t.Fatalf("ok=%v pushed=%v, want both true (body=%s)", got.Ok, got.Pushed, rec.Body.String())
		}
		if got.Error != nil {
			t.Fatalf("error = %v, want nil on success", *got.Error)
		}
		// draft + published are the mirrored branches.
		if len(got.Branches) != 2 {
			t.Fatalf("branches = %v, want draft+published", got.Branches)
		}
		if !contains(e.store.auditActions(), "site.mirror") {
			t.Fatalf("audit actions = %v, want site.mirror", e.store.auditActions())
		}
	})

	t.Run("not linked returns 200 ok=false with a clear message", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("mirror-unlinked", owner) // no GitHubRepo
		rec := e.request(http.MethodPost, "/api/sites/mirror-unlinked/mirror").as(owner).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.MirrorResult
		decodeBody(t, rec, &got)
		if got.Ok || got.Pushed {
			t.Fatalf("ok=%v pushed=%v, want both false when not linked", got.Ok, got.Pushed)
		}
		if got.Message == "" {
			t.Fatalf("message must explain the site is not linked (got empty)")
		}
	})

	t.Run("best-effort push failure returns 200 ok=false (not an HTTP error)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSiteWith("mirror-fail", owner, withRepo)
		// Force the next MirrorPush to fail (origin unreachable / auth rejected).
		e.svc.FailNext["MirrorPush"] = site.ErrGit
		rec := e.request(http.MethodPost, "/api/sites/mirror-fail/mirror").as(owner).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 even on push failure (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.MirrorResult
		decodeBody(t, rec, &got)
		if got.Ok || got.Pushed {
			t.Fatalf("ok=%v pushed=%v, want both false on push failure", got.Ok, got.Pushed)
		}
		if got.Error == nil || *got.Error == "" {
			t.Fatalf("error detail must be set on push failure")
		}
	})

	t.Run("editor cannot mirror (owner-only, 403)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		editor := e.newUser()
		st := e.createSiteWith("mirror-deny", owner, withRepo)
		e.store.setRole(st.ID, editor.rec.ID, gen.SiteRoleEditor)
		rec := e.request(http.MethodPost, "/api/sites/mirror-deny/mirror").as(editor).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("anonymous is unauthenticated (401)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSiteWith("mirror-anon", owner, withRepo)
		rec := e.request(http.MethodPost, "/api/sites/mirror-anon/mirror").do()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
