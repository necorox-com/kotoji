package api

import (
	"net/http"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

// TestPurgeSiteCache covers the operator "Clear cache" endpoint: an editor (or
// owner) can purge and gets a bumped cacheVersion + audit entry; a viewer is
// rejected (403); an unknown handle is 404.
func TestPurgeSiteCache(t *testing.T) {
	t.Run("owner purges -> 200 + bumped version", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("purge-owner", owner)

		rec := e.request(http.MethodPost, "/api/sites/purge-owner/cache/purge").as(owner).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.CachePurgeResult
		decodeBody(t, rec, &got)
		// The fake store starts every site at version 0, so the first bump returns 1.
		if got.CacheVersion != 1 {
			t.Fatalf("cacheVersion = %d, want 1", got.CacheVersion)
		}
		if !contains(e.store.auditActions(), "cache_purge") {
			t.Fatalf("audit = %v, want cache_purge", e.store.auditActions())
		}

		// A second purge increments again (monotonic generation counter).
		rec2 := e.request(http.MethodPost, "/api/sites/purge-owner/cache/purge").as(owner).do()
		var got2 openapi.CachePurgeResult
		decodeBody(t, rec2, &got2)
		if got2.CacheVersion != 2 {
			t.Fatalf("second cacheVersion = %d, want 2", got2.CacheVersion)
		}
	})

	t.Run("editor can purge -> 200", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("purge-editor", owner)
		editor := e.newUser()
		e.store.setRole(st.ID, editor.rec.ID, gen.SiteRoleEditor)

		rec := e.request(http.MethodPost, "/api/sites/purge-editor/cache/purge").as(editor).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("viewer rejected -> 403", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("purge-viewer", owner)
		viewer := e.newUser()
		e.store.setRole(st.ID, viewer.rec.ID, gen.SiteRoleViewer)

		rec := e.request(http.MethodPost, "/api/sites/purge-viewer/cache/purge").as(viewer).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown handle -> 404", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		rec := e.request(http.MethodPost, "/api/sites/no-such-site/cache/purge").as(owner).do()
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}
