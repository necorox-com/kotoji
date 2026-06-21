package api

import (
	"encoding/base64"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// listFiles GET /api/sites/{handle}/branches/{branch}/files — directory listing.
func (s *server) listFiles(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capRead)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")
	q := r.URL.Query()

	entries, ref, err := s.deps.Site.ListFiles(r.Context(), site.ListFilesInput{
		SiteID:    ac.site.ID,
		Branch:    site.BranchName(branch),
		Dir:       q.Get("dir"),
		Ref:       q.Get("ref"),
		Recursive: q.Get("recursive") == "true",
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	files := make([]openapi.FileEntry, 0, len(entries))
	for _, e := range entries {
		files = append(files, toFileEntryWire(e))
	}
	writeJSON(w, http.StatusOK, openapi.FileListing{
		Branch: branch,
		Ref:    ref.SHA,
		Files:  files,
	})
}

// readFile GET /api/sites/{handle}/branches/{branch}/file — one file's content.
// `sha` in the response is the COMMIT lock token to echo as baseSha on write.
func (s *server) readFile(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capRead)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "path is required", validationDetails{Field: "path", Reason: "required"})
		return
	}

	fc, err := s.deps.Site.ReadFile(r.Context(), ac.site.ID, site.BranchName(branch), q.Get("ref"), path)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	// Binary content (or content that is not valid UTF-8) is base64-encoded on the
	// wire; text is sent verbatim as utf-8 (openapi FileContent.encoding).
	encoding := openapi.FileContentEncodingUtf8
	content := string(fc.Content)
	if fc.IsBinary || !utf8.Valid(fc.Content) {
		encoding = openapi.FileContentEncodingBase64
		content = base64.StdEncoding.EncodeToString(fc.Content)
	}

	writeJSON(w, http.StatusOK, openapi.FileContent{
		Path:     fc.Path,
		Sha:      fc.SHA,
		BlobSha:  fc.BlobSHA,
		Encoding: encoding,
		Content:  content,
		Size:     fc.Size,
		IsBinary: fc.IsBinary,
	})
}

// writeFile PUT /api/sites/{handle}/branches/{branch}/file — optimistic-locked
// single-file write (baseSha REQUIRED).
func (s *server) writeFile(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capWrite)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")

	var body openapi.WriteFileRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	// Decode the payload per its declared encoding. A bad base64 body is a client
	// error (400) before the Service sees it.
	content, derr := decodeContent(body.Content, body.Encoding)
	if derr != nil {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid content encoding", validationDetails{Field: "content", Reason: derr.Error()})
		return
	}

	// commit defaults to true at the edge (openapi default) — a stage-only write
	// (commit:false) is the multi-file batch path finished by POST .../commit.
	commit := true
	if body.Commit != nil {
		commit = *body.Commit
	}

	ci, err := s.deps.Site.WriteFile(r.Context(), site.WriteFileInput{
		SiteID:  ac.site.ID,
		Branch:  site.BranchName(branch),
		Path:    body.Path,
		Content: content,
		BaseSHA: body.BaseSha,
		Commit:  commit,
		Message: derefStr(body.Message),
		Actor:   actorFor(ac.user, site.SourceEditor),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}

	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "file.write",
		Source:      gen.AuditSourceEditor,
		CommitSha:   emptyToNilStr(ci.SHA),
		Metadata:    auditMeta(map[string]any{"branch": branch, "path": body.Path, "base_sha": body.BaseSha}),
	})

	bytesWritten := len(content)
	writeJSON(w, http.StatusOK, openapi.WriteResult{
		Path:         body.Path,
		Branch:       branch,
		Committed:    commit,
		Commit:       ci.SHA,
		BytesWritten: &bytesWritten,
		Pushed:       false, // mirror-push is best-effort and not awaited at the edge
	})
}

// deleteFile DELETE /api/sites/{handle}/branches/{branch}/file — optimistic-
// locked delete (path + baseSha required as query params).
func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capWrite)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")
	q := r.URL.Query()
	path := q.Get("path")
	baseSha := q.Get("baseSha")
	if path == "" || baseSha == "" {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "path and baseSha are required", validationDetails{Reason: "path and baseSha are required"})
		return
	}

	ci, err := s.deps.Site.DeleteFile(r.Context(), ac.site.ID, site.BranchName(branch), path, baseSha, actorFor(ac.user, site.SourceEditor))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "file.delete",
		Source:      gen.AuditSourceEditor,
		CommitSha:   emptyToNilStr(ci.SHA),
		Metadata:    auditMeta(map[string]any{"branch": branch, "path": path, "base_sha": baseSha}),
	})
	writeJSON(w, http.StatusOK, toCommitWire(ci))
}

// decodeContent decodes a WriteFileRequest payload per its encoding. utf-8 (the
// default) is verbatim; base64 is decoded; an invalid base64 string is an error.
func decodeContent(content string, encoding *openapi.WriteFileRequestEncoding) ([]byte, error) {
	if encoding != nil && *encoding == openapi.WriteFileRequestEncodingBase64 {
		return base64.StdEncoding.DecodeString(content)
	}
	return []byte(content), nil
}
