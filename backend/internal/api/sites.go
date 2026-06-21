package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// listSites GET /api/sites — sites the caller owns or is a member of, with role.
func (s *server) listSites(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.CurrentUser(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "authentication required", nil)
		return
	}

	// ListSites is membership-filtered by the Service's Store (CANONICAL §1). It
	// returns the sites visible to the user; the per-site role badge is resolved
	// separately so the summary carries it (openapi SiteSummary.role).
	sites, err := s.deps.Site.ListSites(r.Context(), user.UserID)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	out := make([]openapi.SiteSummary, 0, len(sites))
	for _, st := range sites {
		role, _ := s.effectiveRole(r.Context(), st.ID, user)
		out = append(out, toSiteSummary(st, role))
	}
	writeJSON(w, http.StatusOK, struct {
		Sites []openapi.SiteSummary `json:"sites"`
	}{Sites: out})
}

// createSite POST /api/sites — create a draft site. Gated by can_create_sites.
func (s *server) createSite(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.CurrentUser(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthenticated, "authentication required", nil)
		return
	}
	// Site-creation capability is an instance-level gate (decision #2/#8): the
	// user must be permitted to create sites (admins are implicitly permitted).
	if !user.CanCreateSites && !user.IsAdmin {
		writeError(w, http.StatusForbidden, codeForbidden, "you are not allowed to create sites", nil)
		return
	}

	var body openapi.CreateSiteRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	in := site.CreateSiteInput{
		Handle:      site.Handle(body.Handle),
		OwnerID:     user.UserID,
		Description: derefStr(body.Description),
		GitHubRepo:  derefStr(body.GithubRepo),
		Actor:       actorFor(user, site.SourceEditor),
	}
	if body.Visibility != nil {
		in.Visibility = string(*body.Visibility)
	}
	if body.PublishMode != nil {
		in.PublishMode = string(*body.PublishMode)
	}

	created, err := s.deps.Site.CreateSite(r.Context(), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(user.UserID),
		SiteID:      uuidPtr(created.ID),
		Action:      "site.create",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"handle": string(created.Handle)}),
	})
	writeJSON(w, http.StatusCreated, toSiteWire(created))
}

// getSite GET /api/sites/{handle} — site detail (current handle only).
func (s *server) getSite(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toSiteWire(ac.site))
}

// updateSite PATCH /api/sites/{handle} — owner-only settings edit.
func (s *server) updateSite(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}

	var body openapi.UpdateSiteRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	// Settings live in the metadata store; the Service does not own a generic
	// "update settings" verb, so we patch via the Store and re-read the site for
	// the response (single source of truth stays the DB row). github_repo, when
	// provided, is configured through the Service (it owns the git remote).
	cur := ac.site
	vis := string(cur.Visibility)
	if body.Visibility != nil {
		vis = string(*body.Visibility)
	}
	pubMode := cur.PublishMode
	if body.PublishMode != nil {
		pubMode = string(*body.PublishMode)
	}
	webRoot := cur.WebRoot
	if body.WebRoot != nil {
		webRoot = *body.WebRoot
	}
	desc := cur.Description
	if body.Description != nil {
		desc = *body.Description
	}

	if err := s.deps.Store.UpdateSiteSettings(r.Context(), gen.UpdateSiteSettingsParams{
		Visibility:  gen.SiteVisibility(vis),
		PublishMode: pubMode,
		WebRoot:     webRoot,
		Description: desc,
		ID:          cur.ID,
	}); err != nil {
		writeServiceError(w, err)
		return
	}

	// github_repo is the Service's concern (it sets the mirror remote on disk).
	if body.GithubRepo != nil {
		if err := s.deps.Site.SetRemote(r.Context(), cur.ID, derefStr(body.GithubRepo)); err != nil {
			writeServiceError(w, err)
			return
		}
	}

	updated, err := s.deps.Site.GetSite(r.Context(), cur.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(cur.ID),
		Action:      "site.update",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"handle": string(cur.Handle)}),
	})
	writeJSON(w, http.StatusOK, toSiteWire(updated))
}

// deleteSite DELETE /api/sites/{handle} — owner-only soft delete.
func (s *server) deleteSite(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}
	if err := s.deps.Site.DeleteSite(r.Context(), ac.site.ID, actorFor(ac.user, site.SourceEditor)); err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "site.delete",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"handle": string(ac.site.Handle)}),
	})
	w.WriteHeader(http.StatusNoContent)
}

// renameSite POST /api/sites/{handle}/rename — owner-only handle rename.
func (s *server) renameSite(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}

	var body openapi.RenameSiteJSONRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}

	renamed, err := s.deps.Site.RenameHandle(r.Context(), ac.site.ID, site.Handle(body.NewHandle))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "site.rename",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"from": string(ac.site.Handle), "to": string(renamed.Handle)}),
	})
	writeJSON(w, http.StatusOK, toSiteWire(renamed))
}

// derefStr returns the pointed-to string, or "" when nil.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
