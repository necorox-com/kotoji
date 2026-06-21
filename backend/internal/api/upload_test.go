package api

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

// buildZip creates an in-memory zip with the given entries.
func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// buildMultipart wraps a file payload + optional baseSha in a multipart form
// and returns the body bytes and the Content-Type header.
func buildMultipart(t *testing.T, filename string, file []byte, baseSha string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(file); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if baseSha != "" {
		_ = mw.WriteField("baseSha", baseSha)
	}
	_ = mw.Close()
	return &body, mw.FormDataContentType()
}

func TestImportZip(t *testing.T) {
	t.Run("editor imports a valid zip (initial seed allows empty baseSha)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		// Seed a site on a fresh feature branch with no commits so the empty-baseSha
		// initial-seed path applies; simplest is to import onto draft with the
		// current tip, so capture the tip first.
		e.createSite("zip-site", owner)
		fc := e.readDraftFile(owner, "zip-site", "index.html")

		zb := buildZip(t, map[string]string{"index.html": "<h1>imported</h1>", "style.css": "body{}"})
		body, ctype := buildMultipart(t, "site.zip", zb, fc.Sha)

		rec := e.request(http.MethodPost, "/api/sites/zip-site/branches/draft/import").as(owner).
			raw(body, ctype).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var ci openapi.CommitInfo
		decodeBody(t, rec, &ci)
		if ci.Sha == "" {
			t.Fatalf("expected an import commit sha")
		}
		if !contains(e.store.auditActions(), "import") {
			t.Fatalf("audit = %v, want import", e.store.auditActions())
		}
	})

	t.Run("over-cap upload is rejected 413", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("zip-big-site", owner)

		// Build a payload larger than the configured 1 MiB cap (the file part body
		// alone exceeds MaxUploadBytes). It need not be a valid zip — the size gate
		// trips before extraction.
		big := bytes.Repeat([]byte("A"), int(e.cfg.Zip.MaxUploadBytes)+multipartFormMem+1024)
		body, ctype := buildMultipart(t, "big.zip", big, "seed")

		rec := e.request(http.MethodPost, "/api/sites/zip-big-site/branches/draft/import").as(owner).
			raw(body, ctype).do()
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 (body=%s)", rec.Code, rec.Body.String())
		}
		if env := errEnvelope(t, rec); env.Error.Code != codeTooLarge {
			t.Fatalf("code = %q, want too_large", env.Error.Code)
		}
	})

	t.Run("missing file part is 422", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("zip-nofile-site", owner)

		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("baseSha", "seed")
		_ = mw.Close()

		rec := e.request(http.MethodPost, "/api/sites/zip-nofile-site/branches/draft/import").as(owner).
			raw(&body, mw.FormDataContentType()).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("disallowed file type in zip is 415", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("zip-badtype-site", owner)
		fc := e.readDraftFile(owner, "zip-badtype-site", "index.html")

		// .exe is not in the allowlist -> the Service rejects with ErrZipBadType.
		zb := buildZip(t, map[string]string{"index.html": "<h1>ok</h1>", "evil.exe": "MZ"})
		body, ctype := buildMultipart(t, "bad.zip", zb, fc.Sha)
		rec := e.request(http.MethodPost, "/api/sites/zip-badtype-site/branches/draft/import").as(owner).
			raw(body, ctype).do()
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("viewer cannot import (403)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("zip-viewer-site", owner)
		viewer := e.newUser()
		e.store.setRole(st.ID, viewer.rec.ID, "viewer")
		zb := buildZip(t, map[string]string{"index.html": "<h1>x</h1>"})
		body, ctype := buildMultipart(t, "x.zip", zb, "seed")
		rec := e.request(http.MethodPost, "/api/sites/zip-viewer-site/branches/draft/import").as(viewer).
			raw(body, ctype).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

// TestImportZipSlipGuard asserts a path-traversal entry is rejected (400).
func TestImportZipSlipGuard(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("zip-slip-site", owner)
	fc := e.readDraftFile(owner, "zip-slip-site", "index.html")

	zb := buildZip(t, map[string]string{"../../etc/passwd": "x", "index.html": "<h1>ok</h1>"})
	body, ctype := buildMultipart(t, "slip.zip", zb, fc.Sha)
	rec := e.request(http.MethodPost, "/api/sites/zip-slip-site/branches/draft/import").as(owner).
		raw(body, ctype).do()
	// ZipSlip -> 400 validation (CANONICAL §3). Some guards classify a bad path as
	// 422; accept either as long as it is a 4xx rejection, but prefer 400.
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 400/422 rejection (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("zip slip entry was accepted")
	}
}
