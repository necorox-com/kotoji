// Package site is the single git boundary of kotoji. Every write — zip upload,
// the Monaco editor, MCP tools — and the data-plane read side funnel through the
// site.Service interface defined here. It is THE dependency-injection seam:
// handlers depend on this interface, never on git or the filesystem directly, so
// the whole control plane is unit-testable against a fake (fake.go).
//
// The interface signatures are FROZEN by docs/contracts/CANONICAL.md §1 and must
// not be renamed. Two implementations live in this package: gitService (prod,
// git CLI via os/exec + a Postgres-backed metadata Store) and FakeService
// (in-memory, same contract) for downstream tests.
package site

import (
	"context"
	"io"
	"time"

	"github.com/google/uuid"
)

// SiteID is an OPTIONAL readability alias; interface signatures use uuid.UUID
// directly per CANONICAL §1.
type SiteID = uuid.UUID

// Handle is the unique, DNS-safe, renameable english name (see handle.go / §5).
type Handle string

// BranchName is a git branch name. Reserved logical names: "published", "draft".
type BranchName string

const (
	// BranchPublished is the served-in-prod branch.
	BranchPublished BranchName = "published"
	// BranchDraft is the default working branch.
	BranchDraft BranchName = "draft"
)

// WriteSource is the canonical provenance value set (matches audit_source enum,
// CANONICAL §4 / §2).
type WriteSource string

const (
	// SourceUpload is the zip upload path.
	SourceUpload WriteSource = "upload"
	// SourceEditor is the Monaco / dashboard path.
	SourceEditor WriteSource = "editor"
	// SourceMCP is an MCP tool call.
	SourceMCP WriteSource = "mcp"
	// SourceSystem is webhook pulls, scheduled jobs, admin actions, migrations.
	SourceSystem WriteSource = "system"
)

// ---- Domain (read) structs (CANONICAL §2) ----

// Site is the metadata + git head summary of one project.
type Site struct {
	ID            uuid.UUID  `json:"id"`
	Handle        Handle     `json:"handle"`
	OwnerID       uuid.UUID  `json:"ownerId"`
	Visibility    string     `json:"visibility"`           // "public" | "internal" | "private"
	DefaultBranch BranchName `json:"defaultBranch"`        // usually "draft"
	GitHubRepo    string     `json:"githubRepo,omitempty"` // "owner/name" if mirrored, else ""
	PublishMode   string     `json:"publishMode"`          // "direct" | "request"
	WebRoot       string     `json:"webRoot"`              // served subdir; "" = repo root (v1)
	HasPublished  bool       `json:"hasPublished"`         // derived: published_commit_sha IS NOT NULL
	PublishedSHA  string     `json:"publishedSha,omitempty"`
	PublishedAt   *time.Time `json:"publishedAt,omitempty"`
	Description   string     `json:"description"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// Branch is one git branch with its head and a precomputed preview host label
// fragment.
type Branch struct {
	Name        BranchName `json:"name"`
	HeadSHA     string     `json:"headSha"`     // full 40-char SHA of the branch tip
	IsPublished bool       `json:"isPublished"` // Name == BranchPublished
	LastCommit  CommitInfo `json:"lastCommit"`
	// PreviewSubdomain is the host label fragment: "{handle}" for published,
	// "{handle}--{branch}" otherwise. The API/MCP layer composes full URLs from it.
	PreviewSubdomain string `json:"previewSubdomain"`
}

// FileEntry is one node in a directory listing.
type FileEntry struct {
	Path  string `json:"path"`  // repo-relative, forward slashes, no leading "/"
	Name  string `json:"name"`  // base name
	IsDir bool   `json:"isDir"` // directory node
	Size  int64  `json:"size"`  // bytes; 0 for dirs
	Mode  string `json:"mode"`  // git mode, e.g. "100644", "100755", "040000"
}

// FileContent is a single file read at a specific commit.
type FileContent struct {
	Path     string `json:"path"`
	Content  []byte `json:"-"`        // raw bytes; serialized separately (base64 / stream)
	SHA      string `json:"sha"`      // the COMMIT SHA the content was read at (the lock token)
	BlobSHA  string `json:"blobSha"`  // blob hash of this specific file
	Size     int64  `json:"size"`     // byte length
	IsBinary bool   `json:"isBinary"` // detected via NUL-byte heuristic + extension
}

// CommitInfo is one entry in the history.
type CommitInfo struct {
	SHA         string    `json:"sha"`      // full 40-char SHA
	ShortSHA    string    `json:"shortSha"` // 7-char
	Message     string    `json:"message"`
	AuthorName  string    `json:"authorName"`
	AuthorEmail string    `json:"authorEmail"`
	Committed   time.Time `json:"committed"`
	Parents     []string  `json:"parents"` // parent SHAs (1 normal, 2 merge)
	Via         string    `json:"via"`     // provenance: "upload"|"editor"|"mcp"|"system"
}

// ResolvedRef is the concrete SHA a branch/ref name resolved to for a read.
type ResolvedRef struct {
	SHA string `json:"sha"`
}

// DiffResult is a diff between two refs (or a ref and the working set).
type DiffResult struct {
	FromSHA string     `json:"fromSha"`
	ToSHA   string     `json:"toSha"`
	Files   []FileDiff `json:"files"`
}

// FileDiff is one changed path in a DiffResult.
type FileDiff struct {
	Path         string `json:"path"`
	OldPath      string `json:"oldPath,omitempty"` // set on rename
	Status       string `json:"status"`            // "added"|"modified"|"deleted"|"renamed"
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	IsBinary     bool   `json:"isBinary"`
	UnifiedPatch string `json:"unifiedPatch"` // unified-diff text; empty if binary/name-status
}

// ServeTarget tells the data plane what to read from disk, read-only. The
// resolver produces it from the Host header.
type ServeTarget struct {
	SiteID  uuid.UUID  `json:"siteId"`
	Handle  Handle     `json:"handle"`
	Branch  BranchName `json:"branch"`
	Root    string     `json:"-"`       // absolute path to the read-only served worktree root
	HeadSHA string     `json:"headSha"` // for cache-keying / ETag
}

// TreeHandle is the immutable served tree returned by ServedTree (backs
// resolve.TreeProvider).
type TreeHandle struct {
	Root       string    `json:"-"`          // absolute served-worktree root (EXCLUDES .git)
	CommitSHA  string    `json:"commitSha"`  // 40-hex; "" only in dev/empty-site
	CommitTime time.Time `json:"commitTime"` // -> Last-Modified
	Exists     bool      `json:"exists"`
}

// ---- Input value objects (write side) (CANONICAL §2) ----

// CreateSiteInput is the request to create a new site (empty, or seeded from a
// zip).
type CreateSiteInput struct {
	Handle      Handle     // validated + reserved-word checked inside CreateSite
	OwnerID     uuid.UUID  // auth subject; stamped as initial owner + owner site_members row
	Visibility  string     // "public"|"internal"|"private"; default "private"
	PublishMode string     // "direct"|"request"; default "direct"
	Description string     // optional
	Zip         *ZipSource // optional initial content; nil => empty draft w/ placeholder index.html
	GitHubRepo  string     // optional mirror remote "owner/name"
	Actor       Actor      // who is creating (git author + audit)
}

// ZipSource carries the uploaded archive plus its declared length for guard
// checks.
type ZipSource struct {
	Reader   io.ReaderAt // archive/zip needs ReaderAt + Size
	Size     int64       // declared content length (Content-Length); guarded
	Filename string      // original name, for the commit message only
}

// WriteFileInput is a single optimistic-locked content write.
type WriteFileInput struct {
	SiteID  uuid.UUID
	Branch  BranchName
	Path    string // repo-relative; ZipSlip/abs/.. rejected by validatePath
	Content []byte
	// BaseSHA is the commit SHA the caller READ from. REQUIRED. Empty => ErrValidation.
	// Mismatch with the current branch tip => ErrConflict.
	BaseSHA string
	Commit  bool   // true (default at the edge) => create a commit after staging
	Message string // optional commit message
	Actor   Actor  // who is committing -> git author + audit
}

// CommitInput commits the already-staged working set (the multi-file "Save"
// verb).
type CommitInput struct {
	SiteID  uuid.UUID
	Branch  BranchName
	Message string
	BaseSHA string // same optimistic-lock semantics as WriteFileInput; REQUIRED
	Actor   Actor
}

// ListFilesInput merges the MCP `ref` need and the editor `recursive` need
// (consistency-report resolution #14).
type ListFilesInput struct {
	SiteID    uuid.UUID
	Branch    BranchName
	Dir       string // repo-relative subtree filter; "" = root
	Ref       string // optional commit SHA to list at; "" = branch tip
	Recursive bool   // false = one level; true = whole subtree
}

// PublishInput promotes From -> published.
type PublishInput struct {
	SiteID  uuid.UUID
	From    BranchName // default "draft"
	BaseSHA string     // REQUIRED: tip of From the publish is intended for
	Message string     // optional
	Actor   Actor
}

// DiffOptions for GetDiff.
type DiffOptions struct {
	SiteID       uuid.UUID
	From         string // ref or branch
	To           string // ref or branch; "" => working tree of From's branch
	Path         string // optional path filter
	ContextLines int    // default 3
	NameStatus   bool   // true => omit patch text (name-status mode)
}

// LogOptions paginates history.
type LogOptions struct {
	SiteID uuid.UUID
	Branch BranchName
	Limit  int    // default 50, max 500 (clamped); MCP layer caps its own at 100
	Before string // cursor: return commits strictly older than this SHA (exclusive)
	Path   string // optional: restrict to commits touching this path
}

// Actor is the identity attributed to a git commit and the audit log
// (CANONICAL §2; consistency-report resolution #1.1).
type Actor struct {
	UserID  uuid.UUID   // auth subject; primary key for audit_log.actor_id
	Name    string      // display name -> git author.name
	Email   string      // -> git author.email; synthesized if absent (userID@kotoji.local)
	Via     WriteSource // "upload"|"editor"|"mcp"|"system" -> Kotoji-Via trailer + audit_log.source
	TokenID *uuid.UUID  // set when acting via a site_token (MCP/API) -> audit_log.token_id
}

// Service is the ONLY component that touches git on disk. All writers (zip
// upload, Monaco editor, MCP tools) and the data-plane read side funnel through
// this interface. The signatures are FROZEN by CANONICAL §1.
//
// Concurrency: safe for concurrent use across DIFFERENT sites; mutations to the
// SAME site are serialized internally by a per-site lock (lock.go). context is
// always first; error is always last.
type Service interface {
	// ---- Site lifecycle (touches git repo init AND metadata) ----

	// CreateSite validates+reserves the handle, creates /data/sites/{uuid}/.git with an
	// initial commit on "draft" (placeholder index.html, or seeded from in.Zip), records
	// metadata, inserts the owner site_members row, and (if GitHubRepo set) configures the
	// mirror remote. Atomic: on any failure the repo dir and metadata rows are rolled back.
	// Errors: ErrValidation, ErrReservedHandle, ErrHandleTaken, ErrZip*, ErrGit.
	CreateSite(ctx context.Context, in CreateSiteInput) (Site, error)

	// GetSite returns metadata + head summary. Errors: ErrNotFound.
	GetSite(ctx context.Context, id uuid.UUID) (Site, error)

	// GetSiteByHandle resolves a *current* handle to its site. Old (renamed) handles 404
	// here; redirect handling is the resolver/HTTP layer's job (handle_redirects table).
	// Errors: ErrNotFound.
	GetSiteByHandle(ctx context.Context, h Handle) (Site, error)

	// ListSites lists sites visible to ownerID (membership-filtered by the Store).
	// Authz enforcement is the caller's; an empty ownerID with the admin flag lists all.
	// Errors: ErrGit (store failure).
	ListSites(ctx context.Context, ownerID uuid.UUID) ([]Site, error)

	// RenameHandle changes the handle, recording old->new in handle_redirects. Files DO
	// NOT move (path is UUID-keyed). GitHub repo rename (if mirrored) is best-effort.
	// Errors: ErrNotFound, ErrValidation, ErrReservedHandle, ErrHandleTaken.
	RenameHandle(ctx context.Context, id uuid.UUID, newHandle Handle) (Site, error)

	// DeleteSite SOFT-deletes: sets sites.deleted_at; the handle stays reserved during the
	// 30-day grace; the repo is retained. Errors: ErrNotFound, ErrGit.
	DeleteSite(ctx context.Context, id uuid.UUID, actor Actor) error

	// ---- Branches ----

	ListBranches(ctx context.Context, id uuid.UUID) ([]Branch, error)

	// CreateBranch branches `from` (a branch name or SHA) into a new branch `name`.
	// Errors: ErrNotFound, ErrValidation, ErrBranchExists, ErrGit.
	CreateBranch(ctx context.Context, id uuid.UUID, name BranchName, from string) (Branch, error)

	// DeleteBranch refuses to delete "published" or "draft" (ErrValidation).
	// Errors: ErrNotFound, ErrValidation, ErrGit.
	DeleteBranch(ctx context.Context, id uuid.UUID, name BranchName) error

	// ---- Files (read) ----

	// ListFiles lists entries under in.Dir at in.Ref (a SHA) or, if Ref is empty, at the
	// tip of in.Branch. Non-recursive unless in.Recursive. Returns the entries plus the
	// ResolvedRef the listing reflects.
	// Errors: ErrNotFound (site/branch/dir/ref), ErrValidation (bad path).
	ListFiles(ctx context.Context, in ListFilesInput) ([]FileEntry, ResolvedRef, error)

	// ReadFile returns the file at in.Ref or the tip of branch. FileContent.SHA is the
	// COMMIT SHA the caller MUST echo back as BaseSHA on a subsequent write.
	// Errors: ErrNotFound, ErrValidation (bad path).
	ReadFile(ctx context.Context, id uuid.UUID, branch BranchName, ref, path string) (FileContent, error)

	// ---- Files (write) — all optimistic-locked on BaseSHA ----

	// WriteFile creates/overwrites one file and (if in.Commit) commits it on in.Branch.
	// OPTIMISTIC LOCK: rejects with ErrConflict unless in.BaseSHA == current tip of branch.
	// Empty BaseSHA => ErrValidation. "published" branch => ErrValidation. Returns the NEW
	// tip CommitInfo. Mirror-push is best-effort.
	// Errors: ErrNotFound, ErrValidation, ErrConflict, ErrGit.
	WriteFile(ctx context.Context, in WriteFileInput) (CommitInfo, error)

	// DeleteFile removes one file and commits. Same lock + path + branch rules as WriteFile.
	// Errors: ErrNotFound, ErrValidation, ErrConflict, ErrGit.
	DeleteFile(ctx context.Context, id uuid.UUID, branch BranchName, path, baseSHA string, actor Actor) (CommitInfo, error)

	// ImportZip extracts an uploaded archive into branch as ONE commit, REPLACING the tree.
	// Applies ZipSlip + zip-bomb + extension guards BEFORE writing anything. Optimistic-
	// locked on baseSHA (empty BaseSHA allowed ONLY when the branch has no commits yet).
	// "published" branch => ErrValidation.
	// Errors: ErrValidation, ErrZipSlip, ErrZipTooLarge, ErrZipTooManyFiles, ErrZipBadType,
	//         ErrConflict, ErrGit.
	ImportZip(ctx context.Context, id uuid.UUID, branch BranchName, src ZipSource, baseSHA string, actor Actor) (CommitInfo, error)

	// Commit ("Save") turns the currently-staged working set of in.Branch into one commit.
	// Optimistic-locked on in.BaseSHA. "published" => ErrValidation.
	// Errors: ErrNotFound, ErrConflict, ErrNothingToCommit, ErrValidation, ErrGit.
	Commit(ctx context.Context, in CommitInput) (CommitInfo, error)

	// ---- Publish (draft -> published, git level) ----

	// Publish promotes in.From (default "draft") to "published" (fast-forward, else a merge
	// commit). Idempotent if published already equals the source tip. in.BaseSHA guards
	// against publishing a stale snapshot. After success, refreshes the served worktree,
	// mirror-pushes "published", and updates sites.published_commit_sha / published_at.
	// Errors: ErrNotFound, ErrConflict (stale base), ErrPublishConflict, ErrValidation, ErrGit.
	Publish(ctx context.Context, in PublishInput) (CommitInfo, error)

	// ---- History / diff / rollback ----

	// GetDiff diffs two refs. Either may be a branch name or a SHA. If in.To is empty it
	// means the working tree of in.From's branch. Errors: ErrNotFound (ref), ErrValidation.
	GetDiff(ctx context.Context, in DiffOptions) (DiffResult, error)

	// GetLog returns history for in.Branch, newest first, paginated by in.
	// Errors: ErrNotFound (branch), ErrValidation (limit out of range).
	GetLog(ctx context.Context, in LogOptions) ([]CommitInfo, error)

	// Rollback creates a NEW commit on branch whose tree equals the tree at toSHA (a
	// revert-to-tree, NOT a history-rewriting reset). toSHA MUST be an ancestor reachable on
	// branch (else ErrNotFound). baseSHA must equal the current tip. "published" =>
	// ErrValidation. Errors: ErrNotFound, ErrConflict, ErrValidation, ErrGit.
	Rollback(ctx context.Context, id uuid.UUID, branch BranchName, toSHA, baseSHA string, actor Actor) (CommitInfo, error)

	// ---- GitHub mirror / remote (explicit, independently testable) ----

	// SetRemote configures (or clears, if url=="") the "origin" mirror remote.
	// Errors: ErrNotFound, ErrValidation, ErrGit.
	SetRemote(ctx context.Context, id uuid.UUID, url string) error

	// MirrorPush best-effort pushes the given branches to origin. Push failure NEVER fails
	// the originating save/publish. Returns a warning-bearing error only for explicit calls.
	MirrorPush(ctx context.Context, id uuid.UUID, branches ...BranchName) error

	// FetchAndUpdate is the GitHub-webhook entry point: fetch origin, fast-forward the local
	// branch + refresh its served worktree. Non-FF is rejected and flagged.
	// Errors: ErrNotFound, ErrPublishConflict, ErrGit.
	FetchAndUpdate(ctx context.Context, id uuid.UUID, branch BranchName) (CommitInfo, error)

	// ---- Data-plane read side (backs resolve.TreeProvider) ----

	// ServedTree returns an immutable, ready-to-serve file tree for (siteID, branch) without
	// invoking git on the request path (materialized worktree under /data/sites/{uuid}/served).
	// Errors: ErrNotFound (site/branch).
	ServedTree(ctx context.Context, id uuid.UUID, branch BranchName) (TreeHandle, error)
}
