package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// defaultLogLimit / maxLogLimit bound the history page size (CANONICAL LogOptions
// + openapi getLog: default 50, max 500). The Service clamps too; we pre-clamp so
// the cursor math (nextBefore) stays predictable.
const (
	defaultLogLimit = 50
	maxLogLimit     = 500
)

// commit POST /api/sites/{handle}/branches/{branch}/commit — turn the staged
// working set into one commit (the multi-file "Save" verb). Write capability.
func (s *server) commit(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capWrite)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")

	var body openapi.CommitRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	ci, err := s.deps.Site.Commit(r.Context(), site.CommitInput{
		SiteID:  ac.site.ID,
		Branch:  site.BranchName(branch),
		Message: derefStr(body.Message),
		BaseSHA: body.BaseSha,
		Actor:   actorFor(ac.user, site.SourceEditor),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "commit",
		Source:      gen.AuditSourceEditor,
		CommitSha:   emptyToNilStr(ci.SHA),
		Metadata:    auditMeta(map[string]any{"branch": branch, "base_sha": body.BaseSha}),
	})
	writeJSON(w, http.StatusOK, openapi.WriteResult{
		Path:      "",
		Branch:    branch,
		Committed: true,
		Commit:    ci.SHA,
		Pushed:    false,
	})
}

// getLog GET /api/sites/{handle}/branches/{branch}/log — paginated history.
func (s *server) getLog(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capRead)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")
	q := r.URL.Query()

	limit := defaultLogLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = defaultLogLimit
	}
	if limit > maxLogLimit {
		limit = maxLogLimit
	}

	// Fetch one extra row to detect whether there is a further page; trim it back
	// to `limit` and surface its predecessor's SHA as the next cursor.
	commits, err := s.deps.Site.GetLog(r.Context(), site.LogOptions{
		SiteID: ac.site.ID,
		Branch: site.BranchName(branch),
		Limit:  limit + 1,
		Before: q.Get("before"),
		Path:   q.Get("path"),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}

	var nextBefore *string
	if len(commits) > limit {
		// The last item of the trimmed page is the cursor for the next request
		// (Before is exclusive, so the next page starts after it).
		last := commits[limit-1].SHA
		nextBefore = &last
		commits = commits[:limit]
	}

	out := make([]openapi.CommitInfo, 0, len(commits))
	for _, c := range commits {
		out = append(out, toCommitWire(c))
	}
	writeJSON(w, http.StatusOK, openapi.LogResult{
		Branch:     branch,
		Commits:    out,
		NextBefore: nextBefore,
	})
}

// getDiff GET /api/sites/{handle}/diff — diff two refs (or a ref vs its working
// tree when `to` is omitted).
func (s *server) getDiff(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	from := q.Get("from")
	if from == "" {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "from is required", validationDetails{Field: "from", Reason: "required"})
		return
	}

	contextLines := 3
	if v := q.Get("contextLines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			contextLines = n
		}
	}

	res, err := s.deps.Site.GetDiff(r.Context(), site.DiffOptions{
		SiteID:       ac.site.ID,
		From:         from,
		To:           q.Get("to"),
		Path:         q.Get("path"),
		ContextLines: contextLines,
		NameStatus:   q.Get("nameStatus") == "true",
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	files := make([]openapi.FileDiff, 0, len(res.Files))
	for _, d := range res.Files {
		files = append(files, toFileDiffWire(d))
	}
	writeJSON(w, http.StatusOK, openapi.DiffResult{
		FromSha: res.FromSHA,
		ToSha:   res.ToSHA,
		Files:   files,
	})
}

// rollback POST /api/sites/{handle}/branches/{branch}/rollback — restore a
// branch to an ancestor commit's tree as a NEW forward commit. Write capability.
func (s *server) rollback(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capWrite)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")

	var body openapi.RollbackRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	ci, err := s.deps.Site.Rollback(r.Context(), ac.site.ID, site.BranchName(branch), body.ToSha, body.BaseSha, actorFor(ac.user, site.SourceEditor))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "rollback",
		Source:      gen.AuditSourceEditor,
		CommitSha:   emptyToNilStr(ci.SHA),
		Metadata:    auditMeta(map[string]any{"branch": branch, "to_sha": body.ToSha, "base_sha": body.BaseSha}),
	})
	writeJSON(w, http.StatusOK, toCommitWire(ci))
}
