package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// TestCommitHandler exercises the multi-file "save" verb. The FakeService models
// no separate staging area (WriteFile commits per call), so a bare Commit with
// no pending changes is the "nothing to commit" path -> 409. This still drives
// the commit handler end-to-end (decode -> Service -> error envelope mapping).
func TestCommitHandler(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("commit-site", owner)
	fc := e.readDraftFile(owner, "commit-site", "index.html")

	// Empty baseSha is a 422 validation (never treated as force).
	bad := e.request(http.MethodPost, "/api/sites/commit-site/branches/draft/commit").as(owner).
		json(openapi.CommitRequest{BaseSha: ""}).do()
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty baseSha status = %d, want 422 (body=%s)", bad.Code, bad.Body.String())
	}

	// Correct tip but nothing staged -> 409 nothing_to_commit.
	crec := e.request(http.MethodPost, "/api/sites/commit-site/branches/draft/commit").as(owner).
		json(openapi.CommitRequest{BaseSha: fc.Sha}).do()
	if crec.Code != http.StatusConflict {
		t.Fatalf("commit status = %d, want 409 (body=%s)", crec.Code, crec.Body.String())
	}
	if env := errEnvelope(t, crec); env.Error.Code != codeNothingToCommit {
		t.Fatalf("code = %q, want nothing_to_commit", env.Error.Code)
	}
}

// TestDiffSuccess exercises the diff success path between two refs.
func TestDiffSuccess(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("diff-ok-site", owner)
	fc := e.readDraftFile(owner, "diff-ok-site", "index.html")

	// Advance the tip so there are two refs to diff.
	wrec := e.request(http.MethodPut, "/api/sites/diff-ok-site/branches/draft/file").as(owner).
		json(openapi.WriteFileRequest{Path: "index.html", Content: "<h1>v2</h1>", BaseSha: fc.Sha}).do()
	if wrec.Code != http.StatusOK {
		t.Fatalf("write status = %d", wrec.Code)
	}
	var wr openapi.WriteResult
	decodeBody(t, wrec, &wr)

	rec := e.request(http.MethodGet, "/api/sites/diff-ok-site/diff?from="+fc.Sha+"&to="+wr.Commit).as(owner).do()
	if rec.Code != http.StatusOK {
		t.Fatalf("diff status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var got openapi.DiffResult
	decodeBody(t, rec, &got)
	if got.FromSha == "" || got.ToSha == "" {
		t.Fatalf("diff result missing refs: %+v", got)
	}
}

// TestMalformedJSONBody asserts a malformed JSON body is a clean 400 validation.
func TestMalformedJSONBody(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	rec := e.request(http.MethodPost, "/api/sites").as(owner).
		raw(strings.NewReader("{not json"), "application/json").do()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if env := errEnvelope(t, rec); env.Error.Code != codeValidation {
		t.Fatalf("code = %q, want validation", env.Error.Code)
	}
}

// TestUnknownJSONField asserts unknown fields are rejected (strict decoding).
func TestUnknownJSONField(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	rec := e.request(http.MethodPost, "/api/sites").as(owner).
		raw(strings.NewReader(`{"handle":"ok-handle","bogus":1}`), "application/json").do()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestStatusAndCodeMapping is the table-driven mirror of CANONICAL §3: every
// sentinel maps to the documented status + machine code. This guards against
// drift between the API layer's mapping and the site taxonomy.
func TestStatusAndCodeMapping(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{nil, http.StatusOK, ""},
		{site.ErrNotFound, http.StatusNotFound, codeNotFound},
		{site.ErrForbidden, http.StatusForbidden, codeForbidden},
		{site.ErrConflict, http.StatusConflict, codeConflict},
		{site.ErrHandleTaken, http.StatusConflict, codeHandleTaken},
		{site.ErrPublishConflict, http.StatusConflict, codePublishConflict},
		{site.ErrBranchExists, http.StatusConflict, codeBranchExists},
		{site.ErrNothingToCommit, http.StatusConflict, codeNothingToCommit},
		{site.ErrZipTooLarge, http.StatusRequestEntityTooLarge, codeTooLarge},
		{site.ErrZipTooManyFiles, http.StatusRequestEntityTooLarge, codeTooLarge},
		{site.ErrZipBadType, http.StatusUnsupportedMediaType, codeUnsupportedMediaType},
		{site.ErrZipSlip, http.StatusBadRequest, codeValidation},
		{site.ErrValidation, http.StatusUnprocessableEntity, codeValidation},
		{site.ErrReservedHandle, http.StatusUnprocessableEntity, codeValidation},
		{site.ErrGit, http.StatusInternalServerError, codeInternal},
		{errors.New("unknown"), http.StatusInternalServerError, codeInternal},
	}
	for _, tc := range cases {
		gotStatus, gotCode := statusAndCode(tc.err)
		if gotStatus != tc.wantStatus || gotCode != tc.wantCode {
			t.Fatalf("statusAndCode(%v) = (%d,%q), want (%d,%q)", tc.err, gotStatus, gotCode, tc.wantStatus, tc.wantCode)
		}
	}
	// safeMessageFor must produce a non-empty message for every code.
	for _, code := range []string{codeNotFound, codeForbidden, codeUnauthenticated, codeHandleTaken, codePublishConflict, codeBranchExists, codeNothingToCommit, codeConflict, codeTooLarge, codeUnsupportedMediaType, codeValidation, codeRateLimited, codeQuotaExceeded, codeInternal, "weird"} {
		if safeMessageFor(code) == "" {
			t.Fatalf("safeMessageFor(%q) is empty", code)
		}
	}
}

// TestWriteServiceErrorEnvelopes asserts the typed errors produce their
// structured detail envelopes (conflict/publish-conflict/validation).
func TestWriteServiceErrorEnvelopes(t *testing.T) {
	t.Run("publish conflict carries paths", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeServiceError(rec, &site.PublishConflictError{Paths: []string{"a.html", "b.css"}})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "a.html") {
			t.Fatalf("body missing conflict path: %s", rec.Body.String())
		}
	})
	t.Run("validation carries field", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeServiceError(rec, &site.ValidationError{Field: "handle", Reason: "too short"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "handle") {
			t.Fatalf("body missing field: %s", rec.Body.String())
		}
	})
}
