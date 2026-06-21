# SiteService Contract

> The single git boundary of kotoji. Every write — Zip upload, the Monaco editor,
> and MCP — funnels through this one Go interface. It is the **DI seam**: handlers
> depend on the interface, never on git or the filesystem directly, so the whole
> control plane is unit-testable against a fake.

Status: **locked design, implementation-ready**. Package: `internal/site`.

---

## 1. Design rules this contract obeys

These are restated from the spec so the contract is self-contained; do not relitigate.

1. **git is the single source of truth.** Postgres holds *metadata only* (uuid↔handle,
   owner, members, tokens, sessions). The repo at `/data/sites/{uuid}/.git` holds *content
   and history*. `SiteService` is the only component that opens, reads, or mutates a repo.
2. **1 site = 1 repo.** Path is keyed by the immutable **UUID**, never the handle, so a
   handle rename moves no files.
3. **Branches generalized.** `published` (served in prod), `draft` (default working branch),
   `feature-*` (per-user / AI). Each branch is previewable.
4. **Save = commit to a working branch; Publish = a *separate* promotion of
   `draft → published`.** Broken drafts never auto-serve.
5. **Optimistic locking is mandatory on every content write.** The caller passes the base
   commit SHA it read from; the server rejects on mismatch (`ErrConflict`).
6. **Mirror push, not publish.** A successful commit *may* mirror-push the branch to a GitHub
   remote for backup/external diff. Push failure must NOT fail the local commit (best-effort,
   surfaced as a warning). PRs are delegated to GitHub; this service never builds a PR system.
7. **Serving resolves UUID+branch from the Host header (or a `/host/{handle}/...` path
   fallback) behind a swappable resolver.** `ResolveForServing` is that resolver's git-side
   half: handle/branch → on-disk tree root for the data plane.

### Where the boundary actually sits

```
 Zip upload handler ─┐
 Monaco save handler ─┤        ┌──────────────────────────┐      /data/sites/{uuid}/.git
 MCP tool handlers ───┼──────▶ │   site.Service (iface)   │ ───▶  (git CLI via os/exec)
 Data-plane serve  ───┘        └──────────────────────────┘      + GitHub remote (mirror)
                                          ▲
                                          │ injected
                          gitService (prod)  |  fakeService (tests)
```

`SiteService` owns git **and** the metadata reads/writes that must stay transactionally
consistent with git (handle uniqueness, reserved-word block, owner stamping at create).
It depends on two injected collaborators of its own — a `Store` (Postgres, via sqlc) and a
`gitRunner` (the `os/exec` wrapper) — so the *implementation* is itself testable, while
*callers* test against the `Service` interface. See §10.

---

## 2. Package layout

```
backend/
  internal/
    site/
      service.go        // the Service interface (this contract) + domain structs
      errors.go         // typed error taxonomy
      git_service.go    // gitService: prod impl (Store + gitRunner)
      gitrunner.go      // gitRunner interface + execRunner (os/exec) impl
      handle.go         // handle validation + reserved words
      resolve.go        // host/path parsing -> {handle, branch}
      fake.go           // FakeService for handler tests (in-memory)
      service_test.go   // table-driven contract tests
    store/              // sqlc-generated metadata queries (the Store impl)
```

`internal/` keeps the interface unimportable outside the backend module — it is an internal
seam, not a public API. The *public* contract is the OpenAPI/JSON API, not this Go interface.

---

## 3. Domain structs

All structs are plain data (no methods that touch git). Times are UTC `time.Time`.
JSON tags are present because these structs are reused by the REST/MCP serialization layer.

```go
package site

import "time"

// SiteID is the immutable internal UUID (storage path + git identity).
// Stored as a string in canonical UUIDv4 form, e.g. "7f3a1c2e-....".
type SiteID string

// Handle is the unique, DNS-safe, renameable english name. See handle.go for rules.
type Handle string

// BranchName is a git branch name. Reserved logical names: "published", "draft".
// Working branches: "draft", "feature-<slug>".
type BranchName string

const (
    BranchPublished BranchName = "published"
    BranchDraft     BranchName = "draft"
)

// Site is the metadata + git head summary of one project.
type Site struct {
    ID        SiteID    `json:"id"`
    Handle    Handle    `json:"handle"`
    OwnerID   string    `json:"ownerId"`            // user id (auth subject)
    GitHubRepo string   `json:"githubRepo,omitempty"` // "owner/name" if mirror configured, else ""
    HasPublished bool   `json:"hasPublished"`       // does the published branch exist yet?
    DefaultBranch BranchName `json:"defaultBranch"` // usually "draft"
    CreatedAt time.Time `json:"createdAt"`
    UpdatedAt time.Time `json:"updatedAt"`          // last commit time across branches
}

// Branch is one git branch with its head and a precomputed preview hostname fragment.
type Branch struct {
    Name      BranchName `json:"name"`
    HeadSHA   string     `json:"headSha"`           // full 40-char SHA of the branch tip
    IsPublished bool     `json:"isPublished"`       // Name == BranchPublished
    LastCommit CommitInfo `json:"lastCommit"`
    // PreviewSubdomain is the host label for this branch:
    //   "{handle}" for published, "{handle}--{branch}" otherwise.
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
    Path    string `json:"path"`
    Content []byte `json:"-"`        // raw bytes; serialized separately (base64 or stream)
    SHA     string `json:"sha"`      // the COMMIT SHA the content was read at (the lock token)
    Size    int64  `json:"size"`
    IsBinary bool  `json:"isBinary"` // detected via NUL-byte heuristic
}

// CommitInfo is one entry in the history.
type CommitInfo struct {
    SHA       string    `json:"sha"`        // full 40-char SHA
    ShortSHA  string    `json:"shortSha"`   // 7-char
    Message   string    `json:"message"`
    AuthorName  string  `json:"authorName"`
    AuthorEmail string  `json:"authorEmail"`
    Committed time.Time `json:"committed"`
    Parents   []string  `json:"parents"`    // parent SHAs (1 normal, 2 merge)
}

// DiffResult is a unified diff between two refs (or a ref and the working set).
type DiffResult struct {
    FromSHA string     `json:"fromSha"`
    ToSHA   string     `json:"toSha"`
    Files   []FileDiff `json:"files"`
}

type FileDiff struct {
    Path       string `json:"path"`
    OldPath    string `json:"oldPath,omitempty"` // set on rename
    Status     string `json:"status"`            // "added" | "modified" | "deleted" | "renamed"
    Additions  int    `json:"additions"`
    Deletions  int    `json:"deletions"`
    IsBinary   bool   `json:"isBinary"`
    UnifiedPatch string `json:"unifiedPatch"`     // unified-diff hunk text; empty if binary
}

// ServeTarget tells the data plane what to read from disk, read-only.
type ServeTarget struct {
    SiteID SiteID     `json:"siteId"`
    Handle Handle     `json:"handle"`
    Branch BranchName `json:"branch"`
    // Root is an absolute path to a read-only checkout/worktree of {branch}'s tree.
    // The data plane joins the request path onto Root and serves the file.
    Root   string     `json:"-"`
    HeadSHA string    `json:"headSha"` // for cache-keying / ETag
}
```

### Input value objects (write side)

Grouping write inputs into structs keeps signatures stable as fields are added and makes
table-driven tests readable.

```go
// CreateSiteInput is the request to create a new site (empty, or seeded from a zip).
type CreateSiteInput struct {
    Handle  Handle  // validated + reserved-word checked inside CreateSite
    OwnerID string  // auth subject; stamped as initial owner
    // Zip is optional initial content. If nil, an empty draft (single ".keep") is created.
    Zip     *ZipSource
    // GitHubRepo optionally configures the mirror remote at creation, "owner/name".
    GitHubRepo string
}

// ZipSource carries the uploaded archive plus its declared length for guard checks.
type ZipSource struct {
    Reader      io.ReaderAt // archive/zip needs ReaderAt + Size
    Size        int64       // declared content length (Content-Length); guarded
    Filename    string      // original name, for the commit message only
}

// WriteFileInput is a single optimistic-locked content write.
type WriteFileInput struct {
    SiteID  SiteID
    Branch  BranchName
    Path    string // repo-relative; ZipSlip/abs/.. rejected by validatePath
    Content []byte
    // BaseSHA is the commit SHA the caller READ from. The write is rejected with
    // ErrConflict unless BaseSHA == current tip of Branch. REQUIRED (empty => ErrValidation).
    BaseSHA string
    Actor   Actor // who is committing (name/email/subject) -> git author + audit
}

// CommitInput commits the already-staged working set (used after a batch of writes,
// or as the "Save" verb that turns staged changes into one commit).
type CommitInput struct {
    SiteID  SiteID
    Branch  BranchName
    Message string
    BaseSHA string // same optimistic-lock semantics as WriteFileInput
    Actor   Actor
}

// Actor is the identity attributed to a git commit and the audit log.
type Actor struct {
    Subject string // stable auth subject (sub claim) — primary key for audit
    Name    string // display name -> git author.name
    Email   string // -> git author.email; synthesized if absent (subject@kotoji.local)
}

// LogOptions paginates history.
type LogOptions struct {
    Branch BranchName
    Limit  int    // default 50, max 500 (clamped)
    Before string // cursor: return commits strictly older than this SHA (exclusive)
    Path   string // optional: restrict to commits touching this path
}
```

---

## 4. The interface

`context.Context` is always the first parameter; `error` is always the last return.
Method order groups by lifecycle: site CRUD → branches → files → save/publish →
history → serving.

```go
package site

import (
    "context"
    "io"
)

// Service is the ONLY component that touches git. All three writers (zip, editor, MCP)
// and the data-plane resolver depend on this interface, never on git directly.
//
// Concurrency: every method is safe for concurrent use across DIFFERENT sites. Mutations
// to the SAME site are serialized internally (see §8). Read methods take a read lock.
type Service interface {
    // ---- Site lifecycle (touches both git repo init and metadata) ----

    // CreateSite validates+reserves the handle, creates /data/sites/{uuid}/.git with an
    // initial commit on "draft" (empty .keep, or seeded from in.Zip), records metadata,
    // and (if GitHubRepo set) configures the mirror remote. Atomic: on any failure the
    // partially-created repo dir and metadata row are rolled back.
    // Errors: ErrValidation, ErrReservedHandle, ErrHandleTaken, ErrZip*, ErrGit.
    CreateSite(ctx context.Context, in CreateSiteInput) (Site, error)

    // GetSite returns metadata + head summary. Errors: ErrNotFound.
    GetSite(ctx context.Context, id SiteID) (Site, error)

    // GetSiteByHandle resolves a (current) handle to its site. Old handles 404 here;
    // redirect handling is the HTTP layer's job (it consults the handle-history table).
    // Errors: ErrNotFound.
    GetSiteByHandle(ctx context.Context, h Handle) (Site, error)

    // ListSites lists sites visible to ownerID (membership filtered by the Store).
    // Empty ownerID with the admin flag set lists all; enforcement is the caller's.
    ListSites(ctx context.Context, ownerID string) ([]Site, error)

    // RenameHandle changes the handle, recording old->new in the redirect table.
    // Files DO NOT move (path is UUID-keyed). The GitHub repo rename, if configured,
    // is best-effort and surfaced as a warning, not a hard failure.
    // Errors: ErrNotFound, ErrValidation, ErrReservedHandle, ErrHandleTaken.
    RenameHandle(ctx context.Context, id SiteID, newHandle Handle) (Site, error)

    // DeleteSite removes the repo dir and metadata (soft-deletable; see §11 gap).
    // Errors: ErrNotFound, ErrGit.
    DeleteSite(ctx context.Context, id SiteID) error

    // ---- Branches ----

    ListBranches(ctx context.Context, id SiteID) ([]Branch, error)

    // CreateBranch branches `from` (a branch name or SHA) into a new branch `name`.
    // Validates name (no reserved-word collision except the canonical draft/published it
    // already manages; "feature-*" recommended for user/AI branches).
    // Errors: ErrNotFound, ErrValidation, ErrBranchExists, ErrGit.
    CreateBranch(ctx context.Context, id SiteID, name BranchName, from string) (Branch, error)

    // DeleteBranch refuses to delete "published" or "draft" (ErrValidation).
    DeleteBranch(ctx context.Context, id SiteID, name BranchName) error

    // ---- Files (read) ----

    // ListFiles lists entries under dir (repo-relative; "" = root) at the tip of branch.
    // Non-recursive by default; recursive lists the whole subtree.
    // Errors: ErrNotFound (site/branch/dir).
    ListFiles(ctx context.Context, id SiteID, branch BranchName, dir string, recursive bool) ([]FileEntry, error)

    // ReadFile returns the file at the tip of branch. FileContent.SHA is the COMMIT SHA
    // that the caller MUST echo back as BaseSHA on a subsequent write (the lock token).
    // Errors: ErrNotFound, ErrValidation (bad path).
    ReadFile(ctx context.Context, id SiteID, branch BranchName, path string) (FileContent, error)

    // ---- Files (write) — all optimistic-locked ----

    // WriteFile creates/overwrites one file and commits it on in.Branch in a single op.
    // OPTIMISTIC LOCK: rejects with ErrConflict unless in.BaseSHA == current tip of branch.
    // Returns the NEW tip CommitInfo (its SHA becomes the next BaseSHA).
    // Path is validated (no abs, no "..", allowlisted extension). Mirror-push best-effort.
    // Errors: ErrNotFound, ErrValidation, ErrConflict, ErrGit.
    WriteFile(ctx context.Context, in WriteFileInput) (CommitInfo, error)

    // DeleteFile removes one file and commits. Same lock + path rules as WriteFile.
    // Errors: ErrNotFound, ErrValidation, ErrConflict, ErrGit.
    DeleteFile(ctx context.Context, id SiteID, branch BranchName, path, baseSHA string, actor Actor) (CommitInfo, error)

    // ImportZip extracts an uploaded archive into branch as ONE commit, replacing the
    // tree (or merging — see policy in §7). Applies ZipSlip + zip-bomb + extension guards
    // BEFORE writing anything to disk. Optimistic-locked on baseSHA.
    // Errors: ErrValidation, ErrZipSlip, ErrZipTooLarge, ErrZipTooManyFiles, ErrZipBadType,
    //         ErrConflict, ErrGit.
    ImportZip(ctx context.Context, id SiteID, branch BranchName, zip ZipSource, baseSHA string, actor Actor) (CommitInfo, error)

    // Commit ("Save") turns the currently-staged working set of branch into one commit.
    // For the common single-file path, prefer WriteFile (write+commit atomically). Commit
    // exists for multi-file batches staged via repeated lower-level writes within one txn.
    // Optimistic-locked on in.BaseSHA. Errors: ErrNotFound, ErrConflict, ErrNothingToCommit, ErrGit.
    Commit(ctx context.Context, in CommitInput) (CommitInfo, error)

    // ---- Publish (draft -> published, git level) ----

    // Publish promotes a source branch (default "draft") to "published". See §6 for the
    // exact git operation (fast-forward or merge-to-published commit). Idempotent if
    // published already equals the source tip (returns current published CommitInfo).
    // After success, mirror-pushes "published". Triggers a data-plane redeploy signal
    // (the HTTP layer invalidates ServeTarget caches).
    // Errors: ErrNotFound, ErrPublishConflict, ErrGit.
    Publish(ctx context.Context, id SiteID, from BranchName) (CommitInfo, error)

    // ---- History / diff / rollback ----

    // GetDiff diffs two refs. Either ref may be a branch name or a SHA. If `to` is the
    // empty string it means the working tree of `from`'s branch (uncommitted changes).
    GetDiff(ctx context.Context, id SiteID, from, to string) (DiffResult, error)

    // GetLog returns history for opts.Branch, newest first, paginated by opts.
    GetLog(ctx context.Context, id SiteID, opts LogOptions) ([]CommitInfo, error)

    // Rollback creates a NEW commit on branch whose tree equals the tree at toSHA
    // (a `git read-tree` / revert-to-tree, NOT a history-rewriting reset, so history is
    // preserved and the action is itself optimistic-locked on the current tip).
    // baseSHA must equal the current tip of branch. Errors: ErrNotFound, ErrConflict, ErrGit.
    Rollback(ctx context.Context, id SiteID, branch BranchName, toSHA, baseSHA string, actor Actor) (CommitInfo, error)

    // ---- Serving (data plane) ----

    // ResolveForServing parses a host header OR a "/host/{handle}/..." path, resolves the
    // (current) handle -> uuid and the branch (published if no "--" segment), and returns a
    // ServeTarget pointing at a READ-ONLY tree root for that branch's tip. Implementations
    // maintain a per-(uuid,branch) checked-out worktree refreshed on commit/publish.
    // Errors: ErrNotFound (unknown handle/branch), ErrValidation (malformed host/path).
    ResolveForServing(ctx context.Context, hostOrPath string) (ServeTarget, error)
}
```

---

## 5. Optimistic-locking semantics (precise)

This is the single most important behavioral guarantee — it prevents an AI (via MCP) or a
stale editor tab from silently clobbering newer work.

- **The lock token is a COMMIT SHA, not a blob/file SHA.** `ReadFile` and `GetSite`/`ListBranches`
  return the current **tip SHA of the branch**. That same SHA is the `BaseSHA` the caller must
  echo on the next write to that branch.
- **Compared value:** on `WriteFile`/`DeleteFile`/`ImportZip`/`Commit`/`Rollback`, the service
  reads the **current tip of `Branch`** under the per-site write lock (§8) and compares it
  to the supplied `BaseSHA`:
  - equal → proceed: stage change, commit, the new commit becomes the tip.
  - not equal → **reject with `ErrConflict`** carrying both `Expected` (BaseSHA) and
    `Actual` (current tip) so the UI/MCP can fetch the new state and let the user re-apply.
  - empty `BaseSHA` → `ErrValidation` ("base SHA required"). Never treat empty as "force".
- **Branch-level granularity, not file-level.** We lock on the *branch tip*, not the *file's
  last-touching commit*. This is intentionally conservative: two writes to *different* files on
  the same branch will serialize, and the second sees `ErrConflict` if it didn't read the first's
  commit. Rationale: it is the simplest correct rule, it matches "git is the truth", and it
  prevents lost updates with zero merge logic. A future relaxation (3-way file merge when paths
  are disjoint) is a documented gap (§11), not v1.
- **No force flag in v1.** Recovery is explicit: re-read (`ReadFile`/`GetLog`), reconcile,
  re-submit with the new BaseSHA. This keeps "AI overwrote my work" impossible by construction.
- **Publish has its own lock** (`ErrPublishConflict`): see §6.

`ErrConflict` shape lets callers act programmatically:

```go
type ConflictError struct {
    Branch   BranchName
    Expected string // the BaseSHA the caller sent
    Actual   string // the real current tip
}
func (e *ConflictError) Error() string { /* "stale base sha ..." */ }
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }
```

---

## 6. Publish at the git level (precise)

`Publish(ctx, id, from)` (default `from = "draft"`):

1. Acquire the per-site **write** lock.
2. Resolve `srcTip = rev-parse {from}`. If `from` does not exist → `ErrNotFound`.
3. If `published` does not yet exist → create it pointing at `srcTip` (first publish).
   Set `Site.HasPublished = true`.
4. If `published` exists:
   - `git merge-base --is-ancestor published {from}` → if `published` is an ancestor of
     `from`, **fast-forward** `published` to `srcTip` (clean promotion, no new commit).
   - else (published has diverged — e.g. a hotfix was committed directly on published, or a
     GitHub merge landed) → create a **merge commit** on `published` (`git merge -m
     "publish: {from}@{shortSHA}" {from}`). If the merge has content conflicts →
     `ErrPublishConflict` (carries conflicting paths); the user resolves on a branch and
     republishes. We never force-overwrite published.
5. Refresh the read-only `published` worktree used by `ResolveForServing`, then **mirror-push**
   `published` to GitHub (best-effort; failure → warning, not error).
6. Return the new `published` `CommitInfo`.

**Idempotency:** if `published` tip already equals `srcTip`, steps 4–5 are a no-op and the
current `published` `CommitInfo` is returned (so a double-click can't error).

**Publish ≠ push.** Mirror-push happens on *every* successful commit/publish for backup; it is
not what makes content public. Only step 3/4 (moving the `published` ref) makes it public.

**GitHub-merge path (PR delegated):** when a PR is merged to `published` on GitHub, a webhook
hits the HTTP layer, which calls a thin `PullPublished(ctx, id)` flow (a fetch + fast-forward of
the local `published` ref + worktree refresh). For v1 this is folded into `Publish` semantics
via an internal `from = "origin/published"` variant; exposed publicly as a webhook handler, not
an extra interface method, to keep the surface minimal. (Listed as a refinement in §11.)

---

## 7. ImportZip guards (security-critical)

Order matters: **validate fully, then write**. Never extract incrementally to the live tree.

1. **Declared-size gate.** Reject if `zip.Size > MaxZipBytes` (config, default **50 MiB**)
   → `ErrZipTooLarge` before reading.
2. **Open via `archive/zip` with the `ReaderAt`+`Size`** (central-directory based; avoids
   trusting a streamed local header).
3. **Entry-count gate.** Reject if `len(r.File) > MaxZipEntries` (default **2000**) →
   `ErrZipTooManyFiles`.
4. **Per-entry, before extracting any:**
   - **ZipSlip:** compute `target := filepath.Join(destRoot, entry.Name)`; reject unless
     `strings.HasPrefix(target, destRoot+string(os.PathSeparator))` AND
     `filepath.IsLocal(entry.Name)` (Go 1.20+). Reject absolute paths, `..`, and symlink modes
     (`entry.Mode()&os.ModeSymlink != 0`) → `ErrZipSlip`.
   - **Extension allowlist:** ext ∈ {`.html .htm .css .js .mjs .json .svg .png .jpg .jpeg .gif
     .webp .ico .txt .md .woff .woff2 .ttf .map .xml .webmanifest`} (config-overridable).
     Reject others → `ErrZipBadType`. Directories pass.
   - **Decompressed-size accounting:** sum `entry.UncompressedSize64`; if running total
     `> MaxUncompressedBytes` (default **200 MiB**) → `ErrZipTooLarge` (zip-bomb guard). Also
     enforce a **per-entry** decompressed cap and a max **compression ratio** (e.g. reject if a
     single entry’s uncompressed/compressed > 100×) to catch nested bombs.
5. **Extract into a fresh temp dir**, then `git add`-the-tree, commit on `branch` (lock-checked
   on `baseSHA`), and swap. On any per-entry failure during extraction, the temp dir is
   discarded and nothing is committed.
6. **Tree-replace policy (v1):** ImportZip on an existing branch **replaces** the served tree
   (the upload is the new truth for that branch) rather than merging, matching the "drop a folder,
   get a URL" mental model. (Merge-on-upload is a §11 gap.)

```go
// Defaults (config-overridable typed constants).
const (
    MaxZipBytes          = 50 << 20  // 50 MiB compressed (declared)
    MaxUncompressedBytes = 200 << 20 // 200 MiB total expanded
    MaxZipEntries        = 2000
    MaxEntryUncompressed = 50 << 20  // 50 MiB single file
    MaxCompressionRatio  = 100       // uncompressed/compressed per entry
)
```

---

## 8. Per-repo concurrency (mutex strategy)

git working operations on one repo (checkout, add, commit, merge) are **not** safe to run
concurrently against the same `.git`. Strategy:

- A **per-site `sync.RWMutex`**, looked up from a sharded registry keyed by `SiteID`:

```go
type lockRegistry struct {
    mu    sync.Mutex
    locks map[SiteID]*sync.RWMutex
}
func (r *lockRegistry) get(id SiteID) *sync.RWMutex { /* lazily create, never delete in v1 */ }
```

- **Read methods** (`GetSite`, `ListFiles`, `ReadFile`, `GetDiff`, `GetLog`, `ListBranches`,
  `ResolveForServing`) take `RLock`. They use read-only git plumbing (`git cat-file`,
  `git ls-tree`, `git log`) or `go-git` read APIs, so concurrent reads are safe.
- **Write methods** (`WriteFile`, `DeleteFile`, `ImportZip`, `Commit`, `Publish`, `Rollback`,
  `CreateBranch`, `DeleteBranch`, `RenameHandle`, `DeleteSite`) take the full `Lock`.
- The lock is held for the **whole read-tip → stage → commit** sequence, so the optimistic-lock
  comparison and the commit are atomic w.r.t. other writers in this process. (The BaseSHA check
  still matters across *processes*/replicas and across the read-then-write *gap on the client*.)
- **Cross-replica note:** if kotoji is ever scaled to >1 backend replica over a shared `/data`
  volume, the in-process mutex is insufficient. v1 ships **single-writer** (one backend replica);
  multi-replica needs an advisory lock (Postgres `pg_advisory_xact_lock(hashtext(uuid))` or a git
  ref-based CAS). Flagged in §11. The BaseSHA optimistic check is the cross-replica safety net
  even before then.
- Context cancellation: every exec call uses `exec.CommandContext(ctx, ...)`; a cancelled ctx
  kills the git child process and the method returns `ctx.Err()` wrapped as `ErrGit`-compatible
  (still `errors.Is(err, context.Canceled)`).

---

## 9. git-CLI-backed implementation strategy

The production impl `gitService` shells out to the **git CLI** via `os/exec` for full fidelity
(commit author override, branch, merge, archive, push, gc) and uses **go-git** only for cheap
read paths where it avoids a fork (optional optimization).

```go
// gitRunner is the thin os/exec seam UNDER the Service. gitService depends on it so we can
// even unit-test gitService itself against a fake runner (no real git needed in CI for the
// branching/error-mapping logic).
type gitRunner interface {
    // Run executes `git -C repoDir <args...>` with the given stdin, returning stdout.
    // Non-zero exit -> *GitError{Args, ExitCode, Stderr}. Honors ctx cancellation.
    Run(ctx context.Context, repoDir string, stdin []byte, args ...string) (stdout []byte, err error)
}

type execRunner struct {
    gitBin string // resolved "git" path; the Docker image MUST include the git binary
    env    []string // GIT_AUTHOR_*/GIT_COMMITTER_* injected per call from Actor; clean env
}

type gitService struct {
    store   Store        // sqlc metadata queries (uuid<->handle, owners, redirects)
    git     gitRunner    // injected
    root    string       // "/data/sites"
    locks   *lockRegistry
    cfg     Config       // zip limits, allowlist, mirror on/off, worktree dir
    clock   func() time.Time // injected for deterministic tests
}

func NewService(store Store, git gitRunner, cfg Config) *gitService { ... }
```

Representative command mapping (illustrative, not exhaustive):

| Method        | git invocation(s) |
|---------------|-------------------|
| CreateSite    | `init -b draft` → write files → `add -A` → `commit` (author from Actor) |
| WriteFile     | read tip (`rev-parse {branch}`), compare BaseSHA, write blob to worktree, `add <path>`, `commit -m` |
| ReadFile      | `cat-file -p {branch}:{path}` + `rev-parse {branch}` for the lock SHA |
| ListFiles     | `ls-tree [-r] --long {branch} {dir}` |
| Commit        | `rev-parse`, `commit -m` (already staged) |
| Publish (ff)  | `rev-parse`, `merge-base --is-ancestor`, `update-ref refs/heads/published {src}` |
| Publish (merge)| `checkout published`, `merge --no-ff -m ... {from}` (in a temp worktree) |
| GetDiff       | `diff --numstat` + `diff --unified` between refs |
| GetLog        | `log --format=... -n {limit} {branch}` |
| Rollback      | `read-tree {toSHA}` into index on branch, `commit -m "rollback to {short}"` (new commit) |
| Mirror push   | `push --force-with-lease origin {branch}` (best-effort, async-safe) |
| ResolveForServing | maintain per-(uuid,branch) worktree via `worktree add/prune`; refresh on commit |

**Worktrees for serving:** rather than re-`archive` on every request, `ResolveForServing` keeps a
read-only `git worktree` per active (uuid,branch) under `cfg.WorktreeDir`, refreshed (`reset
--hard {tip}`) on each successful commit/publish to that branch. `ServeTarget.Root` points there.
This keeps the data plane a plain static file server with no git in the request path.

**Why this is mockable / the DI story:**
- Callers (handlers, MCP tools) depend on `site.Service`. In their tests they inject a
  **`FakeService`** (in-memory map of sites→branches→files+SHA counter) — no git, no disk, fast,
  deterministic. The fake enforces the SAME contract (BaseSHA conflict, reserved handles, path
  validation) so handler tests exercise real error paths.
- `gitService`’s own logic (branch math, error mapping, lock ordering) is tested against a
  **fake `gitRunner`** that returns canned stdout/exit codes — no real `git` needed for most
  cases — plus a small set of **integration tests** that DO run real `git` in a temp dir to prove
  the command strings actually work.

```go
// FakeService implements Service in-memory for handler/MCP unit tests.
type FakeService struct {
    mu    sync.Mutex
    sites map[SiteID]*fakeSite // branches, files, monotonic commit log
    // Injection hooks to force error paths deterministically:
    FailNext map[string]error // method name -> error to return once
}
var _ Service = (*FakeService)(nil)
```

---

## 10. Error taxonomy

Idiomatic Go: **sentinel errors** for the categories callers branch on with `errors.Is`, plus
**typed errors** (`errors.As`) where structured detail is needed (conflict SHAs, validation
field, git stderr). Every returned error wraps a sentinel so the HTTP/MCP layer can map to a
status/code with a single `errors.Is` switch.

```go
package site

import "errors"

// Sentinels — the stable, switchable categories. (HTTP/MCP map these to codes.)
var (
    ErrNotFound         = errors.New("site: not found")          // 404
    ErrValidation       = errors.New("site: validation failed")  // 400
    ErrReservedHandle   = errors.New("site: reserved handle")    // 400/409
    ErrHandleTaken      = errors.New("site: handle already taken")// 409
    ErrConflict         = errors.New("site: stale base sha")     // 409 (optimistic lock)
    ErrPublishConflict  = errors.New("site: publish merge conflict") // 409
    ErrBranchExists     = errors.New("site: branch already exists")  // 409
    ErrNothingToCommit  = errors.New("site: nothing to commit")  // 409/204
    ErrForbidden        = errors.New("site: forbidden")          // 403 (ownership/scope)
    // Zip family:
    ErrZipSlip          = errors.New("site: zip path traversal")     // 400
    ErrZipTooLarge      = errors.New("site: zip too large")          // 413
    ErrZipTooManyFiles  = errors.New("site: zip too many files")     // 413
    ErrZipBadType       = errors.New("site: zip disallowed file type")// 415
    // git wrapper:
    ErrGit              = errors.New("site: git operation failed")   // 500
)

// ValidationError adds the offending field/reason. Is() ties it to ErrValidation.
type ValidationError struct {
    Field  string
    Reason string
}
func (e *ValidationError) Error() string { return "site: validation: " + e.Field + ": " + e.Reason }
func (e *ValidationError) Is(t error) bool { return t == ErrValidation }

// ConflictError — see §5. errors.Is(err, ErrConflict) true; errors.As gets the SHAs.
type ConflictError struct{ Branch BranchName; Expected, Actual string }
func (e *ConflictError) Error() string { return "site: stale base sha: expected " + e.Expected + " got " + e.Actual }
func (e *ConflictError) Is(t error) bool { return t == ErrConflict }

// PublishConflictError carries the conflicting paths.
type PublishConflictError struct{ Paths []string }
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
func (e *GitError) Unwrap() error  { return nil } // ctx.Canceled passed separately, see §8
```

**Mapping rule for callers (one place):**

```go
func statusFor(err error) int {
    switch {
    case err == nil:                          return 200
    case errors.Is(err, ErrNotFound):         return 404
    case errors.Is(err, ErrForbidden):        return 403
    case errors.Is(err, ErrConflict),
         errors.Is(err, ErrHandleTaken),
         errors.Is(err, ErrPublishConflict),
         errors.Is(err, ErrBranchExists):     return 409
    case errors.Is(err, ErrZipTooLarge),
         errors.Is(err, ErrZipTooManyFiles):  return 413
    case errors.Is(err, ErrZipBadType):       return 415
    case errors.Is(err, ErrValidation),
         errors.Is(err, ErrReservedHandle),
         errors.Is(err, ErrZipSlip):          return 400
    default:                                  return 500 // ErrGit + unknown
    }
}
```

---

## 11. Handle & path validation (referenced by several methods)

```go
const (
    HandleMinLen = 2
    HandleMaxLen = 63 // DNS label limit; also keeps {handle}--{branch} under 253 total
)
// handleRe: lowercase, must start+end alnum, internal hyphens allowed, no "--" (collides with
// the preview separator), no leading/trailing hyphen.
var handleRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

var reservedHandles = map[string]struct{}{
    "draft": {}, "preview": {}, "published": {}, "www": {}, "api": {}, "internal": {},
    "host": {}, "admin": {}, "app": {}, "static": {}, "assets": {}, "mcp": {},
}

// ValidateHandle: length, regex, NO "--" substring, not reserved. Uniqueness is checked
// against the Store inside CreateSite/RenameHandle (a DB unique constraint is the final guard).
func ValidateHandle(h Handle) error { ... } // ErrValidation / ErrReservedHandle

// validatePath: repo-relative only. Reject "" for files, leading "/", any ".." element,
// backslashes, NUL bytes, and non-allowlisted extensions. Uses filepath.IsLocal.
func validatePath(p string) error { ... } // *ValidationError
```

The `--` ban in handles is load-bearing: it guarantees `{handle}--{branch}` is unambiguously
splittable by `ResolveForServing`.

---

## 12. ResolveForServing parsing (Host + path fallback)

```
Host "expense-calc.hosting.example.com"        -> handle=expense-calc, branch=published
Host "expense-calc--draft.hosting.example.com" -> handle=expense-calc, branch=draft
Host "expense-calc--feature-x.hosting.example.com" -> handle=expense-calc, branch=feature-x
Host "expense-calc.localhost:8080"             -> handle=expense-calc, branch=published
Path "/host/expense-calc/index.html"           -> handle=expense-calc, branch=published
Path "/host/expense-calc--draft/app.js"        -> handle=expense-calc, branch=draft
```

- Strip the configured serving zone suffix (`.hosting.example.com`, `.localhost[:port]`, or the
  `/host/` prefix) — the suffix is **config**, not hardcoded.
- Split the remaining label on the **first** `--`: left = handle, right = branch
  (the handle can't contain `--`, so this is unambiguous). No `--` ⇒ branch = `published`.
- Resolve handle → uuid via the Store; old handles still resolve here for serving (serving
  follows redirects so live links don't break) — distinct from `GetSiteByHandle` which 404s old
  names for the control API. **(Decision to confirm — see gaps.)**
- Unknown handle/branch → `ErrNotFound`; malformed host/path → `ErrValidation`.

---

## 13. Test plan (table-driven Go unit tests proving the contract)

All tests use `testing` + table-driven subtests (`t.Run`). Two layers:

### A. Contract tests via `FakeService` (fast, no git)
File: `service_contract_test.go`. A single `func testContract(t *testing.T, newSvc func() Service)`
run against BOTH `FakeService` and a real `gitService` (with `t.TempDir()` repos), guaranteeing
the fake and the real impl are behaviorally identical.

| Test | Asserts |
|------|---------|
| `TestCreateSite_valid` | returns Site, draft branch exists, initial commit present |
| `TestCreateSite_reservedHandle` | `errors.Is(err, ErrReservedHandle)` for each reserved word |
| `TestCreateSite_invalidHandle` | uppercase / `--` / leading-hyphen / too-long → ErrValidation |
| `TestCreateSite_handleTaken` | second create same handle → ErrHandleTaken |
| `TestCreateSite_fromZip` | seeded files readable on draft |
| `TestWriteFile_happy` | tip advances; ReadFile returns new content + new SHA |
| `TestWriteFile_staleBaseSHA` | wrong BaseSHA → ErrConflict; `errors.As` gives Expected/Actual |
| `TestWriteFile_emptyBaseSHA` | "" → ErrValidation (not a force) |
| `TestWriteFile_pathTraversal` | `../x`, `/abs`, `a\\b` → ErrValidation |
| `TestWriteFile_badExtension` | `.php`,`.exe` → ErrValidation/ErrZipBadType analog |
| `TestWriteFile_lostUpdate_serialized` | two writers, same BaseSHA: first wins, second ErrConflict |
| `TestDeleteFile_happy` / `_stale` | mirror of WriteFile |
| `TestReadFile_notFound` | unknown path/branch/site → ErrNotFound |
| `TestImportZip_zipSlip` | entry `../evil` → ErrZipSlip, nothing committed |
| `TestImportZip_tooLarge` | declared size > cap → ErrZipTooLarge (no read) |
| `TestImportZip_tooManyFiles` | entries > cap → ErrZipTooManyFiles |
| `TestImportZip_zipBomb` | high-ratio entry / total > cap → ErrZipTooLarge |
| `TestImportZip_badType` | `.sh` entry → ErrZipBadType |
| `TestImportZip_happy` | tree replaced, one commit, files served |
| `TestPublish_firstPublish` | published created at draft tip, HasPublished=true |
| `TestPublish_fastForward` | published ancestor → ff, no new commit |
| `TestPublish_mergeCommit` | diverged published → merge commit created |
| `TestPublish_mergeConflict` | conflicting trees → ErrPublishConflict + Paths |
| `TestPublish_idempotent` | published==src → no-op, returns current CommitInfo |
| `TestRollback_createsNewCommit` | tree == toSHA's tree, history length +1 (no rewrite) |
| `TestRollback_stale` | wrong baseSHA → ErrConflict |
| `TestGetDiff_addModDelRename` | each FileDiff.Status correct, numstat matches |
| `TestGetLog_pagination` | limit + Before cursor returns correct slice, newest-first |
| `TestRenameHandle_redirectRecorded` | new handle resolves; old recorded; files unmoved |
| `TestRenameHandle_taken/reserved` | error paths |
| `TestListBranches_previewSubdomain` | published→"{handle}", draft→"{handle}--draft" |
| `TestResolveForServing_hostPublished` | bare host → published target |
| `TestResolveForServing_hostBranch` | `--draft` → draft target |
| `TestResolveForServing_pathFallback` | `/host/{handle}--draft/...` → same target |
| `TestResolveForServing_unknownHandle` | ErrNotFound |
| `TestResolveForServing_malformed` | empty/garbage host → ErrValidation |
| `TestConcurrentWrites_sameSite` | N goroutines, exactly one wins per BaseSHA generation |

### B. `gitService` internals via fake `gitRunner`
File: `git_service_test.go`. Inject a `gitRunner` returning canned stdout/exit codes; assert:
- correct `git` argv assembled per method (golden-arg assertions),
- non-zero exit → wrapped `*GitError` with `errors.Is(_, ErrGit)`,
- ctx cancellation → `errors.Is(_, context.Canceled)`,
- Actor → `GIT_AUTHOR_NAME/EMAIL` env present,
- BaseSHA compare uses `rev-parse {branch}` output.

### C. Real-git integration (build-tagged `//go:build integration`)
A handful that run actual `git` in `t.TempDir()` to prove the command strings: create→write→
commit→publish→diff→log→rollback round-trip, and ZipSlip with a real crafted zip. Kept out of
the default `go test ./...` fast path; run in CI with `-tags=integration`.

---

## 14. Open questions / gaps (考慮漏れ surfaced)

1. **Old-handle resolution asymmetry.** §12 proposes serving follows redirects (live links don't
   break) while the control API 404s old handles. Confirm this split, and decide redirect TTL —
   permanent, or expire after a grace period so old handles can be reclaimed?
2. **Cross-replica writes.** v1 is single-writer (in-process mutex). If HA is wanted, choose the
   distributed lock now (Postgres advisory lock vs git ref CAS) so the `Service` impl reserves the
   seam. BaseSHA protects correctness but not against two simultaneous in-flight commits racing
   the worktree on shared `/data`.
3. **ImportZip semantics on an existing branch: replace vs merge.** v1 = replace. If non-engineers
   expect "upload just adds my new file", we need a merge mode and a UI affordance. Decide default.
4. **File-level vs branch-level optimistic lock.** Branch-level (v1) serializes disjoint-file
   edits and yields more conflicts. Do we want a future 3-way merge for disjoint paths, and is the
   extra complexity worth it for the "small multi-user" target?
5. **Binary file editing/serving.** `FileContent.IsBinary` is detected, but the Monaco path is
   text-only. Define behavior: read returns base64? write of binary via editor disallowed? upload
   only via Zip?
6. **`GetDiff` against working tree (`to == ""`).** Since WriteFile commits atomically, is there
   ever an uncommitted working tree to diff? If batched `Commit` (staged-but-not-committed) is
   real, we need a staging area model; if not, drop the working-tree diff case.
7. **`Commit` vs `WriteFile` overlap.** With WriteFile committing per-call, when is multi-file
   `Commit` actually used (Monaco "save all"? MCP batch?)? Define the staging contract or remove
   `Commit` to keep the surface minimal.
8. **Rollback target validation.** Should `toSHA` be required to be reachable from the branch
   (ancestor), or can you roll forward/sideways to any commit in the repo (e.g. a feature branch
   tip)? Pick a reachability rule.
9. **Mirror-push failure surfacing.** Best-effort push returns a warning — through what channel?
   (Response field `warnings []string`? a notifications table? logs only?) Define so the UI can
   show "saved locally, GitHub sync pending".
10. **DeleteSite hard vs soft.** Spec implies hard delete; but accidental deletion of an
    AGPL-served tool is unrecoverable. Recommend soft-delete (mark + retain repo N days) — confirm.
11. **GitHub webhook → PullPublished.** Folded into Publish for v1 (§6). Confirm it stays an
    HTTP-layer concern and doesn't need its own interface method (clean) vs. an explicit
    `SyncFromRemote(ctx, id, branch)` method (more testable in isolation).
12. **Worktree disk cost & eviction.** Per-(uuid,branch) serving worktrees double on-disk size and
    grow with branch count. Need an eviction/LRU policy and a cap, or switch to `git archive` +
    in-memory cache for cold branches. Decide the trade-off.
13. **Per-project MCP token scope enforcement point.** The token→site scope check lives above
    `Service` (HTTP/MCP middleware) or inside it via `ownerID`/`ErrForbidden`? §4 leaves
    `ListSites` enforcement "to the caller" — pin down whether `Service` is authz-aware at all, to
    avoid a confused-deputy gap where an MCP token for site A can `WriteFile` to site B.
