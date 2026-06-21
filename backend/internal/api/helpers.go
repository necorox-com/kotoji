package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// maxJSONBody bounds any JSON request body (defense against memory-exhaustion on
// the control plane; uploads use a separate, larger streamed cap).
const maxJSONBody = 1 << 20 // 1 MiB

// writeJSON emits a JSON body with the given status. Content-Type is set before
// the status line; an encode error after the header is unrecoverable (ignored).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON reads + strictly decodes a JSON request body into dst, capping the
// size and rejecting unknown fields and trailing data. On failure it writes a
// 400 validation envelope and returns false (handlers do `if !ok { return }`).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// A body larger than the cap, malformed JSON, or an unknown field are all
		// client errors -> 400 validation (never reaches the Service).
		writeError(w, http.StatusBadRequest, codeValidation, "invalid request body", validationDetails{Reason: clientDecodeReason(err)})
		return false
	}
	// Reject a second JSON value (trailing garbage) for a clean single-object body.
	if dec.More() {
		writeError(w, http.StatusBadRequest, codeValidation, "invalid request body", validationDetails{Reason: "unexpected trailing data"})
		return false
	}
	return true
}

// clientDecodeReason returns a short, safe reason string for a decode failure.
func clientDecodeReason(err error) string {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return "request body too large"
	}
	if errors.Is(err, io.EOF) {
		return "request body is empty"
	}
	return "malformed JSON"
}

// actorFor builds the git/audit Actor for a write, attributing it to the
// session user with the given provenance (CANONICAL §2 Actor). REST writes are
// always via the editor surface; uploads override Via to SourceUpload.
func actorFor(user *auth.SessionUser, via site.WriteSource) site.Actor {
	email := user.Email
	if email == "" {
		// Synthesize a stable author email when the IdP omitted one (CANONICAL §2).
		email = user.UserID.String() + "@kotoji.local"
	}
	name := user.DisplayName
	if name == "" {
		name = user.Email
	}
	return site.Actor{
		UserID: user.UserID,
		Name:   name,
		Email:  email,
		Via:    via,
	}
}

// ---- domain -> wire converters (site.* -> openapi.*) ----

// toSiteWire maps a site.Site to the openapi Site DTO (CANONICAL §8 names).
func toSiteWire(s site.Site) openapi.Site {
	return openapi.Site{
		Id:            s.ID,
		Handle:        string(s.Handle),
		OwnerId:       s.OwnerID,
		Visibility:    openapi.SiteVisibility(s.Visibility),
		DefaultBranch: string(s.DefaultBranch),
		GithubRepo:    emptyToNilStr(s.GitHubRepo),
		PublishMode:   openapi.PublishMode(s.PublishMode),
		WebRoot:       s.WebRoot,
		HasPublished:  s.HasPublished,
		PublishedSha:  emptyToNilStr(s.PublishedSHA),
		PublishedAt:   s.PublishedAt,
		Description:   s.Description,
		CreatedAt:     s.CreatedAt,
		UpdatedAt:     s.UpdatedAt,
	}
}

// toSiteSummary maps a site.Site + the caller's role to the SiteSummary DTO.
func toSiteSummary(s site.Site, role gen.SiteRole) openapi.SiteSummary {
	defaultBranch := string(s.DefaultBranch)
	desc := s.Description
	return openapi.SiteSummary{
		Id:            s.ID,
		Handle:        string(s.Handle),
		Visibility:    openapi.SiteVisibility(s.Visibility),
		DefaultBranch: &defaultBranch,
		PublishedSha:  emptyToNilStr(s.PublishedSHA),
		PublishedAt:   s.PublishedAt,
		HasPublished:  s.HasPublished,
		Description:   &desc,
		UpdatedAt:     s.UpdatedAt,
		Role:          openapi.SiteRole(role),
	}
}

// toCommitWire maps a site.CommitInfo to the openapi CommitInfo DTO.
func toCommitWire(c site.CommitInfo) openapi.CommitInfo {
	var parents *[]string
	if len(c.Parents) > 0 {
		p := c.Parents
		parents = &p
	}
	return openapi.CommitInfo{
		Sha:         c.SHA,
		ShortSha:    c.ShortSHA,
		Message:     c.Message,
		AuthorName:  c.AuthorName,
		AuthorEmail: c.AuthorEmail,
		Committed:   c.Committed,
		Parents:     parents,
		Via:         openapi.WriteSource(viaOrSystem(c.Via)),
	}
}

// viaOrSystem defaults an empty provenance to "system" so the WriteSource enum
// on the wire is always a valid value (CANONICAL §8 value set).
func viaOrSystem(via string) string {
	if via == "" {
		return string(site.SourceSystem)
	}
	return via
}

// toBranchWire maps a site.Branch to the openapi Branch DTO.
func toBranchWire(b site.Branch) openapi.Branch {
	lc := toCommitWire(b.LastCommit)
	return openapi.Branch{
		Name:             string(b.Name),
		HeadSha:          b.HeadSHA,
		IsPublished:      b.IsPublished,
		LastCommit:       &lc,
		PreviewSubdomain: b.PreviewSubdomain,
	}
}

// toFileEntryWire maps a site.FileEntry to the openapi FileEntry DTO.
func toFileEntryWire(e site.FileEntry) openapi.FileEntry {
	return openapi.FileEntry{
		Path:  e.Path,
		Name:  e.Name,
		IsDir: e.IsDir,
		Size:  e.Size,
		Mode:  e.Mode,
	}
}

// toFileDiffWire maps a site.FileDiff to the openapi FileDiff DTO.
func toFileDiffWire(d site.FileDiff) openapi.FileDiff {
	var oldPath *string
	if d.OldPath != "" {
		op := d.OldPath
		oldPath = &op
	}
	patch := d.UnifiedPatch
	return openapi.FileDiff{
		Path:         d.Path,
		OldPath:      oldPath,
		Status:       openapi.FileDiffStatus(d.Status),
		Additions:    d.Additions,
		Deletions:    d.Deletions,
		IsBinary:     d.IsBinary,
		UnifiedPatch: &patch,
	}
}

// emptyToNilStr returns nil for an empty string (for nullable wire fields).
func emptyToNilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// tsToTimePtr converts a pgtype.Timestamptz to *time.Time (nil when NULL).
func tsToTimePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// auditBestEffort appends an audit row, logging (not failing) on error. The
// audit trail is observability, never on the request's critical path.
func (s *server) auditBestEffort(ctx context.Context, arg gen.InsertAuditParams) {
	if err := s.deps.Store.InsertAudit(ctx, arg); err != nil && s.deps.Logger != nil {
		s.deps.Logger.WarnContext(ctx, "audit insert failed", "action", arg.Action, "err", err)
	}
}

// uuidPtr returns a pointer to a uuid (for nullable audit FK columns).
func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }

// auditMeta marshals an audit metadata map to JSONB bytes. A marshal failure
// (practically impossible for plain maps) degrades to an empty object so the
// audit insert never blocks the request.
func auditMeta(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// parseInt32 parses a base-10 int32 from a query value.
func parseInt32(s string) (int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	return int32(n), err
}

// parseInt64 parses a base-10 int64 from a query value.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// clampInt32 clamps n into [lo, hi].
func clampInt32(n, lo, hi int32) int32 {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
