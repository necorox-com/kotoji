# kotoji — CANONICAL Contract (the law)

> **This file overrides all other docs on conflict. Other docs are detail/rationale.**
>
> Status: **FROZEN.** All downstream code generation (Go `oapi-codegen`, TS
> `openapi-typescript`/`openapi-fetch`, sqlc/goose) follows this file and its sibling
> [`openapi.yaml`](./openapi.yaml). Where `architecture.md`, `design.md`,
> `site-service.md`, `data-model.md`, `mcp.md`, `routing-and-serving.md`, or
> `consistency-report.md` disagree with this document, **this document wins** and the
> other doc is to be read only for explanation/justification.
>
> This file resolves **every** inconsistency catalogued in
> [`consistency-report.md`](./consistency-report.md) (whose RESOLUTIONS are authoritative
> and are applied verbatim below) and applies the 8 LOCKED PRODUCT DECISIONS (see §9).

---

## 0. Contents

1. The frozen `site.Service` Go interface (package `internal/site`)
2. Domain structs and input value objects
3. The frozen error taxonomy + `statusFor()` HTTP mapping
4. The frozen complete PostgreSQL DDL (goose `0001_init`)
5. Identifier / handle / branch rules (regex, reserved words, redirects, `--` separator)
6. The role → capability matrix + token-scope × role capping rule
7. The `ON DELETE` matrix, indexes, FKs (cross-reference of §4)
8. Wire ↔ Go field-name mapping (the drift-free contract)
9. Locked decisions (the 8)

---

## 1. The FROZEN `site.Service` interface

- **Package:** `internal/site`.
- **Interface name:** `site.Service`.
- **Site identity:** `uuid.UUID` (bare, `github.com/google/uuid`). A type alias
  `type SiteID = uuid.UUID` MAY be declared for readability but the interface signatures
  below use `uuid.UUID`. `SiteRef` (architecture.md) and `SiteID string` (old
  site-service.md) are **rejected**.
- **`context.Context` is always the first parameter; `error` is always the last return.**
- **Concurrency:** safe for concurrent use across *different* sites; mutations to the
  *same* site are serialized internally by a per-site lock (in-process keyed `sync.RWMutex`
  + OS `flock` on `/data/sites/{uuid}/.git/kotoji.lock`). Single backend replica in v1
  (decision #4); the lock acquisition sits behind an interface so a
  `pg_advisory_xact_lock(hashtext(uuid))` impl can drop in for future HA.
- **Authz boundary:** `site.Service` is **NOT membership-authz-aware**. It trusts the
  `SiteID` it is given. Session→role and token→membership+scope checks are enforced *above*
  it (REST/MCP middleware). An MCP token is per-USER; the MCP layer resolves the named site,
  reads the user's membership role, and caps the call to `intersection(token.scopes, role
  scopes)` before passing the resolved `SiteID` down. The Service still validates
  paths/branches/baseSHA (defense in depth) and returns `ErrValidation` for git-level
  operation rules it owns (e.g. refusing to write/delete the `published` branch directly).

```go
package site

import (
	"context"

	"github.com/google/uuid"
)

// Service is the ONLY component that touches git on disk. All writers (zip upload,
// Monaco editor, MCP tools) and the data-plane read side funnel through this interface.
type Service interface {
	// ---- Site lifecycle (touches git repo init AND metadata, in one tx) ----

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
	// 30-day grace; the repo is retained. A reaper job later git-bundles to /data/backups
	// then reclaims disk (decision #3). Errors: ErrNotFound, ErrGit.
	DeleteSite(ctx context.Context, id uuid.UUID, actor Actor) error

	// ---- Branches ----

	ListBranches(ctx context.Context, id uuid.UUID) ([]Branch, error)

	// CreateBranch branches `from` (a branch name or SHA) into a new branch `name`.
	// `name` must be host-safe (feature-<user>-<slug> convention) and pass branch grammar.
	// Errors: ErrNotFound, ErrValidation, ErrBranchExists, ErrGit.
	CreateBranch(ctx context.Context, id uuid.UUID, name BranchName, from string) (Branch, error)

	// DeleteBranch refuses to delete "published" or "draft" (ErrValidation).
	// Errors: ErrNotFound, ErrValidation, ErrGit.
	DeleteBranch(ctx context.Context, id uuid.UUID, name BranchName) error

	// ---- Files (read) ----

	// ListFiles lists entries under in.Dir (repo-relative; "" = root) at in.Ref (a SHA) or,
	// if Ref is empty, at the tip of in.Branch. Non-recursive unless in.Recursive.
	// Returns the entries plus the ResolvedRef the listing reflects (consistency-report
	// resolution #14 — merged ListFiles signature).
	// Errors: ErrNotFound (site/branch/dir/ref), ErrValidation (bad path).
	ListFiles(ctx context.Context, in ListFilesInput) ([]FileEntry, ResolvedRef, error)

	// ReadFile returns the file at in.Ref or the tip of branch. FileContent.SHA is the
	// COMMIT SHA the caller MUST echo back as BaseSHA on a subsequent write (the lock token).
	// Errors: ErrNotFound, ErrValidation (bad path).
	ReadFile(ctx context.Context, id uuid.UUID, branch BranchName, ref, path string) (FileContent, error)

	// ---- Files (write) — all optimistic-locked on BaseSHA ----

	// WriteFile creates/overwrites one file and (if in.Commit) commits it on in.Branch.
	// OPTIMISTIC LOCK: rejects with ErrConflict unless in.BaseSHA == current tip of branch.
	// Empty BaseSHA => ErrValidation (never treated as "force"). "published" branch =>
	// ErrValidation. Returns the NEW tip CommitInfo (its SHA becomes the next BaseSHA).
	// Mirror-push is best-effort (failure => warning, not error).
	// Errors: ErrNotFound, ErrValidation, ErrConflict, ErrGit.
	WriteFile(ctx context.Context, in WriteFileInput) (CommitInfo, error)

	// DeleteFile removes one file and commits. Same lock + path + branch rules as WriteFile.
	// Errors: ErrNotFound, ErrValidation, ErrConflict, ErrGit.
	DeleteFile(ctx context.Context, id uuid.UUID, branch BranchName, path, baseSHA string, actor Actor) (CommitInfo, error)

	// ImportZip extracts an uploaded archive into branch as ONE commit, REPLACING the tree.
	// Applies ZipSlip + zip-bomb + extension guards BEFORE writing anything. Optimistic-
	// locked on baseSHA (empty BaseSHA allowed ONLY when the branch has no commits yet, i.e.
	// initial seed). "published" branch => ErrValidation.
	// Errors: ErrValidation, ErrZipSlip, ErrZipTooLarge, ErrZipTooManyFiles, ErrZipBadType,
	//         ErrConflict, ErrGit.
	ImportZip(ctx context.Context, id uuid.UUID, branch BranchName, src ZipSource, baseSHA string, actor Actor) (CommitInfo, error)

	// Commit ("Save") turns the currently-staged working set of in.Branch into one commit.
	// Used for the multi-file batch case (MCP write_file(commit:false)*N then save; Monaco
	// "save all"). Optimistic-locked on in.BaseSHA. "published" => ErrValidation.
	// Errors: ErrNotFound, ErrConflict, ErrNothingToCommit, ErrValidation, ErrGit.
	Commit(ctx context.Context, in CommitInput) (CommitInfo, error)

	// ---- Publish (draft -> published, git level) ----

	// Publish promotes in.From (default "draft") to "published" (fast-forward, else a merge
	// commit). Idempotent if published already equals the source tip. in.BaseSHA guards
	// against publishing a stale snapshot (must equal the tip of in.From). After success,
	// refreshes the served worktree, mirror-pushes "published", and updates
	// sites.published_commit_sha / published_at inside the same logical op.
	// Errors: ErrNotFound, ErrConflict (stale base), ErrPublishConflict, ErrValidation, ErrGit.
	Publish(ctx context.Context, in PublishInput) (CommitInfo, error)

	// ---- History / diff / rollback ----

	// GetDiff diffs two refs. Either may be a branch name or a SHA. If in.To is empty it
	// means the working tree of in.From's branch (uncommitted staged changes).
	// Errors: ErrNotFound (ref), ErrValidation.
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
	// the originating save/publish; it is invoked internally by WriteFile/Commit/Publish and
	// is also callable directly. Returns a warning-bearing error only for explicit calls.
	MirrorPush(ctx context.Context, id uuid.UUID, branches ...BranchName) error

	// FetchAndUpdate is the GitHub-webhook entry point: fetch origin, fast-forward the local
	// branch (typically "published") + refresh its served worktree. Non-FF is rejected and
	// flagged (never force-applied). Errors: ErrNotFound, ErrPublishConflict, ErrGit.
	FetchAndUpdate(ctx context.Context, id uuid.UUID, branch BranchName) (CommitInfo, error)

	// ---- Data-plane read side (backs resolve.TreeProvider) ----

	// ServedTree returns an immutable, ready-to-serve file tree for (siteID, branch) without
	// invoking git on the request path (materialized worktree under /data/sites/{uuid}/served).
	// Host->{handle,branch} parsing lives in resolve.Resolver, and handle->uuid lookup lives
	// in the Store — NOT in this Service.
	// Errors: ErrNotFound (site/branch).
	ServedTree(ctx context.Context, id uuid.UUID, branch BranchName) (TreeHandle, error)
}
```

> The MCP doc (`mcp.md` §7.1) and architecture doc (§2.2) show only a subset / older shape;
> they are NON-authoritative and must be read as "subset; authoritative in CANONICAL.md".

---

## 2. Domain structs & input value objects (FROZEN)

All structs are plain data. Times are `time.Time` (UTC). JSON tags are the **internal**
Go-core serialization; the **wire** contract (REST/MCP) is `openapi.yaml` + `mcp.md` and the
field-name mapping is fixed in §8.

```go
package site

import (
	"io"
	"time"

	"github.com/google/uuid"
)

// SiteID is an OPTIONAL readability alias; interface signatures use uuid.UUID directly.
type SiteID = uuid.UUID

// Handle is the unique, DNS-safe, renameable english name (see §5).
type Handle string

// BranchName is a git branch name. Reserved logical names: "published", "draft".
type BranchName string

const (
	BranchPublished BranchName = "published"
	BranchDraft     BranchName = "draft"
)

// ---- Domain (read) structs ----

// Site is the metadata + git head summary of one project.
type Site struct {
	ID             uuid.UUID  `json:"id"`
	Handle         Handle     `json:"handle"`
	OwnerID        uuid.UUID  `json:"ownerId"`
	Visibility     string     `json:"visibility"`     // "public" | "internal" | "private"
	DefaultBranch  BranchName `json:"defaultBranch"`  // usually "draft"
	GitHubRepo     string     `json:"githubRepo,omitempty"` // "owner/name" if mirrored, else ""
	PublishMode    string     `json:"publishMode"`    // "direct" | "request"
	WebRoot        string     `json:"webRoot"`        // served subdir; "" = repo root (v1)
	HasPublished   bool       `json:"hasPublished"`   // derived: published_commit_sha IS NOT NULL
	PublishedSHA   string     `json:"publishedSha,omitempty"`
	PublishedAt    *time.Time `json:"publishedAt,omitempty"`
	Description    string     `json:"description"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// Branch is one git branch with its head and a precomputed preview host label fragment.
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
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`  // bytes; 0 for dirs
	Mode  string `json:"mode"`  // git mode, e.g. "100644", "100755", "040000"
}

// FileContent is a single file read at a specific commit.
type FileContent struct {
	Path     string `json:"path"`
	Content  []byte `json:"-"`        // raw bytes; serialized separately (base64 / stream)
	SHA      string `json:"sha"`      // the COMMIT SHA the content was read at (the lock token)
	BlobSHA  string `json:"blobSha"`  // blob hash of this specific file
	Size     int64  `json:"size"`
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

// ServeTarget tells the data plane what to read from disk, read-only. (Used by the serve
// layer; the resolver produces it from the Host header.)
type ServeTarget struct {
	SiteID  uuid.UUID  `json:"siteId"`
	Handle  Handle     `json:"handle"`
	Branch  BranchName `json:"branch"`
	Root    string     `json:"-"`       // absolute path to the read-only served worktree root
	HeadSHA string     `json:"headSha"` // for cache-keying / ETag
}

// TreeHandle is the immutable served tree returned by ServedTree (backs resolve.TreeProvider).
type TreeHandle struct {
	Root       string    `json:"-"`          // absolute served-worktree root (EXCLUDES .git)
	CommitSHA  string    `json:"commitSha"`  // 40-hex; "" only in dev/empty-site
	CommitTime time.Time `json:"commitTime"` // -> Last-Modified
	Exists     bool      `json:"exists"`
}

// ---- Input value objects (write side) ----

// CreateSiteInput is the request to create a new site (empty, or seeded from a zip).
type CreateSiteInput struct {
	Handle      Handle      // validated + reserved-word checked inside CreateSite
	OwnerID     uuid.UUID   // auth subject; stamped as initial owner + owner site_members row
	Visibility  string      // "public"|"internal"|"private"; default "private"
	PublishMode string      // "direct"|"request"; default "direct"
	Description string      // optional
	Zip         *ZipSource  // optional initial content; nil => empty draft w/ placeholder index.html
	GitHubRepo  string      // optional mirror remote "owner/name"
	Actor       Actor       // who is creating (git author + audit)
}

// ZipSource carries the uploaded archive plus its declared length for guard checks.
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

// CommitInput commits the already-staged working set (the multi-file "Save" verb).
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

// Actor is the identity attributed to a git commit and the audit log.
// (consistency-report resolution #1.1: D3's Actor shape + D5's Via field.)
type Actor struct {
	UserID  uuid.UUID   // auth subject; primary key for audit_log.actor_id
	Name    string      // display name -> git author.name
	Email   string      // -> git author.email; synthesized if absent (userID@kotoji.local)
	Via     WriteSource // "upload"|"editor"|"mcp"|"system" -> Kotoji-Via trailer + audit_log.source
	TokenID *uuid.UUID  // set when acting via a site_token (MCP/API) -> audit_log.token_id
}

// WriteSource is the canonical provenance value set (matches audit_source enum, §4).
type WriteSource string

const (
	SourceUpload WriteSource = "upload"
	SourceEditor WriteSource = "editor"
	SourceMCP    WriteSource = "mcp"
	SourceSystem WriteSource = "system"
)
```

---

## 3. The FROZEN error taxonomy + `statusFor()`

Idiomatic Go: **sentinel errors** for the categories callers branch on with `errors.Is`,
plus **typed errors** (`errors.As`) where structured detail is needed. Every returned error
wraps a sentinel so the HTTP/MCP layer maps to a status/code with one `errors.Is` switch.

```go
package site

import (
	"errors"
	"strings"
)

// Sentinels — the stable, switchable categories.
var (
	ErrNotFound        = errors.New("site: not found")               // 404
	ErrValidation      = errors.New("site: validation failed")       // 422 (400 for malformed)
	ErrReservedHandle  = errors.New("site: reserved handle")         // 422
	ErrHandleTaken     = errors.New("site: handle already taken")    // 409
	ErrConflict        = errors.New("site: stale base sha")          // 409 (optimistic lock)
	ErrPublishConflict = errors.New("site: publish merge conflict")  // 409
	ErrBranchExists    = errors.New("site: branch already exists")   // 409
	ErrNothingToCommit = errors.New("site: nothing to commit")       // 409
	ErrForbidden       = errors.New("site: forbidden")               // 403
	// Zip family:
	ErrZipSlip         = errors.New("site: zip path traversal")      // 400
	ErrZipTooLarge     = errors.New("site: zip too large")           // 413
	ErrZipTooManyFiles = errors.New("site: zip too many files")      // 413
	ErrZipBadType      = errors.New("site: zip disallowed file type")// 415
	// git wrapper:
	ErrGit             = errors.New("site: git operation failed")    // 500
)

// ValidationError adds the offending field/reason. Is() ties it to ErrValidation.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return "site: validation: " + e.Field + ": " + e.Reason }
func (e *ValidationError) Is(t error) bool { return t == ErrValidation }

// ConflictError — the optimistic-lock conflict. errors.Is(err, ErrConflict) is true;
// errors.As gets the SHAs + changed paths. (consistency-report resolution #1.1/#1.6:
// D3 fields Expected/Actual + D5 field renamed to ChangedPaths.) WIRE names: expected,
// actual, changedPaths (see §8) — used identically by REST and MCP.
type ConflictError struct {
	Branch       BranchName
	Expected     string   // the BaseSHA the caller sent
	Actual       string   // the real current tip
	ChangedPaths []string // files that differ between Expected and Actual
}

func (e *ConflictError) Error() string {
	return "site: stale base sha on " + string(e.Branch) + ": expected " + e.Expected + " got " + e.Actual
}
func (e *ConflictError) Is(t error) bool { return t == ErrConflict }

// PublishConflictError carries the conflicting paths from a publish merge.
type PublishConflictError struct {
	Paths []string
}

func (e *PublishConflictError) Error() string { return "site: publish merge conflict" }
func (e *PublishConflictError) Is(t error) bool { return t == ErrPublishConflict }

// GitError is the raw os/exec failure. errors.Is(err, ErrGit) true; errors.As gets stderr.
type GitError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *GitError) Error() string { return "site: git " + strings.Join(e.Args, " ") + ": " + e.Stderr }
func (e *GitError) Is(t error) bool { return t == ErrGit }

// statusFor is the SINGLE place errors become HTTP statuses. (Context cancellation is
// surfaced as context.Canceled and handled by the request layer, mapped to 499/timeout.)
func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrForbidden):
		return 403
	case errors.Is(err, ErrConflict),
		errors.Is(err, ErrHandleTaken),
		errors.Is(err, ErrPublishConflict),
		errors.Is(err, ErrBranchExists),
		errors.Is(err, ErrNothingToCommit):
		return 409
	case errors.Is(err, ErrZipTooLarge),
		errors.Is(err, ErrZipTooManyFiles):
		return 413
	case errors.Is(err, ErrZipBadType):
		return 415
	case errors.Is(err, ErrZipSlip):
		return 400
	case errors.Is(err, ErrValidation),
		errors.Is(err, ErrReservedHandle):
		return 422
	default:
		return 500 // ErrGit + unknown
	}
}
```

**Wire error envelope** (uniform across `/api`, mirrored by MCP tool-error bodies):

```json
{ "error": { "code": "conflict", "message": "human-readable, safe to show",
             "details": { "expected": "<sha>", "actual": "<sha>", "changedPaths": ["index.html"] } } }
```

Stable machine `code` enum (REST + MCP):
`unauthenticated | forbidden | validation | conflict | not_found | handle_taken | publish_conflict | branch_exists | nothing_to_commit | too_large | unsupported_media_type | rate_limited | quota_exceeded | internal`

`code` → HTTP status map: `unauthenticated`→401, `forbidden`→403, `validation`→422,
`conflict`/`handle_taken`/`publish_conflict`/`branch_exists`/`nothing_to_commit`→409,
`not_found`→404, `too_large`→413, `unsupported_media_type`→415, `rate_limited`→429,
`quota_exceeded`→413, `internal`→500. (Malformed request bodies that never reach the Service
return 400 with code `validation`.)

---

## 4. The FROZEN, COMPLETE PostgreSQL DDL (goose `0001_init`)

> Target: PostgreSQL 18 (17 compatible). One migration file `0001_init.sql` containing the
> whole schema (the per-table split in `data-model.md` is an editorial convenience; the
> canonical artifact is this single `0001_init`). A separate seed `0002_seed_reserved.sql`
> seeds the reserved handles (§5). Applies consistency-report resolutions #1.2, #1.4, #1.10,
> #1.12, #1.13, #2(P0-2), #6(P0-6), #1.5/§8(P0-7), §P1-3, §P2-26, plus locked decisions
> #2, #3, #6.

```sql
-- +goose Up
-- +goose StatementBegin

-- ============================ extensions ============================
CREATE EXTENSION IF NOT EXISTS citext;    -- case-insensitive handles & emails
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid()

-- ============================ enums ============================
-- Per-site role. owner > editor > viewer. (consistency-report #1.10: owner, not admin.)
CREATE TYPE site_role AS ENUM ('owner', 'editor', 'viewer');

-- Served-content visibility (data plane). 3-valued (consistency-report #1.4).
--   public   : anyone with the URL (subject to instance PUBLISHED_PUBLIC cap)
--   internal : any authenticated user of this kotoji instance
--   private  : only site members (owner/editor/viewer)
CREATE TYPE site_visibility AS ENUM ('public', 'internal', 'private');

-- Which writer/origin produced an audited action (consistency-report #1.12).
--   upload : zip upload path
--   editor : Monaco / dashboard
--   mcp    : MCP tool call
--   system : webhook pulls, scheduled jobs, admin actions, migrations
CREATE TYPE audit_source AS ENUM ('upload', 'editor', 'mcp', 'system');

-- ============================ shared trigger fn ============================
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ============================ users ============================
CREATE TABLE users (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email            CITEXT      NOT NULL UNIQUE,
    display_name     TEXT        NOT NULL DEFAULT '',
    avatar_url       TEXT,
    is_admin         BOOLEAN     NOT NULL DEFAULT FALSE,  -- instance superuser (separate axis)
    can_create_sites BOOLEAN     NOT NULL DEFAULT TRUE,   -- may this user create sites? (decision #2/#8)
    is_active        BOOLEAN     NOT NULL DEFAULT TRUE,    -- soft disable
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================ user_identities ============================
CREATE TABLE user_identities (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT        NOT NULL,   -- AuthProvider key: 'google','keycloak','dev',...
    subject       TEXT        NOT NULL,   -- OIDC `sub`: stable, opaque, provider-scoped
    email_at_link CITEXT,                 -- email seen at link time (audit)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    CONSTRAINT user_identities_provider_subject_key UNIQUE (provider, subject)
);
CREATE INDEX idx_user_identities_user_id ON user_identities (user_id);

-- ============================ sessions ============================
CREATE TABLE sessions (
    id           TEXT        PRIMARY KEY,   -- opaque CSPRNG id (the __Host- cookie value)
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_agent   TEXT        NOT NULL DEFAULT '',
    ip_addr      INET
);
CREATE INDEX idx_sessions_user_id    ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

-- ============================ sites ============================
CREATE TABLE sites (
    id                   UUID            PRIMARY KEY DEFAULT gen_random_uuid(),  -- == /data/sites/{id}
    handle               CITEXT          NOT NULL UNIQUE,
    owner_id             UUID            NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    visibility           site_visibility NOT NULL DEFAULT 'private',
    default_branch       TEXT            NOT NULL DEFAULT 'draft',
    published_commit_sha TEXT,                       -- CACHE pointer into git; NULL = never published
    published_at         TIMESTAMPTZ,                -- last publish time (dashboard badge)
    publish_mode         TEXT            NOT NULL DEFAULT 'direct'   -- decision #6
                         CHECK (publish_mode IN ('direct', 'request')),
    github_repo          TEXT,                       -- 'owner/name' for mirror; NULL = no mirror
    web_root             TEXT            NOT NULL DEFAULT '',        -- served subdir; '' = repo root (v1)
    description          TEXT            NOT NULL DEFAULT '',
    deleted_at           TIMESTAMPTZ,                -- soft delete (decision #3); 30-day grace
    created_at           TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ     NOT NULL DEFAULT now(),

    -- DNS-label format (defense in depth; Go validator is the friendly primary gate).
    CONSTRAINT sites_handle_format CHECK (
        handle ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
    ),
    -- git object id: 40 hex (SHA-1) or 64 hex (SHA-256 forward-compat).
    CONSTRAINT sites_published_sha_format CHECK (
        published_commit_sha IS NULL OR published_commit_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
    )
);
CREATE UNIQUE INDEX idx_sites_github_repo ON sites (github_repo) WHERE github_repo IS NOT NULL;
CREATE INDEX idx_sites_owner_id   ON sites (owner_id);
CREATE INDEX idx_sites_updated_at ON sites (updated_at DESC);
-- handle resolution should ignore soft-deleted sites on the hot path:
CREATE INDEX idx_sites_handle_live ON sites (handle) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_sites_updated_at BEFORE UPDATE ON sites
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================ site_members ============================
CREATE TABLE site_members (
    site_id    UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       site_role   NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID        REFERENCES users(id) ON DELETE SET NULL,  -- who granted (audit)
    PRIMARY KEY (site_id, user_id)
);
CREATE INDEX idx_site_members_user_id ON site_members (user_id);

-- ============================ user_tokens ============================
-- Per-USER MCP/API token (established by migration 0004, which DROPPED the v1
-- per-project `site_tokens`). A token is owned by a user and automatically covers
-- ALL of that user's memberships; the effective scope on a given site is computed
-- at call time as intersection(token.scopes, membership-role scopes) — §6.2.
CREATE TABLE user_tokens (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- the human it acts as
    name             TEXT        NOT NULL DEFAULT '',   -- human label, e.g. "claude-laptop"
    token_prefix     TEXT        NOT NULL,              -- first 12 chars of plaintext (UI + lookup)
    token_hash       BYTEA       NOT NULL,              -- sha256(plaintext), 32 bytes
    scopes           TEXT[]      NOT NULL DEFAULT '{read,write,publish}',  -- subset of {read,write,publish}
    can_create_sites BOOLEAN     NOT NULL DEFAULT FALSE,  -- gates create_site over MCP; capped by users.can_create_sites
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at     TIMESTAMPTZ,                        -- throttled update
    expires_at       TIMESTAMPTZ,                        -- NULL = no expiry
    revoked_at       TIMESTAMPTZ,                        -- NULL = active
    CONSTRAINT user_tokens_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT user_tokens_prefix_len CHECK (char_length(token_prefix) = 12),
    CONSTRAINT user_tokens_scopes_valid CHECK (scopes <@ ARRAY['read','write','publish']::text[])
);
CREATE UNIQUE INDEX uq_user_tokens_hash   ON user_tokens (token_hash);
CREATE INDEX idx_user_tokens_prefix       ON user_tokens (token_prefix) WHERE revoked_at IS NULL;
CREATE INDEX idx_user_tokens_user_id      ON user_tokens (user_id);

-- ============================ handle_redirects ============================
CREATE TABLE handle_redirects (
    old_handle CITEXT      PRIMARY KEY,                                  -- the freed former handle
    site_id    UUID        NOT NULL REFERENCES sites(id) ON DELETE CASCADE,  -- -> current site
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT handle_redirects_format CHECK (
        old_handle ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
    )
);
CREATE INDEX idx_handle_redirects_site_id ON handle_redirects (site_id);

-- ============================ reserved_handles ============================
-- Admin-editable blocklist; baseline seeded by 0002 (§5). Go constant is the fallback.
CREATE TABLE reserved_handles (
    handle     CITEXT      PRIMARY KEY,
    reason     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============================ audit_log ============================
-- Append-only. ALL FKs ON DELETE SET NULL so the trail outlives referenced rows.
CREATE TABLE audit_log (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_user_id UUID      REFERENCES users(id)       ON DELETE SET NULL,  -- human actor; NULL for system
    site_id    UUID         REFERENCES sites(id)       ON DELETE SET NULL,  -- target; NULL for instance-level
    token_id   UUID         REFERENCES user_tokens(id) ON DELETE SET NULL,  -- if via a token (re-pointed in 0004)
    action     TEXT         NOT NULL,        -- 'site.create','file.write','publish','rollback',...
    source     audit_source NOT NULL,        -- upload|editor|mcp|system
    commit_sha TEXT,                         -- resulting git commit, if any
    metadata   JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- {paths,branch,base_sha,handle,ip,kind,...}
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_log_site_created  ON audit_log (site_id, created_at DESC);
CREATE INDEX idx_audit_log_actor_created ON audit_log (actor_user_id, created_at DESC);
CREATE INDEX idx_audit_log_metadata      ON audit_log USING gin (metadata);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS reserved_handles;
DROP TABLE IF EXISTS handle_redirects;
DROP TABLE IF EXISTS site_tokens;
DROP TABLE IF EXISTS site_members;
DROP TABLE IF EXISTS sites;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS user_identities;
DROP TABLE IF EXISTS users;
DROP TYPE IF EXISTS audit_source;
DROP TYPE IF EXISTS site_visibility;
DROP TYPE IF EXISTS site_role;
DROP FUNCTION IF EXISTS set_updated_at();
DROP EXTENSION IF EXISTS pgcrypto;
DROP EXTENSION IF EXISTS citext;
-- +goose StatementEnd
```

### 4.1 Reserved-handle seed (goose `0002_seed_reserved.sql`)

```sql
-- +goose Up
-- +goose StatementBegin
INSERT INTO reserved_handles (handle, reason) VALUES
    ('draft',     'branch/preview keyword'),
    ('preview',   'branch/preview keyword'),
    ('published', 'branch/preview keyword'),
    ('www',       'infra'),
    ('api',       'control-plane path prefix'),
    ('internal',  'infra'),
    ('host',      'path-based fallback prefix'),
    ('admin',     'reserved'),
    ('app',       'reserved'),
    ('static',    'reserved'),
    ('assets',    'reserved'),
    ('mcp',       'MCP endpoint prefix')
ON CONFLICT (handle) DO NOTHING;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DELETE FROM reserved_handles WHERE handle IN
    ('draft','preview','published','www','api','internal','host','admin','app','static','assets','mcp');
-- +goose StatementEnd
```

### 4.2 ON DELETE matrix (FROZEN)

| FK | Behavior | Why |
|---|---|---|
| `user_identities.user_id → users` | CASCADE | identities are meaningless without the user |
| `sessions.user_id → users` | CASCADE | kill sessions with the user |
| `sites.owner_id → users` | **RESTRICT** | never orphan a site/git repo; force ownership transfer |
| `site_members.site_id → sites` | CASCADE | membership is a pure join |
| `site_members.user_id → users` | CASCADE | membership is a pure join |
| `site_members.created_by → users` | SET NULL | keep the grant record |
| `user_tokens.user_id → users` | CASCADE | no orphan credentials (a token dies with its owner) |
| `handle_redirects.site_id → sites` | CASCADE | free former handles on delete |
| `audit_log.actor_user_id → users` | **SET NULL** | audit must survive referenced-row deletion |
| `audit_log.site_id → sites` | **SET NULL** | audit must survive (denormalize handle into metadata) |
| `audit_log.token_id → user_tokens` | **SET NULL** | audit must survive (re-pointed in 0004) |

> **Deleting a site** removes the `sites` row's relational dependents via cascade, but the
> on-disk repo is the SiteService's job: soft-delete sets `deleted_at`; the 30-day reaper
> `git bundle`s to `/data/backups/{uuid}` then `rm -rf`s. (decision #3)

---

## 5. Identifier / handle / branch rules (FROZEN)

### 5.1 Handle grammar

```go
package handle

const (
	HandleMinLen = 3  // create-time minimum (consistency-report #1.3; friendlier, anti-squat)
	HandleMaxLen = 63 // DNS label limit; also keeps {handle}--{branch} parseable
)

// handleRe: lowercase, start+end alnum, internal hyphens allowed.
// The no-"--" rule is a SEPARATE post-regex check (the regex permits internal hyphens).
var handleRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
```

Rules (Go validator is primary; DB CHECK is defense in depth):

| Rule | Value |
|---|---|
| Allowed chars | `[a-z0-9-]`, ASCII only (no IDN/punycode — homoglyph defense) |
| Start/end | alphanumeric (no leading/trailing hyphen) |
| No `--` substring | rejected in Go (load-bearing for the `--` branch separator) |
| Length (create) | **min 3, max 63** |
| Length (resolver) | accepts **1–63** so already-created short handles still resolve |
| Case | lowercased before store; uniqueness via `citext` |
| Reserved | not in `reservedHandles` (table ∪ Go constant) |
| Uniqueness | not an existing `sites.handle`, `handle_redirects.old_handle`, or `reserved_handles` (checked in one tx; DB UNIQUE is the final guard) |

```go
// ReservedHandles is the locked baseline blocklist: the fallback when the reserved_handles
// table is empty/unreachable AND the seed source for that table (0002_seed_reserved.sql).
// Admins may ADD to the DB table at runtime; they cannot remove these baseline entries.
var ReservedHandles = []string{
	"draft", "preview", "published", "www", "api", "internal",
	"host", "admin", "app", "static", "assets", "mcp",
}
```

### 5.2 Branch grammar

```
branch := ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$   // same shape as handle, lowercased, no "--"
```

- Preview/AI branches are named **`feature-<user>-<slug>`** (host-safe, no slashes) — this is
  a DECISION, not a recommendation (consistency-report #1.3).
- `published` and `draft` are the two reserved logical branches.
- **`{handle}--published` → `400 bad_branch`**: published is reached only via the bare
  `{handle}` host, never via `--` (consistency-report #1.3 / routing-and-serving.md §2.2).
- Host-label budget: `len(handle)+2+len(branch) ≤ 63` (one DNS label). Overflow → resolver
  `400`/`421`; control plane must avoid creating such preview URLs.
- Non-host-safe refs (containing `/`) get no clean subdomain; reachable only via path mode
  with percent-encoding. The control plane SHOULD only create host-safe branches.

### 5.3 The `--` separator rule (exact)

Given a project label `L` (the part left of `KOTOJI_BASE_DOMAIN`):
1. If `L` contains `--`, split on the **first** `--`: `handle = L[:i]`, `branch = L[i+2:]`.
2. Else `handle = L`, `branch = "published"`.

Because handles and branches both forbid `--`, a well-formed label has at most one `--`.
A malformed `a--b--c` yields `handle=a`, `branch=b--c`, which then fails branch validation →
`400 bad_branch` (fail-closed).

### 5.4 Path-relative file paths

`validatePath` (used by every read/write): reject empty (for files), leading `/`, any `..`
element, backslashes, NUL bytes, the `.git` segment, and non-allowlisted extensions; use
`filepath.IsLocal` + `path.Clean` then re-check no `..` remains. The git layer additionally
operates via the index (blob writes), never `os.Open(join(root, userPath))`.

### 5.5 Rename / redirect behavior

- Rename `old → new` (atomic, in SiteService tx): `INSERT handle_redirects(old_handle=old,
  site_id)` + `UPDATE sites SET handle=new`. If `new` is a stale redirect of the *same* site
  (rename-back), delete that redirect row. Files do NOT move (UUID-keyed path).
- **Data plane** follows redirects and emits `301` to the current handle (preserving
  branch+path+query). **Control API** (`GetSiteByHandle`) 404s old handles (confirmed
  asymmetry, consistency-report #P2-12). Redirects are permanent (cheap rows; admin can prune).

### 5.6 Extension allowlist (single source of truth)

The serve-side MIME table in `routing-and-serving.md` §5.3 (exported as one Go map
`site.MIMEByExt`) is the **single source of truth** (consistency-report #1.8):
- **Upload allowlist** = `keys(MIMEByExt)`.
- **MCP `write_file` allowlist** = the same set **minus** large-media types
  (`.mp4 .webm .mp3 .wav .pdf`) — MCP is text-first; binaries are upload-only (decision-adjacent,
  consistency-report #P1-7). Includes at minimum: `.html .htm .css .js .mjs .json .map .svg
  .png .jpg .jpeg .gif .webp .avif .ico .woff .woff2 .ttf .otf .txt .xml .webmanifest
  .manifest .csv .wasm`.

---

## 6. Role → capability matrix + token-scope capping (FROZEN, decision #2)

Two **orthogonal** axes:
- **Per-site role** (`site_members.role`): `owner | editor | viewer`.
- **Token/API scope** (`user_tokens.scopes`): subset of `{read, write, publish}`, chain
  `read ⊂ write ⊂ publish`. A token is owned by a **user** (`user_tokens.user_id`) and
  automatically covers ALL of that user's memberships (no per-project binding).
- **Instance superuser**: `users.is_admin` (a third, separate axis — admin screen, quotas,
  reserved-word edits; not a site role).
- **Site-creation capability**: `users.can_create_sites` and `user_tokens.can_create_sites`.

### 6.1 Role → capability matrix

| Action | owner | editor | viewer |
|---|:---:|:---:|:---:|
| Read files / history / diff / log | ✅ | ✅ | ✅ |
| View previews (draft / feature-*) | ✅ | ✅ | ✅ |
| Write/save files (editor + MCP write) | ✅ | ✅ | ❌ |
| Create / delete branches | ✅ | ✅ | ❌ |
| Upload zip / import | ✅ | ✅ | ❌ |
| Rollback | ✅ | ✅ | ❌ |
| Publish (when `publish_mode='direct'`) | ✅ | ✅ | ❌ |
| Request publish (when `publish_mode='request'`) | ✅ (or direct) | ✅ | ❌ |
| Rename handle | ✅ | ❌ | ❌ |
| Delete site (soft) | ✅ | ❌ | ❌ |
| Manage members (add/remove/role) | ✅ | ❌ | ❌ |
| Issue / revoke site tokens | ✅ | ❌ | ❌ |
| Set GitHub mirror | ✅ | ❌ | ❌ |
| Edit site settings (visibility, publish_mode, web_root) | ✅ | ❌ | ❌ |

Notes:
- **Viewers see drafts/previews** (they are trusted site members) but make **no writes**.
- **Editors publish directly** under the default `publish_mode='direct'` (small-team default).
  Set `publish_mode='request'` per site to route non-owner publishes through a GitHub PR.
- `users.is_admin` overrides for instance operations only; it does NOT silently grant
  per-site write — admins act on a site by being a member (or via explicit admin tooling).

### 6.2 Token-scope × role capping rule (membership-capped, per-user token)

A token is **owned by a user** (`user_tokens.user_id`) and automatically covers EVERY
project the user is a member of. On a given site, the token's *effective* capability =
**intersection** of:
1. the token's `scopes` (chain `read ⊂ write ⊂ publish`), AND
2. the scopes the user's **membership role** grants on THAT site, re-evaluated on EVERY
   request (`site_members.role` of `user_tokens.user_id`): `owner/editor → {read,write,
   publish}`, `viewer → {read}`.

Because step 2 is re-evaluated per call, removing the user's membership (or downgrading the
role) **instantly** limits the token — no re-issue — and the token can **never exceed the
user's own access**.

Concretely, on a site where the user is a member:
- `read` scope ⇒ read tools only.
- `write` scope ⇒ read + write/save/rollback/create_branch — **only if the user is owner or
  editor on that site** (capped to read where the user is a viewer).
- `publish` scope ⇒ write + publish — **only if the user may publish** (owner, or editor
  under `publish_mode='direct'`).
- A user who is **not a member** of the named site is refused with `not_found` (no existence
  leak) — the token cannot reach it at all.
- `create_site` via MCP requires `user_tokens.can_create_sites = true` (default FALSE) AND
  the user's `users.can_create_sites = true` (decision #2/#8). The new site is owned by the
  user, so the SAME token immediately covers it.
- A token can never exceed `owner`; "delete site / manage members / issue tokens" are **not**
  grantable to any token in v1.

Enforcement point: REST/MCP middleware (the Service is not membership-authz-aware, §1). The
MCP layer resolves the named `site` and caps the call to the user's membership
(membership-capped authorization REPLACES the old "no content tool takes a site selector"
pin; `mcp.md` §4).

---

## 7. Cross-reference: indexes & FKs

All indexes and FK actions are defined inline in §4 and summarized in §4.2. The authoritative
set is the DDL in §4; `data-model.md` §4 (sqlc query catalogue) targets exactly these tables.

---

## 8. Wire ↔ Go field-name mapping (drift-free contract)

The single set of wire names used by **both** REST (`openapi.yaml`) and MCP (`mcp.md`):

| Concept | Go (core) | Wire (REST + MCP) |
|---|---|---|
| Optimistic-lock base | `WriteFileInput.BaseSHA` | `baseSha` |
| Conflict: sent SHA | `ConflictError.Expected` | `expected` |
| Conflict: real tip | `ConflictError.Actual` | `actual` |
| Conflict: changed files | `ConflictError.ChangedPaths` | `changedPaths` |
| Read lock token | `FileContent.SHA` | `commit` (MCP) / `sha` (REST file read) |
| Provenance | `CommitInfo.Via` / `Actor.Via` | `via` (values `upload\|editor\|mcp\|system`) |
| Published pointer | `Site.PublishedSHA` | `publishedSha` |
| Published flag | derived (`PublishedSHA != ""`) | `hasPublished` (REST) / `is_published` (MCP, derived) |

- The frontend (`design.md` §4.1) MUST use `expected`/`actual`/`changedPaths` — NOT the old
  `baseSha`/`currentSha` (consistency-report #1.6).
- `via` value mapping is fixed: `ui`/`monaco` → `editor`; `webhook`/`github`/`admin` →
  `system` (finer distinction in `audit_log.metadata.kind`) (consistency-report #1.12).
- Token plaintext format: **`kotoji_pat_<base62>`**, ≥160 bits CSPRNG; only `sha256(hash)` +
  12-char `token_prefix` stored. A token is **per-user** (`user_tokens.user_id`) and
  membership-capped — it covers all the user's projects, effective scope = token ∩ role (§6.2).

---

## 9. Locked decisions (the 8)

1. **API sync.** Hand-written **OpenAPI 3.1** (`openapi.yaml`) is the source of truth. Go
   types via **oapi-codegen** (`-generate types`, hand-written chi handlers); TS client via
   **openapi-typescript** + **openapi-fetch**. A **CI drift gate** regenerates both and fails
   if the working tree changes.
2. **Per-site roles.** `owner` (full: members, rename, delete, publish, edit, branches),
   `editor` (edit + publish + create/delete branches), `viewer` (read incl. previews; NO
   writes). Instance superuser = `users.is_admin` (separate axis). Token/API scope chain
   `read ⊂ write ⊂ publish`. A token is **per-user** (`user_tokens`) and covers all the
   user's memberships; on each site its effective scope is **capped by the user's role on
   that site**, re-evaluated per request (§6.2). Both users and tokens carry `can_create_sites`.
3. **Soft delete.** `sites.deleted_at`; **30-day** grace (handle stays reserved); the reaper
   makes a `git bundle` backup to `/data/backups/{uuid}` then reclaims disk.
4. **Single backend replica for v1.** In-process per-site keyed mutex + `flock`. HA is not a
   goal; the lock seam is interfaced so a `pg_advisory_xact_lock` impl can drop in later.
5. **i18n.** `next-intl`; all UI copy as message keys; ship **ja (default) + en** at launch.
6. **Per-site `publish_mode ∈ {direct, request}`**, default **`direct`**.
7. **One registrable domain for v1.** Hosted content under `*.<KOTOJI_BASE_DOMAIN>` (e.g.
   `*.hosting.example.com`). **`__Host-` cookies everywhere**; previews use a signed
   **preview-grant → host-only `kotoji_preview` cookie** (no `Domain` attribute).
   `*.kotoji-usercontent.com` (a second registrable domain for served content) is documented
   as **FUTURE hardening**, not v1.
8. **AI-autonomous `create_site` over MCP is OFF by default**; opt-in via
   `site_tokens.can_create_sites = true` (also requires `users.can_create_sites`). No token is
   ever minted over MCP; the new site's first token is issued only in the dashboard.

---

*End of CANONICAL.md — the law. Companion: [`openapi.yaml`](./openapi.yaml).*
