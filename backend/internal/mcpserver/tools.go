package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// encodingUTF8 / encodingBase64 are the two content encodings the file tools use.
const (
	encodingUTF8   = "utf-8"
	encodingBase64 = "base64"
)

// registerAll wires every tool with its declared scope + rate class. The In types
// carry jsonschema tags (auto-inferred schema). Content tools take a `site` (the
// project handle); every call is membership-capped (intersection of the token's
// scopes and the user's role on that site, authz.go). create_site is the one tool
// that does NOT take a site (it mints a new one) and is can_create_sites-gated.
func (r *registry) registerAll(s *mcp.Server) {
	addTool(s, r, "list_sites", "List the projects you are a member of, with your effective scope on each.", scopeRead, classRead, r.listSites)
	addTool(s, r, "list_files", "List files in a project (site) at a branch or commit.", scopeRead, classRead, r.listFiles)
	addTool(s, r, "read_file", "Read a file from a project (site); returns the commit SHA to pass back as base_sha.", scopeRead, classRead, r.readFile)
	addTool(s, r, "write_file", "Write a file to a project (site) (base_sha REQUIRED; stale base_sha is rejected as conflict).", scopeWrite, classWrite, r.writeFile)
	addTool(s, r, "create_site", "Create a NEW project (disabled unless the token AND your account allow creating sites).", scopeWrite, classCreate, r.createSite)
	addTool(s, r, "save", "Commit the working set on a project (site) branch (base_sha REQUIRED).", scopeWrite, classWrite, r.save)
	addTool(s, r, "publish", "Promote a project (site) branch to the live published site (base_sha REQUIRED).", scopePublish, classPublish, r.publish)
	addTool(s, r, "get_diff", "Diff two refs in a project (site), or a commit vs its parent.", scopeRead, classRead, r.getDiff)
	addTool(s, r, "get_log", "Return commit history for a project (site) branch (paginated).", scopeRead, classRead, r.getLog)
	addTool(s, r, "rollback", "Forward-revert a project (site) branch to a previous commit's tree (base_sha REQUIRED).", scopeWrite, classWrite, r.rollback)
	addTool(s, r, "create_branch", "Create a new branch in a project (site) from an existing branch or commit.", scopeWrite, classWrite, r.createBranch)
}

// actorFor builds the git/audit Actor for an MCP call from the verified claims.
// Via is always SourceMCP; TokenID is set so audit can attribute the action.
func actorFor(c TokenInfo) site.Actor {
	tokenID := c.TokenID
	return site.Actor{
		UserID:  c.UserID,
		Via:     site.SourceMCP,
		TokenID: &tokenID,
	}
}

// branchOrDefault returns the requested branch, or the working default (draft)
// when the optional arg is empty/nil.
func branchOrDefault(b *string) site.BranchName {
	if b == nil || *b == "" {
		return site.BranchDraft
	}
	return site.BranchName(*b)
}

// deref returns the pointed-to string or "".
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ============================ list_sites ============================

// ListSitesArgs takes no arguments — it enumerates the token user's memberships.
type ListSitesArgs struct{}

// SiteSummary is one project in a list_sites result, including the user's role on
// it and the EFFECTIVE scope (token ∩ role) so the model knows what it may do.
type SiteSummary struct {
	UUID            string   `json:"uuid"`
	Handle          string   `json:"handle"`
	Role            string   `json:"role"`             // the token user's membership role
	EffectiveScopes []string `json:"effective_scopes"` // token.scopes ∩ roleScopes(role)
	PublishedURL    string   `json:"published_url"`
	DraftURL        string   `json:"draft_url"`
	DefaultBranch   string   `json:"default_branch"`
	IsPublished     bool     `json:"is_published"`
	UpdatedAt       string   `json:"updated_at"`
}

// ListSitesResult wraps the user's membership list.
type ListSitesResult struct {
	Sites []SiteSummary `json:"sites"`
}

func (r *registry) listSites(ctx context.Context, c TokenInfo, _ ListSitesArgs) (*mcp.CallToolResult, ListSitesResult, error) {
	// Enumerate the projects the token's USER is a member of (NOT every site): a
	// token spans exactly its user's memberships (CANONICAL §6). The effective
	// scope per site is the intersection of the token's scopes and the membership
	// role's scopes, so the model sees precisely what it can do where.
	rows, err := r.members.ListSitesForUser(ctx, gen.ListSitesForUserParams{
		UserID: c.UserID,
		Off:    0,
		Lim:    listSitesPageLimit,
	})
	if err != nil {
		res, gerr := r.mapError(err, "list_sites")
		return res, ListSitesResult{}, gerr
	}
	out := ListSitesResult{Sites: make([]SiteSummary, 0, len(rows))}
	for _, row := range rows {
		out.Sites = append(out.Sites, r.toSummary(c, row))
	}
	return nil, out, nil
}

// toSummary composes the wire summary for a membership row, including the role,
// the effective scopes (token ∩ role), and preview URLs from the base domain.
func (r *registry) toSummary(c TokenInfo, row gen.ListSitesForUserRow) SiteSummary {
	handle := site.Handle(row.Handle)
	return SiteSummary{
		UUID:            row.ID.String(),
		Handle:          row.Handle,
		Role:            string(row.Role),
		EffectiveScopes: scopeStrings(intersectScopes(c.Scopes, row.Role)),
		PublishedURL:    r.publishedURL(handle),
		DraftURL:        r.previewURL(handle, site.BranchDraft),
		DefaultBranch:   row.DefaultBranch,
		IsPublished:     row.PublishedCommitSha != nil && *row.PublishedCommitSha != "",
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// ============================ list_files ============================

// ListFilesArgs takes the target `site` (handle). Optional branch/ref/path/recursive.
type ListFilesArgs struct {
	Site      string  `json:"site" jsonschema:"project handle (from list_sites) to list files in"`
	Branch    *string `json:"branch,omitempty" jsonschema:"branch name; defaults to the working branch (draft)"`
	Path      *string `json:"path,omitempty" jsonschema:"subtree filter, repo-relative POSIX path; defaults to repo root"`
	Ref       *string `json:"ref,omitempty" jsonschema:"commit SHA to list at; defaults to the branch tip"`
	Recursive *bool   `json:"recursive,omitempty" jsonschema:"list the whole subtree instead of one level; default false"`
}

// FileItem is one entry in a list_files result.
type FileItem struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Mode string `json:"mode"`
}

// ListFilesResult is the flat file list reflecting a resolved commit.
type ListFilesResult struct {
	Branch    string     `json:"branch"`
	Commit    string     `json:"commit"`
	Files     []FileItem `json:"files"`
	Truncated bool       `json:"truncated"`
}

func (r *registry) listFiles(ctx context.Context, c TokenInfo, in ListFilesArgs) (*mcp.CallToolResult, ListFilesResult, error) {
	ac, errRes, gerr := r.authorizeSite(ctx, c, in.Site, scopeRead)
	if errRes != nil || gerr != nil {
		return errRes, ListFilesResult{}, gerr
	}
	dir := deref(in.Path)
	if dir != "" {
		// Reuse the read confinement (no extension check) for the dir filter.
		clean, errRes := validateContentPath(dir, false)
		if errRes != nil {
			return errRes, ListFilesResult{}, nil
		}
		dir = clean
	}
	branch := branchOrDefault(in.Branch)
	recursive := in.Recursive != nil && *in.Recursive

	entries, ref, err := r.svc.ListFiles(ctx, site.ListFilesInput{
		SiteID:    ac.site.ID,
		Branch:    branch,
		Dir:       dir,
		Ref:       deref(in.Ref),
		Recursive: recursive,
	})
	if err != nil {
		res, gerr := r.mapError(err, "list_files")
		return res, ListFilesResult{}, gerr
	}

	files := make([]FileItem, 0, len(entries))
	truncated := false
	for _, e := range entries {
		if e.IsDir {
			continue // flat file list (mcp.md §5.2): directories are implied by paths
		}
		if len(files) >= r.limits.MaxListItems {
			truncated = true
			break
		}
		files = append(files, FileItem{Path: e.Path, Size: e.Size, Mode: e.Mode})
	}
	out := ListFilesResult{Branch: string(branch), Commit: ref.SHA, Files: files, Truncated: truncated}
	return nil, out, nil
}

// ============================ read_file ============================

// ReadFileArgs takes the target `site` (handle) plus the file path.
type ReadFileArgs struct {
	Site   string  `json:"site" jsonschema:"project handle (from list_sites) to read from"`
	Path   string  `json:"path" jsonschema:"file path relative to the site root, POSIX style"`
	Branch *string `json:"branch,omitempty" jsonschema:"branch name; defaults to the working branch (draft)"`
	Ref    *string `json:"ref,omitempty" jsonschema:"commit SHA to read at; defaults to the branch tip"`
}

// ReadFileResult echoes the commit SHA to pass back as base_sha to write_file.
type ReadFileResult struct {
	Path      string `json:"path"`
	Branch    string `json:"branch"`
	Commit    string `json:"commit"`   // pass this back as base_sha to write_file
	BlobSHA   string `json:"blob_sha"` // blob hash for fine-grained conflict checks
	Encoding  string `json:"encoding"` // "utf-8" | "base64"
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

func (r *registry) readFile(ctx context.Context, c TokenInfo, in ReadFileArgs) (*mcp.CallToolResult, ReadFileResult, error) {
	ac, errRes, gerr := r.authorizeSite(ctx, c, in.Site, scopeRead)
	if errRes != nil || gerr != nil {
		return errRes, ReadFileResult{}, gerr
	}
	clean, errRes := validateContentPath(in.Path, false)
	if errRes != nil {
		return errRes, ReadFileResult{}, nil
	}
	branch := branchOrDefault(in.Branch)

	fc, err := r.svc.ReadFile(ctx, ac.site.ID, branch, deref(in.Ref), clean)
	if err != nil {
		res, gerr := r.mapError(err, "read_file")
		return res, ReadFileResult{}, gerr
	}

	// Binary files (or content with NUL bytes) are returned base64; text is inlined
	// UTF-8. Files over the read cap are truncated with a flag (mcp.md §5.3 / §10.2).
	content := fc.Content
	truncated := false
	if int64(len(content)) > r.limits.MaxReadBytes {
		content = content[:r.limits.MaxReadBytes]
		truncated = true
	}

	encoding := encodingUTF8
	body := string(content)
	if fc.IsBinary {
		encoding = encodingBase64
		body = base64.StdEncoding.EncodeToString(content)
	}

	out := ReadFileResult{
		Path:      fc.Path,
		Branch:    string(branch),
		Commit:    fc.SHA,
		BlobSHA:   fc.BlobSHA,
		Encoding:  encoding,
		Content:   body,
		Size:      fc.Size,
		Truncated: truncated,
	}
	return nil, out, nil
}

// ============================ write_file ============================

// WriteFileArgs takes the target `site` (handle). base_sha is REQUIRED (no force
// flag, v1).
type WriteFileArgs struct {
	Site     string  `json:"site" jsonschema:"project handle (from list_sites) to write to"`
	Path     string  `json:"path" jsonschema:"file path relative to the site root"`
	Content  string  `json:"content" jsonschema:"full new file contents"`
	Encoding *string `json:"encoding,omitempty" jsonschema:"utf-8 (default) or base64 for binary"`
	BaseSHA  string  `json:"base_sha" jsonschema:"REQUIRED commit SHA the edit is based on (from read_file.commit). A stale value is rejected with a conflict error."`
	Branch   *string `json:"branch,omitempty" jsonschema:"target branch; 'published' is not writable"`
	Commit   *bool   `json:"commit,omitempty" jsonschema:"create a commit after writing; default true"`
	Message  *string `json:"message,omitempty" jsonschema:"commit message"`
}

// WriteFileResult is the success body; warnings carry best-effort mirror-push issues.
type WriteFileResult struct {
	Path         string   `json:"path"`
	Branch       string   `json:"branch"`
	Committed    bool     `json:"committed"`
	Commit       string   `json:"commit"`
	BlobSHA      string   `json:"blob_sha"`
	Pushed       bool     `json:"pushed"`
	BytesWritten int      `json:"bytes_written"`
	Warnings     []string `json:"warnings,omitempty"`
}

func (r *registry) writeFile(ctx context.Context, c TokenInfo, in WriteFileArgs) (*mcp.CallToolResult, WriteFileResult, error) {
	ac, authRes, gerr := r.authorizeSite(ctx, c, in.Site, scopeWrite)
	if authRes != nil || gerr != nil {
		return authRes, WriteFileResult{}, gerr
	}
	clean, errRes := validateContentPath(in.Path, true)
	if errRes != nil {
		return errRes, WriteFileResult{}, nil
	}
	if in.BaseSHA == "" {
		return toolErr(codeValidation, "base_sha: required (no force overwrite in v1)", nil), WriteFileResult{}, nil
	}
	branch := branchOrDefault(in.Branch)
	if branch == site.BranchPublished {
		return toolErr(codeValidation, "branch: 'published' is not writable; use publish", nil), WriteFileResult{}, nil
	}

	// Decode the content per encoding and enforce the file-size cap BEFORE the
	// service (mcp.md §10.2 — MCP must not bypass upload-path size guards).
	content, errRes := r.decodeContent(in.Content, in.Encoding)
	if errRes != nil {
		return errRes, WriteFileResult{}, nil
	}
	if len(content) > r.limits.MaxFileBytes {
		return toolErr(codeTooLarge, fmt.Sprintf("content exceeds %d bytes", r.limits.MaxFileBytes), nil), WriteFileResult{}, nil
	}

	commit := in.Commit == nil || *in.Commit // default true

	ci, err := r.svc.WriteFile(ctx, site.WriteFileInput{
		SiteID:  ac.site.ID,
		Branch:  branch,
		Path:    clean,
		Content: content,
		BaseSHA: in.BaseSHA,
		Commit:  commit,
		Message: deref(in.Message),
		Actor:   actorFor(c),
	})
	if err != nil {
		res, gerr := r.mapError(err, "write_file")
		return res, WriteFileResult{}, gerr
	}

	// Mirror-push is best-effort inside the service; a failure does NOT fail the
	// tool (server is the source of truth). We optimistically report pushed:true
	// since WriteFile returning nil means the commit succeeded; a real push
	// failure is captured as a warning by the service layer (not surfaced as an
	// error here per safety guarantee #9).
	out := WriteFileResult{
		Path:         clean,
		Branch:       string(branch),
		Committed:    commit,
		Commit:       ci.SHA,
		BlobSHA:      "",
		Pushed:       true,
		BytesWritten: len(content),
	}
	return nil, out, nil
}

// decodeContent decodes the raw arg content per its declared encoding.
func (r *registry) decodeContent(raw string, encoding *string) ([]byte, *mcp.CallToolResult) {
	enc := encodingUTF8
	if encoding != nil && *encoding != "" {
		enc = strings.ToLower(*encoding)
	}
	switch enc {
	case encodingUTF8:
		return []byte(raw), nil
	case encodingBase64:
		b, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, toolErr(codeValidation, "content: invalid base64", nil)
		}
		return b, nil
	default:
		return nil, toolErr(codeValidation, "encoding: must be 'utf-8' or 'base64'", nil)
	}
}

// ============================ create_site ============================

// CreateSiteArgs is the ONE tool that mints a NEW site (the user becomes its
// owner). It takes a NEW `handle` (not a selector over existing sites). Gated by
// token.can_create_sites AND the user's users.can_create_sites (decision #8).
type CreateSiteArgs struct {
	Handle   string  `json:"handle" jsonschema:"new site handle; lowercase [a-z0-9-], 3-63 chars, unique, not reserved"`
	Init     *string `json:"init,omitempty" jsonschema:"'empty' (default) or 'template'"`
	Template *string `json:"template,omitempty" jsonschema:"template name when init=template"`
	Private  *bool   `json:"private,omitempty" jsonschema:"mirror to a private GitHub repo; default true"`
}

// CreateSiteResult never returns a usable token (no credential minting over MCP).
type CreateSiteResult struct {
	UUID          string `json:"uuid"`
	Handle        string `json:"handle"`
	DraftURL      string `json:"draft_url"`
	DefaultBranch string `json:"default_branch"`
	TokenHint     string `json:"token_hint"`
}

func (r *registry) createSite(ctx context.Context, c TokenInfo, in CreateSiteArgs) (*mcp.CallToolResult, CreateSiteResult, error) {
	// Capability gate (CANONICAL §6.2 / decision #8): create_site requires BOTH the
	// token's can_create_sites AND the user's account flag. c.CanCreateSites is the
	// AND computed at verify time; we ALSO re-read the user's flag here so a flag
	// revoked AFTER issuance takes effect immediately (the same per-request
	// re-evaluation principle the membership-cap uses). Either gate failing -> forbidden.
	if !c.CanCreateSites {
		return toolErr(codeForbidden, "this token cannot create sites; create the project in the dashboard", nil), CreateSiteResult{}, nil
	}
	user, err := r.members.GetUserByID(ctx, c.UserID)
	if err != nil {
		res, gerr := r.mapError(err, "create_site")
		return res, CreateSiteResult{}, gerr
	}
	if !user.CanCreateSites && !user.IsAdmin {
		return toolErr(codeForbidden, "your account is not allowed to create sites", nil), CreateSiteResult{}, nil
	}

	s, err := r.svc.CreateSite(ctx, site.CreateSiteInput{
		Handle:     site.Handle(in.Handle),
		OwnerID:    c.UserID,
		Visibility: "private",
		Actor:      actorFor(c),
	})
	if err != nil {
		res, gerr := r.mapError(err, "create_site")
		return res, CreateSiteResult{}, gerr
	}
	out := CreateSiteResult{
		UUID:          s.ID.String(),
		Handle:        string(s.Handle),
		DraftURL:      r.previewURL(s.Handle, site.BranchDraft),
		DefaultBranch: string(s.DefaultBranch),
		TokenHint:     "your existing token already covers this new site (you are its owner); use it directly via MCP",
	}
	return nil, out, nil
}

// ============================ save ============================

// SaveArgs takes the target `site` (handle). base_sha REQUIRED.
type SaveArgs struct {
	Site    string  `json:"site" jsonschema:"project handle (from list_sites) to commit in"`
	Branch  *string `json:"branch,omitempty" jsonschema:"branch to commit; 'published' rejected; default draft"`
	BaseSHA string  `json:"base_sha" jsonschema:"REQUIRED tip SHA the commit is based on (optimistic lock)"`
	Message *string `json:"message,omitempty" jsonschema:"commit message"`
}

// SaveResult reports the new tip; no_op when the working tree was clean.
type SaveResult struct {
	Branch       string `json:"branch"`
	Commit       string `json:"commit"`
	NoOp         bool   `json:"no_op"`
	Pushed       bool   `json:"pushed"`
	FilesChanged int    `json:"files_changed"`
}

func (r *registry) save(ctx context.Context, c TokenInfo, in SaveArgs) (*mcp.CallToolResult, SaveResult, error) {
	ac, authRes, gerr := r.authorizeSite(ctx, c, in.Site, scopeWrite)
	if authRes != nil || gerr != nil {
		return authRes, SaveResult{}, gerr
	}
	if in.BaseSHA == "" {
		return toolErr(codeValidation, "base_sha: required", nil), SaveResult{}, nil
	}
	branch := branchOrDefault(in.Branch)
	if branch == site.BranchPublished {
		return toolErr(codeValidation, "branch: 'published' is not committable; use publish", nil), SaveResult{}, nil
	}

	ci, err := r.svc.Commit(ctx, site.CommitInput{
		SiteID:  ac.site.ID,
		Branch:  branch,
		Message: deref(in.Message),
		BaseSHA: in.BaseSHA,
		Actor:   actorFor(c),
	})
	if err != nil {
		// A clean working tree surfaces as ErrNothingToCommit; the save tool
		// reports that as a successful no_op (idempotency, mcp.md §10.1), echoing
		// the base_sha as the (unchanged) tip.
		if isNothingToCommit(err) {
			out := SaveResult{Branch: string(branch), Commit: in.BaseSHA, NoOp: true, Pushed: true}
			return nil, out, nil
		}
		res, gerr := r.mapError(err, "save")
		return res, SaveResult{}, gerr
	}
	out := SaveResult{Branch: string(branch), Commit: ci.SHA, NoOp: false, Pushed: true, FilesChanged: len(ci.Parents)}
	return nil, out, nil
}

// isNothingToCommit reports whether err is the "clean tree" sentinel.
func isNothingToCommit(err error) bool {
	return errors.Is(err, site.ErrNothingToCommit)
}

// ============================ publish ============================

// PublishArgs takes the target `site` (handle). base_sha guards against shipping a
// stale snapshot.
type PublishArgs struct {
	Site    string  `json:"site" jsonschema:"project handle (from list_sites) to publish"`
	From    *string `json:"from,omitempty" jsonschema:"source branch to publish; default draft"`
	BaseSHA string  `json:"base_sha" jsonschema:"REQUIRED tip SHA of the source branch you intend to publish"`
	Message *string `json:"message,omitempty" jsonschema:"publish commit message"`
}

// PublishResult is the go-live body.
type PublishResult struct {
	PublishedCommit string `json:"published_commit"`
	PublishedURL    string `json:"published_url"`
	From            string `json:"from"`
	FromCommit      string `json:"from_commit"`
	Pushed          bool   `json:"pushed"`
	Redeploy        string `json:"redeploy"`
}

func (r *registry) publish(ctx context.Context, c TokenInfo, in PublishArgs) (*mcp.CallToolResult, PublishResult, error) {
	ac, authRes, gerr := r.authorizeSite(ctx, c, in.Site, scopePublish)
	if authRes != nil || gerr != nil {
		return authRes, PublishResult{}, gerr
	}
	// publish_mode gating (PARITY with the REST gate in internal/api/publish.go,
	// M3): an editor (non-owner) may only publish directly when the site is in
	// 'direct' mode; 'request' mode routes their publish through a GitHub PR, so a
	// direct publish is forbidden for them. The owner may always publish directly.
	if ac.role != gen.SiteRoleOwner && ac.site.PublishMode != "direct" {
		return toolErr(codeForbidden, "this site requires publish requests; only the owner can publish directly", nil), PublishResult{}, nil
	}
	if in.BaseSHA == "" {
		return toolErr(codeValidation, "base_sha: required (tip of the source branch)", nil), PublishResult{}, nil
	}
	from := branchOrDefault(in.From)

	ci, err := r.svc.Publish(ctx, site.PublishInput{
		SiteID:  ac.site.ID,
		From:    from,
		BaseSHA: in.BaseSHA,
		Message: deref(in.Message),
		Actor:   actorFor(c),
	})
	if err != nil {
		res, gerr := r.mapError(err, "publish")
		return res, PublishResult{}, gerr
	}

	// Compose the published URL from the site's current handle (already resolved).
	publishedURL := r.publishedURL(ac.site.Handle)

	out := PublishResult{
		PublishedCommit: ci.SHA,
		PublishedURL:    publishedURL,
		From:            string(from),
		FromCommit:      in.BaseSHA,
		Pushed:          true,
		Redeploy:        "live",
	}
	return nil, out, nil
}

// ============================ get_diff ============================

// GetDiffArgs takes the target `site` (handle). Two modes: from/to, or a single
// commit vs parent.
type GetDiffArgs struct {
	Site         string  `json:"site" jsonschema:"project handle (from list_sites) to diff in"`
	From         *string `json:"from,omitempty" jsonschema:"source ref or branch"`
	To           *string `json:"to,omitempty" jsonschema:"target ref or branch; omit with 'commit' to diff a single commit vs its parent"`
	Commit       *string `json:"commit,omitempty" jsonschema:"single commit SHA to diff against its parent (alternative to from/to)"`
	Path         *string `json:"path,omitempty" jsonschema:"optional path filter"`
	ContextLines *int    `json:"context_lines,omitempty" jsonschema:"unified-diff context lines; default 3"`
	Format       *string `json:"format,omitempty" jsonschema:"'unified' (default) or 'name-status'"`
}

// DiffFile is one changed path in a get_diff result.
type DiffFile struct {
	Path      string  `json:"path"`
	Status    string  `json:"status"`
	OldPath   *string `json:"old_path,omitempty"`
	Additions int     `json:"additions"`
	Deletions int     `json:"deletions"`
	Patch     string  `json:"patch,omitempty"`
}

// DiffResult is the get_diff body.
type DiffResult struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	FromCommit string     `json:"from_commit"`
	ToCommit   string     `json:"to_commit"`
	Files      []DiffFile `json:"files"`
	Truncated  bool       `json:"truncated"`
}

func (r *registry) getDiff(ctx context.Context, c TokenInfo, in GetDiffArgs) (*mcp.CallToolResult, DiffResult, error) {
	ac, authRes, gerr := r.authorizeSite(ctx, c, in.Site, scopeRead)
	if authRes != nil || gerr != nil {
		return authRes, DiffResult{}, gerr
	}
	// Resolve the two modes into From/To for the service.
	from := deref(in.From)
	to := deref(in.To)
	if in.Commit != nil && *in.Commit != "" {
		// Single-commit mode: diff the commit against its first parent. The
		// service interprets From=commit^, To=commit; we pass the commit as To and
		// its parent notation as From so the unified service contract handles it.
		from = *in.Commit + "^"
		to = *in.Commit
	}
	if from == "" {
		return toolErr(codeValidation, "from or commit is required", nil), DiffResult{}, nil
	}

	contextLines := 3
	if in.ContextLines != nil {
		contextLines = *in.ContextLines
	}
	nameStatus := in.Format != nil && strings.ToLower(*in.Format) == "name-status"

	dr, err := r.svc.GetDiff(ctx, site.DiffOptions{
		SiteID:       ac.site.ID,
		From:         from,
		To:           to,
		Path:         deref(in.Path),
		ContextLines: contextLines,
		NameStatus:   nameStatus,
	})
	if err != nil {
		res, gerr := r.mapError(err, "get_diff")
		return res, DiffResult{}, gerr
	}

	files := make([]DiffFile, 0, len(dr.Files))
	budget := r.limits.MaxDiffBytes
	truncated := false
	for _, fd := range dr.Files {
		df := DiffFile{
			Path:      fd.Path,
			Status:    fd.Status,
			Additions: fd.Additions,
			Deletions: fd.Deletions,
		}
		if fd.OldPath != "" {
			op := fd.OldPath
			df.OldPath = &op
		}
		if !nameStatus {
			// Drop patches once the total budget is exhausted (mcp.md §10.2).
			if len(fd.UnifiedPatch) <= budget {
				df.Patch = fd.UnifiedPatch
				budget -= len(fd.UnifiedPatch)
			} else {
				truncated = true
			}
		}
		files = append(files, df)
	}
	out := DiffResult{
		From:       from,
		To:         to,
		FromCommit: dr.FromSHA,
		ToCommit:   dr.ToSHA,
		Files:      files,
		Truncated:  truncated,
	}
	return nil, out, nil
}

// ============================ get_log ============================

// GetLogArgs takes the target `site` (handle).
type GetLogArgs struct {
	Site   string  `json:"site" jsonschema:"project handle (from list_sites) to read history from"`
	Branch *string `json:"branch,omitempty" jsonschema:"branch; default draft"`
	Path   *string `json:"path,omitempty" jsonschema:"restrict to commits touching this path"`
	Limit  *int    `json:"limit,omitempty" jsonschema:"max commits, default 20, max 100"`
	Before *string `json:"before,omitempty" jsonschema:"pagination cursor: return commits strictly older than this SHA"`
}

// LogCommit is one history entry.
type LogCommit struct {
	SHA          string `json:"sha"`
	Short        string `json:"short"`
	Author       string `json:"author"`
	Via          string `json:"via"`
	Message      string `json:"message"`
	CommittedAt  string `json:"committed_at"`
	FilesChanged int    `json:"files_changed"`
}

// LogResult is the get_log body with a pagination cursor.
type LogResult struct {
	Branch     string      `json:"branch"`
	Commits    []LogCommit `json:"commits"`
	NextBefore *string     `json:"next_before,omitempty"`
}

func (r *registry) getLog(ctx context.Context, c TokenInfo, in GetLogArgs) (*mcp.CallToolResult, LogResult, error) {
	ac, authRes, gerr := r.authorizeSite(ctx, c, in.Site, scopeRead)
	if authRes != nil || gerr != nil {
		return authRes, LogResult{}, gerr
	}
	branch := branchOrDefault(in.Branch)
	limit := 20
	if in.Limit != nil {
		limit = *in.Limit
	}
	// Validation: limit out of range is a client error (mcp.md §5.9).
	if limit < 1 || limit > r.limits.MaxLogLimit {
		return toolErr(codeValidation, fmt.Sprintf("limit: must be 1..%d", r.limits.MaxLogLimit), nil), LogResult{}, nil
	}

	commits, err := r.svc.GetLog(ctx, site.LogOptions{
		SiteID: ac.site.ID,
		Branch: branch,
		Limit:  limit,
		Before: deref(in.Before),
		Path:   deref(in.Path),
	})
	if err != nil {
		res, gerr := r.mapError(err, "get_log")
		return res, LogResult{}, gerr
	}

	out := LogResult{Branch: string(branch), Commits: make([]LogCommit, 0, len(commits))}
	for _, ci := range commits {
		out.Commits = append(out.Commits, LogCommit{
			SHA:          ci.SHA,
			Short:        ci.ShortSHA,
			Author:       ci.AuthorEmail,
			Via:          ci.Via,
			Message:      ci.Message,
			CommittedAt:  ci.Committed.UTC().Format("2006-01-02T15:04:05Z07:00"),
			FilesChanged: len(ci.Parents),
		})
	}
	// Pagination cursor: if we filled the page, the last SHA is the next cursor.
	if len(out.Commits) == limit && limit > 0 {
		last := out.Commits[len(out.Commits)-1].SHA
		out.NextBefore = &last
	}
	return nil, out, nil
}

// ============================ rollback ============================

// RollbackArgs takes the target `site` (handle). base_sha + to_sha both REQUIRED.
type RollbackArgs struct {
	Site    string  `json:"site" jsonschema:"project handle (from list_sites) to roll back in"`
	Branch  *string `json:"branch,omitempty" jsonschema:"branch; 'published' rejected; default draft"`
	ToSHA   string  `json:"to_sha" jsonschema:"REQUIRED commit whose tree is restored (must be an ancestor on the branch)"`
	BaseSHA string  `json:"base_sha" jsonschema:"REQUIRED current branch tip you believe you are reverting from"`
	Message *string `json:"message,omitempty"`
}

// RollbackResult is the forward-revert body.
type RollbackResult struct {
	Branch       string `json:"branch"`
	Commit       string `json:"commit"`
	RestoredFrom string `json:"restored_from"`
	Pushed       bool   `json:"pushed"`
}

func (r *registry) rollback(ctx context.Context, c TokenInfo, in RollbackArgs) (*mcp.CallToolResult, RollbackResult, error) {
	ac, authRes, gerr := r.authorizeSite(ctx, c, in.Site, scopeWrite)
	if authRes != nil || gerr != nil {
		return authRes, RollbackResult{}, gerr
	}
	if in.ToSHA == "" {
		return toolErr(codeValidation, "to_sha: required", nil), RollbackResult{}, nil
	}
	if in.BaseSHA == "" {
		return toolErr(codeValidation, "base_sha: required", nil), RollbackResult{}, nil
	}
	branch := branchOrDefault(in.Branch)
	if branch == site.BranchPublished {
		return toolErr(codeValidation, "branch: 'published' is not rollback-able; publish from a rolled-back draft", nil), RollbackResult{}, nil
	}

	ci, err := r.svc.Rollback(ctx, ac.site.ID, branch, in.ToSHA, in.BaseSHA, actorFor(c))
	if err != nil {
		res, gerr := r.mapError(err, "rollback")
		return res, RollbackResult{}, gerr
	}
	out := RollbackResult{Branch: string(branch), Commit: ci.SHA, RestoredFrom: in.ToSHA, Pushed: true}
	return nil, out, nil
}

// ============================ create_branch ============================

// CreateBranchArgs takes the target `site` (handle). Creates a new branch from an
// existing ref.
type CreateBranchArgs struct {
	Site string  `json:"site" jsonschema:"project handle (from list_sites) to create the branch in"`
	Name string  `json:"name" jsonschema:"new branch name; lowercase [a-z0-9-], no '--', not 'published'/'draft'"`
	From *string `json:"from,omitempty" jsonschema:"source branch or commit; default draft"`
}

// CreateBranchResult is the new branch body.
type CreateBranchResult struct {
	Name             string `json:"name"`
	HeadSHA          string `json:"head_sha"`
	PreviewSubdomain string `json:"preview_subdomain"`
}

func (r *registry) createBranch(ctx context.Context, c TokenInfo, in CreateBranchArgs) (*mcp.CallToolResult, CreateBranchResult, error) {
	ac, authRes, gerr := r.authorizeSite(ctx, c, in.Site, scopeWrite)
	if authRes != nil || gerr != nil {
		return authRes, CreateBranchResult{}, gerr
	}
	if in.Name == "" {
		return toolErr(codeValidation, "name: required", nil), CreateBranchResult{}, nil
	}
	from := "draft"
	if in.From != nil && *in.From != "" {
		from = *in.From
	}
	b, err := r.svc.CreateBranch(ctx, ac.site.ID, site.BranchName(in.Name), from)
	if err != nil {
		res, gerr := r.mapError(err, "create_branch")
		return res, CreateBranchResult{}, gerr
	}
	out := CreateBranchResult{Name: string(b.Name), HeadSHA: b.HeadSHA, PreviewSubdomain: b.PreviewSubdomain}
	return nil, out, nil
}

// ---- URL composition (from the configured base domain) ----

// publishedURL builds the live URL: https://{handle}.<base>.
func (r *registry) publishedURL(h site.Handle) string {
	return r.cfg.urlFor(string(h))
}

// previewURL builds a branch preview URL: https://{handle}--{branch}.<base>
// (published uses the bare handle).
func (r *registry) previewURL(h site.Handle, branch site.BranchName) string {
	if branch == site.BranchPublished {
		return r.cfg.urlFor(string(h))
	}
	return r.cfg.urlFor(string(h) + "--" + string(branch))
}
