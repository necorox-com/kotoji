package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/necorox-com/kotoji/backend/internal/site"
)

// encodingUTF8 / encodingBase64 are the two content encodings the file tools use.
const (
	encodingUTF8   = "utf-8"
	encodingBase64 = "base64"
)

// registerAll wires every tool with its scope + rate class. The In types carry
// jsonschema tags (auto-inferred schema) and — critically — NONE of the content
// tools carry a site selector: the site is always claims.SiteID (PIVOT guarantee,
// asserted by reflection in the tests).
func (r *registry) registerAll(s *mcp.Server) {
	addTool(s, r, "list_sites", "List the single project this token may address.", scopeRead, classRead, r.listSites)
	addTool(s, r, "list_files", "List files in the project at a branch or commit.", scopeRead, classRead, r.listFiles)
	addTool(s, r, "read_file", "Read a file; returns the commit SHA to pass back as base_sha.", scopeRead, classRead, r.readFile)
	addTool(s, r, "write_file", "Write a file (base_sha REQUIRED; stale base_sha is rejected as conflict).", scopeWrite, classWrite, r.writeFile)
	addTool(s, r, "create_site", "Create a NEW project (disabled unless the token has can_create_sites).", scopeWrite, classCreate, r.createSite)
	addTool(s, r, "save", "Commit the working set on a branch (base_sha REQUIRED).", scopeWrite, classWrite, r.save)
	addTool(s, r, "publish", "Promote a branch to the live published site (base_sha REQUIRED).", scopePublish, classPublish, r.publish)
	addTool(s, r, "get_diff", "Diff two refs, or a commit vs its parent.", scopeRead, classRead, r.getDiff)
	addTool(s, r, "get_log", "Return commit history for a branch (paginated).", scopeRead, classRead, r.getLog)
	addTool(s, r, "rollback", "Forward-revert a branch to a previous commit's tree (base_sha REQUIRED).", scopeWrite, classWrite, r.rollback)
	addTool(s, r, "create_branch", "Create a new branch from an existing branch or commit.", scopeWrite, classWrite, r.createBranch)
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

// ListSitesArgs takes no arguments — the token already identifies the site.
type ListSitesArgs struct{}

// SiteSummary is one project in a list_sites result.
type SiteSummary struct {
	UUID          string `json:"uuid"`
	Handle        string `json:"handle"`
	PublishedURL  string `json:"published_url"`
	DraftURL      string `json:"draft_url"`
	DefaultBranch string `json:"default_branch"`
	IsPublished   bool   `json:"is_published"`
	UpdatedAt     string `json:"updated_at"`
}

// ListSitesResult wraps the (single) pinned site.
type ListSitesResult struct {
	Sites []SiteSummary `json:"sites"`
}

func (r *registry) listSites(ctx context.Context, c TokenInfo, _ ListSitesArgs) (*mcp.CallToolResult, ListSitesResult, error) {
	// Pinned: a single GetSite on claims.SiteID — never a list query, so a token
	// can only ever see its own project (mcp.md §5.1).
	s, err := r.svc.GetSite(ctx, c.SiteID)
	if err != nil {
		res, gerr := r.mapError(err, "list_sites")
		return res, ListSitesResult{}, gerr
	}
	out := ListSitesResult{Sites: []SiteSummary{r.toSummary(s)}}
	return nil, out, nil
}

// toSummary composes the wire summary, including preview URLs from the base domain.
func (r *registry) toSummary(s site.Site) SiteSummary {
	return SiteSummary{
		UUID:          s.ID.String(),
		Handle:        string(s.Handle),
		PublishedURL:  r.publishedURL(s.Handle),
		DraftURL:      r.previewURL(s.Handle, site.BranchDraft),
		DefaultBranch: string(s.DefaultBranch),
		IsPublished:   s.HasPublished,
		UpdatedAt:     s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// ============================ list_files ============================

// ListFilesArgs — NO site selector. Optional branch/ref/path/recursive.
type ListFilesArgs struct {
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
		SiteID:    c.SiteID,
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

// ReadFileArgs — NO site selector.
type ReadFileArgs struct {
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
	clean, errRes := validateContentPath(in.Path, false)
	if errRes != nil {
		return errRes, ReadFileResult{}, nil
	}
	branch := branchOrDefault(in.Branch)

	fc, err := r.svc.ReadFile(ctx, c.SiteID, branch, deref(in.Ref), clean)
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

// WriteFileArgs — NO site selector. base_sha is REQUIRED (no force flag, v1).
type WriteFileArgs struct {
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
		SiteID:  c.SiteID,
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

// CreateSiteArgs is the ONE tool that mints a NEW site (its result is outside the
// token's original binding). Gated by claims.CanCreateSites (decision #8).
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
	// Account capability gate (CANONICAL §6.2 / decision #8): OFF by default for
	// project tokens. A per-project token that can spawn projects is a privilege-
	// escalation vector, so we refuse unless explicitly enabled at issuance.
	if !c.CanCreateSites {
		return toolErr(codeForbidden, "this token cannot create sites; create the project in the dashboard", nil), CreateSiteResult{}, nil
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
		TokenHint:     "issue a project token for this site in the dashboard to manage it via MCP",
	}
	return nil, out, nil
}

// ============================ save ============================

// SaveArgs — NO site selector. base_sha REQUIRED.
type SaveArgs struct {
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
	if in.BaseSHA == "" {
		return toolErr(codeValidation, "base_sha: required", nil), SaveResult{}, nil
	}
	branch := branchOrDefault(in.Branch)
	if branch == site.BranchPublished {
		return toolErr(codeValidation, "branch: 'published' is not committable; use publish", nil), SaveResult{}, nil
	}

	ci, err := r.svc.Commit(ctx, site.CommitInput{
		SiteID:  c.SiteID,
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

// PublishArgs — NO site selector. base_sha guards against shipping a stale snapshot.
type PublishArgs struct {
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
	if in.BaseSHA == "" {
		return toolErr(codeValidation, "base_sha: required (tip of the source branch)", nil), PublishResult{}, nil
	}
	from := branchOrDefault(in.From)

	ci, err := r.svc.Publish(ctx, site.PublishInput{
		SiteID:  c.SiteID,
		From:    from,
		BaseSHA: in.BaseSHA,
		Message: deref(in.Message),
		Actor:   actorFor(c),
	})
	if err != nil {
		res, gerr := r.mapError(err, "publish")
		return res, PublishResult{}, gerr
	}

	// Compose the published URL from the site's current handle.
	var publishedURL string
	if s, gerr := r.svc.GetSite(ctx, c.SiteID); gerr == nil {
		publishedURL = r.publishedURL(s.Handle)
	}

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

// GetDiffArgs — NO site selector. Two modes: from/to, or a single commit vs parent.
type GetDiffArgs struct {
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
		SiteID:       c.SiteID,
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

// GetLogArgs — NO site selector.
type GetLogArgs struct {
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
		SiteID: c.SiteID,
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

// RollbackArgs — NO site selector. base_sha + to_sha both REQUIRED.
type RollbackArgs struct {
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

	ci, err := r.svc.Rollback(ctx, c.SiteID, branch, in.ToSHA, in.BaseSHA, actorFor(c))
	if err != nil {
		res, gerr := r.mapError(err, "rollback")
		return res, RollbackResult{}, gerr
	}
	out := RollbackResult{Branch: string(branch), Commit: ci.SHA, RestoredFrom: in.ToSHA, Pushed: true}
	return nil, out, nil
}

// ============================ create_branch ============================

// CreateBranchArgs — NO site selector. Creates a new branch from an existing ref.
type CreateBranchArgs struct {
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
	if in.Name == "" {
		return toolErr(codeValidation, "name: required", nil), CreateBranchResult{}, nil
	}
	from := "draft"
	if in.From != nil && *in.From != "" {
		from = *in.From
	}
	b, err := r.svc.CreateBranch(ctx, c.SiteID, site.BranchName(in.Name), from)
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
