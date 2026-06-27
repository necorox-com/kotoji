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
	// is reached at {handle}.<baseDomain>, never via "--"). The base domain is the
	// EFFECTIVE value (env > DB > derived) so a runtime-configured instance links to
	// the right host.
	publishedURL := s.urlFor(r, string(ac.site.Handle))
	writeJSON(w, http.StatusOK, openapi.PublishResult{
		PublishedCommit: ci.SHA,
		PublishedUrl:    &publishedURL,
		From:            string(from),
		FromCommit:      body.BaseSha,
		Pushed:          false, // mirror-push is best-effort, not awaited at the edge
	})
}

// purgeSiteCache POST /api/sites/{handle}/cache/purge — operator "Clear cache".
// It bumps the per-site cache version (folded into every served asset ETag), so all
// clients refetch fresh content on their next revalidation WITHOUT requiring a new
// publish/commit. Gated by capPublish: the same role that can edit/publish (editor
// or owner) may purge; viewers are rejected (403).
func (s *server) purgeSiteCache(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capPublish)
	if !ok {
		return
	}

	// Bump the cache generation. The data plane reads sites.cache_version LIVE on
	// every request (the published served tree is an atomic-swap dir on disk that is
	// never held in an in-process cache — only PREVIEW trees are cached), so the new
	// version takes effect on the NEXT request with no explicit in-process resolver
	// invalidation needed here. This is the SAME mechanism that lets a publish serve
	// immediately: the handle->site read is always fresh.
	newVersion, err := s.deps.Store.BumpCacheVersion(r.Context(), ac.site.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	// EXTENSION POINT (upstream-CDN purge): if a future deployment fronts the data
	// plane with a shared/CDN cache (e.g. Cloudflare, a Traefik cache layer), this is
	// where an OPTIONAL outbound purge webhook would be invoked to evict the edge copy
	// in addition to bumping the client-facing ETag. Intentionally NOT implemented in
	// this pass: the ETag bump already forces origin revalidation, and an external
	// HTTP purge needs its own config (endpoint, auth, retry/timeout) + failure policy.

	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		Action:      "cache_purge",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"handle": string(ac.site.Handle), "cache_version": newVersion}),
	})

	cacheVersion := int(newVersion)
	writeJSON(w, http.StatusOK, openapi.CachePurgeResult{CacheVersion: cacheVersion})
}

// urlFor composes "<scheme>://<label>.<baseDomain>" for a host label, mirroring
// the MCP server's URL composition (dev returns scheme+label when no domain). The
// base domain is the EFFECTIVE value for this request (env > DB > derived): the
// Domain provider returns the static env value on the live fast path and the
// DB/derived value on the dynamic path. A nil provider (tests) falls back to the
// static cfg value, so behavior is unchanged where it is not wired.
func (s *server) urlFor(r *http.Request, label string) string {
	scheme := "https"
	if !s.deps.Config.IsProduction() {
		scheme = "http"
	}
	base := s.deps.Config.BaseDomain
	if s.deps.Domain != nil {
		base = s.deps.Domain.Resolve(r.Context(), r).BaseDomain.Value
	}
	if base == "" {
		return scheme + "://" + label
	}
	return scheme + "://" + label + "." + base
}
