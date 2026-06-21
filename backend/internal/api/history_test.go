package api

import (
	"net/http"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

func TestPublish(t *testing.T) {
	t.Run("owner publishes draft (direct mode)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("pub-site", owner)
		fc := e.readDraftFile(owner, "pub-site", "index.html")

		rec := e.request(http.MethodPost, "/api/sites/pub-site/publish").as(owner).
			json(openapi.PublishRequest{BaseSha: fc.Sha}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.PublishResult
		decodeBody(t, rec, &got)
		if got.PublishedCommit == "" {
			t.Fatalf("expected a published commit sha")
		}
		if got.PublishedUrl == nil || *got.PublishedUrl == "" {
			t.Fatalf("expected a published URL")
		}
		if !contains(e.store.auditActions(), "publish") {
			t.Fatalf("audit = %v, want publish", e.store.auditActions())
		}
	})

	t.Run("editor cannot direct-publish in request mode (403)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		// Seed a site whose publish_mode is 'request' directly through the Service
		// (the Service is the source of truth the publish gate reads from).
		st := e.createSiteWith("req-pub-site", owner, func(in *site.CreateSiteInput) {
			in.PublishMode = "request"
		})

		editor := e.newUser()
		e.store.setRole(st.ID, editor.rec.ID, gen.SiteRoleEditor)
		fc := e.readDraftFile(owner, "req-pub-site", "index.html")
		rec := e.request(http.MethodPost, "/api/sites/req-pub-site/publish").as(editor).
			json(openapi.PublishRequest{BaseSha: fc.Sha}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("owner can direct-publish even in request mode", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSiteWith("owner-req-site", owner, func(in *site.CreateSiteInput) {
			in.PublishMode = "request"
		})
		fc := e.readDraftFile(owner, "owner-req-site", "index.html")
		rec := e.request(http.MethodPost, "/api/sites/owner-req-site/publish").as(owner).
			json(openapi.PublishRequest{BaseSha: fc.Sha}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestLog(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("log-site", owner)

	rec := e.request(http.MethodGet, "/api/sites/log-site/branches/draft/log").as(owner).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got openapi.LogResult
	decodeBody(t, rec, &got)
	if len(got.Commits) == 0 {
		t.Fatalf("expected at least the initial commit")
	}
	if got.Branch != "draft" {
		t.Fatalf("branch = %q, want draft", got.Branch)
	}
}

func TestCommitAndRollback(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("hist-site", owner)

	// Write (stage+commit) to advance history, capturing the new tip.
	fc := e.readDraftFile(owner, "hist-site", "index.html")
	wrec := e.request(http.MethodPut, "/api/sites/hist-site/branches/draft/file").as(owner).
		json(openapi.WriteFileRequest{Path: "index.html", Content: "<h1>v2</h1>", BaseSha: fc.Sha}).do()
	if wrec.Code != http.StatusOK {
		t.Fatalf("write status = %d", wrec.Code)
	}
	var wr openapi.WriteResult
	decodeBody(t, wrec, &wr)

	// Rollback to the original commit's tree as a new forward commit.
	rrec := e.request(http.MethodPost, "/api/sites/hist-site/branches/draft/rollback").as(owner).
		json(openapi.RollbackRequest{ToSha: fc.Sha, BaseSha: wr.Commit}).do()
	if rrec.Code != http.StatusOK {
		t.Fatalf("rollback status = %d (body=%s)", rrec.Code, rrec.Body.String())
	}
}

func TestDiff(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("diff-site", owner)

	t.Run("missing from is 422", func(t *testing.T) {
		rec := e.request(http.MethodGet, "/api/sites/diff-site/diff").as(owner).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})
}
