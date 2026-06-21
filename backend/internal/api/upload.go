package api

import (
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// multipartFormMem bounds how much of the multipart form is buffered in memory
// before parts spill to temp files. Kept small; the archive is streamed to a
// temp file by us regardless (we never hold the whole zip in RAM).
const multipartFormMem = 1 << 20 // 1 MiB

// importZip POST /api/sites/{handle}/branches/{branch}/import — streamed zip
// upload that REPLACES the branch tree as one commit (write capability).
//
// Security (CANONICAL §5.4/§5.6, architecture §8.1): the body is hard-capped at
// the configured max upload size with http.MaxBytesReader BEFORE any parsing,
// streamed to a private temp file (never fully buffered), then handed to
// site.ImportZip which applies ZipSlip + zip-bomb + extension guards on extract.
func (s *server) importZip(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capWrite)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")

	// HARD CAP: refuse anything beyond the configured max upload size before we
	// read a single byte of the body (memory + disk exhaustion defense). Add a
	// small headroom for the multipart envelope around the declared file size.
	maxBody := s.deps.Config.Zip.MaxUploadBytes + multipartFormMem
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseMultipartForm(multipartFormMem); err != nil {
		// A MaxBytesError here means the upload exceeded the cap -> 413.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, codeTooLarge, "the upload is too large", nil)
			return
		}
		writeError(w, http.StatusBadRequest, codeValidation, "malformed multipart form", validationDetails{Reason: "could not parse upload"})
		return
	}
	// ParseMultipartForm spills oversize parts to temp files owned by net/http;
	// ensure they are cleaned up regardless of outcome.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "the 'file' part is required", validationDetails{Field: "file", Reason: "required"})
		return
	}
	defer file.Close()

	// archive/zip needs io.ReaderAt + Size. Stream the part to our own temp file
	// so we control its lifecycle and never hold the whole archive in memory.
	tmp, terr := os.CreateTemp("", "kotoji-upload-*.zip")
	if terr != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "could not stage the upload", nil)
		return
	}
	// Remove the temp file on the way out (Close after Remove is fine on unix).
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	// CopyN with the cap+1 detects an over-cap stream even if Content-Length lied
	// (the MaxBytesReader above is the primary gate; this is belt-and-braces).
	written, cerr := io.CopyN(tmp, file, s.deps.Config.Zip.MaxUploadBytes+1)
	if cerr != nil && !errors.Is(cerr, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(cerr, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, codeTooLarge, "the upload is too large", nil)
			return
		}
		writeError(w, http.StatusBadRequest, codeValidation, "could not read the upload", validationDetails{Reason: "stream error"})
		return
	}
	if written > s.deps.Config.Zip.MaxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, codeTooLarge, "the upload is too large", nil)
		return
	}

	baseSha := r.FormValue("baseSha")

	ci, ierr := s.deps.Site.ImportZip(r.Context(), ac.site.ID, site.BranchName(branch), site.ZipSource{
		Reader:   tmp, // *os.File satisfies io.ReaderAt
		Size:     written,
		Filename: header.Filename,
	}, baseSha, actorFor(ac.user, site.SourceUpload))
	if ierr != nil {
		writeServiceError(w, ierr)
		return
	}

	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "import",
		Source:      gen.AuditSourceUpload,
		CommitSha:   emptyToNilStr(ci.SHA),
		Metadata:    auditMeta(map[string]any{"branch": branch, "filename": header.Filename, "bytes": written, "base_sha": baseSha}),
	})
	writeJSON(w, http.StatusOK, toCommitWire(ci))
}
