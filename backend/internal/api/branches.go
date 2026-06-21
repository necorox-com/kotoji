package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// listBranches GET /api/sites/{handle}/branches — enumerate git branches.
func (s *server) listBranches(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capRead)
	if !ok {
		return
	}
	branches, err := s.deps.Site.ListBranches(r.Context(), ac.site.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]openapi.Branch, 0, len(branches))
	for _, b := range branches {
		out = append(out, toBranchWire(b))
	}
	writeJSON(w, http.StatusOK, struct {
		Branches []openapi.Branch `json:"branches"`
	}{Branches: out})
}

// createBranch POST /api/sites/{handle}/branches — branch from a ref (write cap).
func (s *server) createBranch(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capWrite)
	if !ok {
		return
	}
	var body openapi.CreateBranchJSONRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}
	br, err := s.deps.Site.CreateBranch(r.Context(), ac.site.ID, site.BranchName(body.Name), body.From)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "branch.create",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"branch": body.Name, "from": body.From}),
	})
	writeJSON(w, http.StatusCreated, toBranchWire(br))
}

// deleteBranch DELETE /api/sites/{handle}/branches/{branch} — write cap; the
// Service refuses published/draft (ErrValidation -> 422).
func (s *server) deleteBranch(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capWrite)
	if !ok {
		return
	}
	branch := chi.URLParam(r, "branch")
	if err := s.deps.Site.DeleteBranch(r.Context(), ac.site.ID, site.BranchName(branch)); err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "branch.delete",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"branch": branch}),
	})
	w.WriteHeader(http.StatusNoContent)
}
