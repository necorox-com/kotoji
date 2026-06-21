package api

import (
	"net/http"
	"testing"
)

func TestAdminSurface(t *testing.T) {
	t.Run("non-admin is forbidden from the flags endpoint", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		target := e.newUser()
		rec := e.request(http.MethodPatch, "/api/admin/users/"+target.rec.ID.String()+"/flags").as(u).
			json(map[string]any{"isAdmin": true}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("anonymous is unauthenticated", func(t *testing.T) {
		e := newTestEnv(t)
		target := e.newUser()
		rec := e.request(http.MethodPatch, "/api/admin/users/"+target.rec.ID.String()+"/flags").
			json(map[string]any{"isAdmin": true}).do()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("admin toggles user flags", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		target := e.newUser(withNoCreate)
		rec := e.request(http.MethodPatch, "/api/admin/users/"+target.rec.ID.String()+"/flags").as(admin).
			json(map[string]any{"canCreateSites": true}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got struct {
			CanCreateSites bool `json:"canCreateSites"`
		}
		decodeBody(t, rec, &got)
		if !got.CanCreateSites {
			t.Fatalf("canCreateSites = false, want true")
		}
	})

	t.Run("admin reads a site audit feed", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		owner := e.newUser()
		// Create a site via the API so an audit row exists.
		e.createSite("audit-site", owner)
		// Trigger one audited write through the owner.
		fc := e.readDraftFile(owner, "audit-site", "index.html")
		if rec := e.request(http.MethodPut, "/api/sites/audit-site/branches/draft/file").as(owner).
			json(map[string]any{"path": "index.html", "content": "x", "baseSha": fc.Sha}).do(); rec.Code != http.StatusOK {
			t.Fatalf("seed write status = %d", rec.Code)
		}

		rec := e.request(http.MethodGet, "/api/admin/sites/audit-site/audit").as(admin).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got struct {
			Audit []auditEntry `json:"audit"`
		}
		decodeBody(t, rec, &got)
		if len(got.Audit) == 0 {
			t.Fatalf("expected at least one audit row")
		}
	})
}
