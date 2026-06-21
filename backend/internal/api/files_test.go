package api

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

// readDraftFile reads index.html on draft and returns its content + commit SHA
// (the optimistic-lock token to echo as baseSha on write).
func (e *testEnv) readDraftFile(u testUser, handle, path string) openapi.FileContent {
	e.t.Helper()
	rec := e.request(http.MethodGet, "/api/sites/"+handle+"/branches/draft/file?path="+url.QueryEscape(path)).as(u).do()
	if rec.Code != http.StatusOK {
		e.t.Fatalf("read %s: status %d (body=%s)", path, rec.Code, rec.Body.String())
	}
	var fc openapi.FileContent
	decodeBody(e.t, rec, &fc)
	return fc
}

func TestReadFile(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("read-site", owner)

	t.Run("reads seeded index.html as utf-8", func(t *testing.T) {
		fc := e.readDraftFile(owner, "read-site", "index.html")
		if fc.Encoding != openapi.FileContentEncodingUtf8 {
			t.Fatalf("encoding = %q, want utf-8", fc.Encoding)
		}
		if fc.Sha == "" {
			t.Fatalf("sha (lock token) must be set")
		}
	})

	t.Run("missing path is 422", func(t *testing.T) {
		rec := e.request(http.MethodGet, "/api/sites/read-site/branches/draft/file").as(owner).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})

	t.Run("unknown file is 404", func(t *testing.T) {
		rec := e.request(http.MethodGet, "/api/sites/read-site/branches/draft/file?path=nope.html").as(owner).do()
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestWriteFile(t *testing.T) {
	t.Run("editor writes with correct baseSha", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("write-site", owner)
		fc := e.readDraftFile(owner, "write-site", "index.html")

		rec := e.request(http.MethodPut, "/api/sites/write-site/branches/draft/file").as(owner).
			json(openapi.WriteFileRequest{Path: "index.html", Content: "<h1>updated</h1>", BaseSha: fc.Sha}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.WriteResult
		decodeBody(t, rec, &got)
		if !got.Committed || got.Commit == "" {
			t.Fatalf("expected committed with a new tip, got %+v", got)
		}
		if !contains(e.store.auditActions(), "file.write") {
			t.Fatalf("audit = %v, want file.write", e.store.auditActions())
		}
	})

	t.Run("stale baseSha returns 409 with conflict envelope", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("conflict-site", owner)

		rec := e.request(http.MethodPut, "/api/sites/conflict-site/branches/draft/file").as(owner).
			json(openapi.WriteFileRequest{Path: "index.html", Content: "x", BaseSha: "0000000000000000000000000000000000000000"}).do()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
		var env openapi.ConflictEnvelope
		decodeBody(t, rec, &env)
		if env.Error.Code != openapi.ConflictEnvelopeErrorCodeConflict {
			t.Fatalf("code = %q, want conflict", env.Error.Code)
		}
		// The conflict detail must carry the frozen field names (CANONICAL §8).
		if env.Error.Details.Expected != "0000000000000000000000000000000000000000" {
			t.Fatalf("details.expected = %q, want the sent sha", env.Error.Details.Expected)
		}
		if env.Error.Details.Actual == "" {
			t.Fatalf("details.actual must be the real tip")
		}
	})

	t.Run("empty baseSha is 422 validation", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("empty-base-site", owner)
		rec := e.request(http.MethodPut, "/api/sites/empty-base-site/branches/draft/file").as(owner).
			json(openapi.WriteFileRequest{Path: "index.html", Content: "x", BaseSha: ""}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("viewer cannot write (403)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("viewer-write-site", owner)
		viewer := e.newUser()
		e.store.setRole(st.ID, viewer.rec.ID, "viewer")
		fc := e.readDraftFile(owner, "viewer-write-site", "index.html")
		rec := e.request(http.MethodPut, "/api/sites/viewer-write-site/branches/draft/file").as(viewer).
			json(openapi.WriteFileRequest{Path: "index.html", Content: "x", BaseSha: fc.Sha}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("base64 content round-trips", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("b64-site", owner)
		fc := e.readDraftFile(owner, "b64-site", "index.html")
		enc := openapi.WriteFileRequestEncodingBase64
		payload := base64.StdEncoding.EncodeToString([]byte("<h2>b64</h2>"))
		rec := e.request(http.MethodPut, "/api/sites/b64-site/branches/draft/file").as(owner).
			json(openapi.WriteFileRequest{Path: "index.html", Content: payload, Encoding: &enc, BaseSha: fc.Sha}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestDeleteFile(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("del-file-site", owner)
	fc := e.readDraftFile(owner, "del-file-site", "index.html")

	rec := e.request(http.MethodDelete,
		"/api/sites/del-file-site/branches/draft/file?path=index.html&baseSha="+url.QueryEscape(fc.Sha)).as(owner).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var ci openapi.CommitInfo
	decodeBody(t, rec, &ci)
	if ci.Sha == "" {
		t.Fatalf("expected a delete commit sha")
	}
}

func TestListFiles(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("ls-site", owner)

	rec := e.request(http.MethodGet, "/api/sites/ls-site/branches/draft/files").as(owner).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got openapi.FileListing
	decodeBody(t, rec, &got)
	if got.Ref == "" {
		t.Fatalf("listing must carry the resolved ref")
	}
	if len(got.Files) == 0 {
		t.Fatalf("expected at least the seeded index.html")
	}
}
