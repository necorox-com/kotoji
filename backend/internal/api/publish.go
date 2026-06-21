package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// publish POST /api/sites/{handle}/publish — promote a source branch to
// published. Requires the publish capability AND (for non-owners) publish_mode
// 'direct' (CANONICAL §6.1: editors publish only in direct mode).
func (s *server) publish(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capPublish)
	if !ok {
		return
	}
	// publish_mode gating: an editor (non-owner) may only publish directly when
	// the site is in 'direct' mode; 'request' mode routes their publish through a
	// GitHub PR, so a direct publish is forbidden for them.
	if ac.role != gen.SiteRoleOwner && ac.site.PublishMode != "direct" {
		writeError(w, http.StatusForbidden, codeForbidden, "this site requires publish requests; only the owner can publish directly", nil)
		return
	}

	var body openapi.PublishRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	from := site.BranchDraft
	if body.From != nil && *body.From != "" {
		from = site.BranchName(*body.From)
	}

	ci, err := s.deps.Site.Publish(r.Context(), site.PublishInput{
		SiteID:  ac.site.ID,
		From:    from,
		BaseSHA: body.BaseSha,
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
		Action:      "publish",
		Source:      gen.AuditSourceEditor,
		CommitSha:   emptyToNilStr(ci.SHA),
		Metadata:    auditMeta(map[string]any{"from": string(from), "base_sha": body.BaseSha}),
	})

	// Compose the published URL from the bare handle host (CANONICAL §5: published
	// is reached at {handle}.<baseDomain>, never via "--").
	publishedURL := s.urlFor(string(ac.site.Handle))
	writeJSON(w, http.StatusOK, openapi.PublishResult{
		PublishedCommit: ci.SHA,
		PublishedUrl:    &publishedURL,
		From:            string(from),
		FromCommit:      body.BaseSha,
		Pushed:          false, // mirror-push is best-effort, not awaited at the edge
	})
}

// urlFor composes "<scheme>://<label>.<baseDomain>" for a host label, mirroring
// the MCP server's URL composition (dev returns scheme+label when no domain).
func (s *server) urlFor(label string) string {
	scheme := "https"
	if !s.deps.Config.IsProduction() {
		scheme = "http"
	}
	if s.deps.Config.BaseDomain == "" {
		return scheme + "://" + label
	}
	return scheme + "://" + label + "." + s.deps.Config.BaseDomain
}
