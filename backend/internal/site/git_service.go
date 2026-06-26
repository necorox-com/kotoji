package site

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Config bounds the gitService behavior (zip limits + allowlist live here; the
// upstream internal/config maps env to these). Defaults are applied by
// withDefaults so a zero Config is usable in tests.
type Config struct {
	Root        string // base dir for site repos: "/data/sites"
	GitBin      string // git binary path; "" => "git"
	MirrorOn    bool   // whether mirror-push is attempted at all (best-effort regardless)
	ListLimit   int    // default LogOptions.Limit when 0
	MaxLogLimit int    // hard cap for LogOptions.Limit
	// GitHubToken is the instance-level PAT/app token used to AUTHENTICATE mirror
	// push/fetch against github.com. It is injected per git invocation via an HTTP
	// extra-header (NEVER written to .git/config or the remote URL on disk) and is
	// scrubbed from every command output/log (see git_auth.go). Empty => mirroring
	// is unauthenticated (anonymous push to a public repo fails; we no-op with a
	// warning rather than hang on a credential prompt — scrubbedEnv disables prompts).
	GitHubToken string
	// GitHubUser is the basic-auth username paired with the token. GitHub accepts
	// any non-empty username with a token as the password; "x-access-token" is the
	// canonical value for app/installation tokens, so we default to it.
	GitHubUser string
	// MirrorToken, when non-nil, is the DYNAMIC source of the mirror credential,
	// resolved PER git invocation (so a runtime change via the admin GUI takes
	// effect without a restart). It returns the effective (token, user); an empty
	// token means "unauthenticated" (mirror push to a private repo will fail, but
	// we never block on a credential prompt). When nil the static GitHubToken /
	// GitHubUser fields are used instead — preserving env-only deployments. The
	// ctx is the in-flight request's so DB reads honor cancellation/deadlines.
	MirrorToken func(ctx context.Context) (token, user string)
	// MirrorEnabled, when non-nil, is the DYNAMIC "is mirroring on" gate resolved
	// per write (DB-overrides-env via the composition root) so the admin can turn
	// mirroring on/off at runtime. When nil the static MirrorOn flag is used (env-
	// only deployments / tests). bestEffortMirror consults it before pushing.
	MirrorEnabled func(ctx context.Context) bool
	Zip           ZipConfig
	// SiteQuotaBytes is the per-site on-disk hard cap (repo + objects + worktree)
	// enforced by ImportZip/WriteFile BEFORE a write is committed (CANONICAL §8.4 /
	// KOTOJI_SITE_QUOTA_BYTES). A value <= 0 disables the check (unbounded). Unlike
	// the per-import zip caps, this bounds CUMULATIVE growth across many imports.
	SiteQuotaBytes int64
	// GitOpTimeout (T1) is the server-side hard deadline applied to EVERY git child
	// process, independent of the inbound request context. A slow/hung network
	// fetch/push (or any wedged git invocation) is bounded by this so a request can
	// never block a worker indefinitely. A value <= 0 falls back to
	// defaultGitOpTimeout via withDefaults; the deadline is the MIN of this and any
	// shorter request deadline, so callers that pass a tighter ctx still win.
	GitOpTimeout time.Duration
}

// ZipConfig holds the ImportZip security limits (CANONICAL / site-service.md §7).
type ZipConfig struct {
	MaxUploadBytes       int64    // declared compressed size gate
	MaxUncompressedBytes int64    // total expanded gate (zip-bomb)
	MaxEntries           int      // entry-count gate
	MaxEntryUncompressed int64    // per-entry expanded gate
	MaxCompressionRatio  int      // per-entry uncompressed/compressed ratio gate
	AllowedExt           []string // optional override of the upload allowlist
}

// Default limits (CANONICAL / site-service.md §7). Exported so callers and tests
// share the exact numbers.
const (
	DefaultMaxZipBytes          = 50 << 20  // 50 MiB compressed (declared)
	DefaultMaxUncompressedBytes = 200 << 20 // 200 MiB total expanded
	DefaultMaxZipEntries        = 2000
	DefaultMaxEntryUncompressed = 50 << 20 // 50 MiB single file
	DefaultMaxCompressionRatio  = 100      // uncompressed/compressed per entry
	defaultLogLimit             = 50
	defaultMaxLogLimit          = 500
	// defaultGitOpTimeout (T1) bounds any single git child process server-side so a
	// hung network fetch/push cannot block a worker forever. 120s is generous for a
	// large clone/fetch yet finite. Operators can raise/lower it via Config.
	defaultGitOpTimeout = 120 * time.Second
	// emptyTreeSHA is git's well-known empty tree object hash (stable across every
	// repo). Used as a diff endpoint when the target ref does not exist yet (e.g.
	// the `published` branch before the first publish), so the diff renders all of
	// the source branch's content as additions rather than failing as not-found.
	emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// withDefaults fills unset Config fields so a zero/partial Config is valid.
func (c Config) withDefaults() Config {
	if c.GitBin == "" {
		c.GitBin = "git"
	}
	if c.ListLimit == 0 {
		c.ListLimit = defaultLogLimit
	}
	if c.MaxLogLimit == 0 {
		c.MaxLogLimit = defaultMaxLogLimit
	}
	if c.Zip.MaxUploadBytes == 0 {
		c.Zip.MaxUploadBytes = DefaultMaxZipBytes
	}
	if c.Zip.MaxUncompressedBytes == 0 {
		c.Zip.MaxUncompressedBytes = DefaultMaxUncompressedBytes
	}
	if c.Zip.MaxEntries == 0 {
		c.Zip.MaxEntries = DefaultMaxZipEntries
	}
	if c.Zip.MaxEntryUncompressed == 0 {
		c.Zip.MaxEntryUncompressed = DefaultMaxEntryUncompressed
	}
	if c.Zip.MaxCompressionRatio == 0 {
		c.Zip.MaxCompressionRatio = DefaultMaxCompressionRatio
	}
	if c.GitOpTimeout <= 0 {
		c.GitOpTimeout = defaultGitOpTimeout
	}
	return c
}

// gitService is the production Service: it shells out to the git CLI via a
// gitRunner and persists metadata via a Store. Per-site mutations are serialized
// by the lock registry + an OS flock (lock.go). It satisfies site.Service.
type gitService struct {
	store Store
	git   gitRunner
	cfg   Config
	locks *lockRegistry
	clock func() time.Time // injected for deterministic commit timestamps in tests
}

// compile-time guarantee gitService implements the frozen interface.
var _ Service = (*gitService)(nil)

// NewService builds the production gitService. store + git are injected
// collaborators (DI); cfg.Root must be a writable base directory.
func NewService(store Store, git gitRunner, cfg Config) *gitService {
	return &gitService{
		store: store,
		git:   git,
		cfg:   cfg.withDefaults(),
		locks: newLockRegistry(),
		clock: time.Now,
	}
}

// NewServiceWithClock is NewService with an injectable clock (tests).
func NewServiceWithClock(store Store, git gitRunner, cfg Config, clock func() time.Time) *gitService {
	svc := NewService(store, git, cfg)
	if clock != nil {
		svc.clock = clock
	}
	return svc
}

// ---- path helpers ----

// repoDir is the on-disk repository directory for a site (a non-bare working
// repo; the .git lives inside). CANONICAL: /data/sites/{uuid}.
func (g *gitService) repoDir(id uuid.UUID) string {
	return filepath.Join(g.cfg.Root, id.String())
}

// gitDir is the .git directory (where the flock + refs live).
func (g *gitService) gitDir(id uuid.UUID) string {
	return filepath.Join(g.repoDir(id), ".git")
}

// servedDir is the materialized read-only served worktree root for a branch
// (CANONICAL §1 ServedTree: /data/sites/{uuid}/served/{branch}).
func (g *gitService) servedDir(id uuid.UUID, branch BranchName) string {
	return filepath.Join(g.repoDir(id), "served", string(branch))
}

// ---- small git wrappers (each returns trimmed stdout or a wrapped error) ----

// run executes a read/plumbing git command in the site repo.
func (g *gitService) run(ctx context.Context, id uuid.UUID, args ...string) (string, error) {
	out, err := g.git.Run(ctx, g.repoDir(id), nil, args...)
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// runEnv executes a commit-producing git command with author/committer identity
// injected from the Actor + a deterministic timestamp. It uses the envRunner
// seam when the runner supports it (execRunner does); otherwise it falls back to
// the plain runner (the fake runner records args without env).
func (g *gitService) runEnv(ctx context.Context, id uuid.UUID, a Actor, stdin []byte, args ...string) (string, error) {
	when := g.clock().UTC().Format(time.RFC3339)
	env := authorEnv(a, when)
	if er, ok := g.git.(envRunner); ok {
		out, err := er.RunEnv(ctx, g.repoDir(id), env, stdin, args...)
		return string(out), err
	}
	out, err := g.git.Run(ctx, g.repoDir(id), stdin, args...)
	return string(out), err
}

// revParse resolves a ref (branch/SHA) to a full commit SHA. A non-existent ref
// is mapped to ErrNotFound (git exits non-zero with "unknown revision"). This is
// the central laundering chokepoint: callers funnel ATTACKER-CONTROLLED ref text
// (pagination cursors, diff endpoints, rollback targets) through here to obtain a
// resolved hex SHA. "--end-of-options" forces git to treat ref as a revision and
// never an option, so a leading-dash ref (e.g. "-foo", "--output=x") cannot inject
// a git option even before it resolves (H1 hardening / defense in depth).
func (g *gitService) revParse(ctx context.Context, id uuid.UUID, ref string) (string, error) {
	out, err := g.run(ctx, id, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	sha := strings.TrimSpace(out)
	if err != nil || sha == "" {
		return "", ErrNotFound
	}
	return sha, nil
}

// branchExists reports whether a local branch ref exists.
func (g *gitService) branchExists(ctx context.Context, id uuid.UUID, branch BranchName) (bool, error) {
	_, err := g.run(ctx, id, "show-ref", "--verify", "--quiet", "refs/heads/"+string(branch))
	if err == nil {
		return true, nil
	}
	// show-ref exits 1 when the ref is absent; that is a clean "no", not ErrGit.
	var ge *GitError
	if errors.As(err, &ge) && ge.ExitCode == 1 {
		return false, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	return false, err
}

// ---- lock helpers ----

// withWriteLock runs fn while holding the per-site write lock (in-process RWMutex
// + cross-process flock). The flock is best-effort: if the .git dir does not yet
// exist (pre-create), only the in-process lock is taken.
func (g *gitService) withWriteLock(id uuid.UUID, fn func() error) error {
	m := g.locks.get(id)
	m.Lock()
	defer m.Unlock()

	// Take the OS advisory lock only when the repo already exists; CreateSite
	// builds the dir itself and holds just the in-process lock during init.
	if _, err := os.Stat(g.gitDir(id)); err == nil {
		fl, lerr := acquireFileLock(g.gitDir(id))
		if lerr != nil {
			return fmt.Errorf("%w: %v", ErrGit, lerr)
		}
		defer fl.release()
	}
	return fn()
}

// withReadLock runs fn while holding the per-site read lock.
func (g *gitService) withReadLock(id uuid.UUID, fn func() error) error {
	m := g.locks.get(id)
	m.RLock()
	defer m.RUnlock()
	return fn()
}

// ---- Site lifecycle ----

// CreateSite validates+reserves the handle, inits the repo with a first commit on
// draft (placeholder index.html or zip seed), and persists metadata + owner row.
// Atomic-ish: the metadata row is written ONLY after git init succeeds; on a
// metadata failure the repo dir is removed so a retry is clean (CANONICAL §1).
func (g *gitService) CreateSite(ctx context.Context, in CreateSiteInput) (Site, error) {
	if err := ValidateHandle(in.Handle); err != nil {
		return Site{}, err
	}
	// S1: a tenant-supplied mirror repo is validated up front (before it is stored
	// or fed to `git remote add` in initRepo) against the strict GitHub-only
	// allowlist, so a file:// / internal-IP / non-GitHub remote can never be
	// persisted at create time. Empty means "no mirror" and is allowed.
	if in.GitHubRepo != "" {
		if err := validateRemoteURL(in.GitHubRepo); err != nil {
			return Site{}, err
		}
	}
	visibility := in.Visibility
	if visibility == "" {
		visibility = "private"
	}
	publishMode := in.PublishMode
	if publishMode == "" {
		publishMode = "direct"
	}

	// Collision check across live handles, redirects, and reserved words. The DB
	// UNIQUE is the final guard; this gives a friendly error first.
	taken, err := g.store.HandleIsTaken(ctx, strings.ToLower(string(in.Handle)))
	if err != nil {
		return Site{}, fmt.Errorf("%w: handle check: %v", ErrGit, err)
	}
	if taken {
		return Site{}, ErrHandleTaken
	}

	// We need the UUID up front to key the repo path, but the DB generates it.
	// Generate client-side and let CreateSiteWithOwner use it would require a DDL
	// change; instead we persist metadata FIRST (to obtain the UUID), then init the
	// repo at that path. If git init fails we soft-roll-back by deleting the row.
	var repoID uuid.UUID
	var rec SiteRecord
	rec, err = g.store.CreateSiteWithOwner(ctx, StoreCreateSite{
		Handle:      strings.ToLower(string(in.Handle)),
		OwnerID:     in.OwnerID,
		Visibility:  visibility,
		PublishMode: publishMode,
		GitHubRepo:  emptyToNil(in.GitHubRepo),
		WebRoot:     "",
		Description: in.Description,
	})
	if err != nil {
		// A unique-violation here means a race lost the handle; map to taken.
		if isUniqueViolation(err) {
			return Site{}, ErrHandleTaken
		}
		return Site{}, fmt.Errorf("%w: create site row: %v", ErrGit, err)
	}
	repoID = rec.ID

	// From here, any failure must remove the repo dir so disk/metadata stay
	// consistent. The metadata row deletion on failure keeps the handle free.
	if err := g.initRepo(ctx, repoID, in); err != nil {
		_ = os.RemoveAll(g.repoDir(repoID))
		_ = g.store.SoftDeleteSite(ctx, repoID) // best-effort: frees nothing on disk but marks the row
		return Site{}, err
	}

	return rec.toSite(), nil
}

// initRepo creates the bare-on-disk working repo, seeds initial content, and
// makes the first commit on draft.
func (g *gitService) initRepo(ctx context.Context, id uuid.UUID, in CreateSiteInput) error {
	dir := g.repoDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%w: mkdir repo: %v", ErrGit, err)
	}
	// init -b draft creates the working repo with draft as the initial branch.
	if _, err := g.run(ctx, id, "init", "-b", string(BranchDraft)); err != nil {
		return wrapGit(err)
	}

	// Seed content: from the zip if provided (guards applied), else a placeholder
	// index.html so the site serves something immediately.
	if in.Zip != nil {
		if err := g.extractZipToWorktree(dir, *in.Zip); err != nil {
			return err
		}
	} else {
		placeholder := []byte(defaultIndexHTML(string(in.Handle)))
		if err := os.WriteFile(filepath.Join(dir, "index.html"), placeholder, 0o644); err != nil {
			return fmt.Errorf("%w: seed index: %v", ErrGit, err)
		}
	}

	if _, err := g.run(ctx, id, "add", "-A"); err != nil {
		return wrapGit(err)
	}
	msg := "Initial commit"
	if in.Zip != nil && in.Zip.Filename != "" {
		msg = "Import " + in.Zip.Filename
	}
	if _, err := g.runEnv(ctx, id, in.Actor, nil, "commit", "-m", msg, viaTrailer(in.Actor)); err != nil {
		return wrapGit(err)
	}
	// Configure mirror remote if requested (best-effort; remote misconfig must not
	// fail create — the row is already valid).
	if in.GitHubRepo != "" {
		_, _ = g.run(ctx, id, "remote", "add", "origin", normalizeRemoteURL(in.GitHubRepo))
	}
	return nil
}

func (g *gitService) GetSite(ctx context.Context, id uuid.UUID) (Site, error) {
	rec, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return Site{}, fmt.Errorf("%w: get site: %v", ErrGit, err)
	}
	if !found || rec.DeletedAt != nil {
		return Site{}, ErrNotFound
	}
	return rec.toSite(), nil
}

func (g *gitService) GetSiteByHandle(ctx context.Context, h Handle) (Site, error) {
	rec, found, err := g.store.GetSiteByHandle(ctx, strings.ToLower(string(h)))
	if err != nil {
		return Site{}, fmt.Errorf("%w: get site by handle: %v", ErrGit, err)
	}
	if !found {
		return Site{}, ErrNotFound
	}
	return rec.toSite(), nil
}

func (g *gitService) ListSites(ctx context.Context, ownerID uuid.UUID) ([]Site, error) {
	recs, err := g.store.ListSitesForUser(ctx, ownerID, 500, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: list sites: %v", ErrGit, err)
	}
	out := make([]Site, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.toSite())
	}
	return out, nil
}

func (g *gitService) RenameHandle(ctx context.Context, id uuid.UUID, newHandle Handle) (Site, error) {
	if err := ValidateHandle(newHandle); err != nil {
		return Site{}, err
	}
	rec, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return Site{}, fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found || rec.DeletedAt != nil {
		return Site{}, ErrNotFound
	}
	newLower := strings.ToLower(string(newHandle))
	if newLower == rec.Handle {
		// No-op rename: return the current site unchanged.
		return rec.toSite(), nil
	}
	taken, err := g.store.HandleIsTaken(ctx, newLower)
	if err != nil {
		return Site{}, fmt.Errorf("%w: handle check: %v", ErrGit, err)
	}
	// A redirect of THIS same site (rename-back) is not "taken" for us; the Store's
	// rename routine clears it. Re-check: HandleIsTaken counts redirects globally,
	// so distinguish a self-redirect.
	if taken && !g.isOwnRedirect(ctx, id, newLower) {
		return Site{}, ErrHandleTaken
	}
	if err := g.store.RenameHandleWithRedirect(ctx, id, rec.Handle, newLower); err != nil {
		if isUniqueViolation(err) {
			return Site{}, ErrHandleTaken
		}
		return Site{}, fmt.Errorf("%w: rename: %v", ErrGit, err)
	}
	rec.Handle = newLower
	return rec.toSite(), nil
}

// isOwnRedirect reports whether newLower is a redirect pointing back at THIS
// site (the rename-back case), which RenameHandleWithRedirect handles cleanly.
func (g *gitService) isOwnRedirect(ctx context.Context, id uuid.UUID, newLower string) bool {
	rec, found, err := g.store.GetSiteByRedirect(ctx, newLower)
	return err == nil && found && rec.ID == id
}

func (g *gitService) DeleteSite(ctx context.Context, id uuid.UUID, actor Actor) error {
	rec, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found || rec.DeletedAt != nil {
		return ErrNotFound
	}
	// Soft delete: stamp deleted_at. The on-disk repo is retained for the 30-day
	// grace; the reaper bundles+reclaims later (decision #3).
	if err := g.store.SoftDeleteSite(ctx, id); err != nil {
		return fmt.Errorf("%w: soft delete: %v", ErrGit, err)
	}
	return nil
}

// ---- Branches ----

func (g *gitService) ListBranches(ctx context.Context, id uuid.UUID) ([]Branch, error) {
	rec, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found || rec.DeletedAt != nil {
		return nil, ErrNotFound
	}
	var branches []Branch
	err = g.withReadLock(id, func() error {
		// for-each-ref gives the branch name + tip SHA in one cheap call.
		out, e := g.run(ctx, id, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads/")
		if e != nil {
			return wrapGit(e)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			name := BranchName(parts[0])
			ci, e := g.commitInfo(ctx, id, parts[1])
			if e != nil {
				return e
			}
			branches = append(branches, Branch{
				Name:             name,
				HeadSHA:          parts[1],
				IsPublished:      name == BranchPublished,
				LastCommit:       ci,
				PreviewSubdomain: previewSubdomain(Handle(rec.Handle), name),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return branches, nil
}

func (g *gitService) CreateBranch(ctx context.Context, id uuid.UUID, name BranchName, from string) (Branch, error) {
	if err := validateBranchName(name); err != nil {
		return Branch{}, err
	}
	// New branches may not collide with the reserved logical names the service
	// manages itself.
	if name == BranchPublished || name == BranchDraft {
		return Branch{}, &ValidationError{Field: "branch", Reason: "reserved branch name"}
	}
	rec, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return Branch{}, fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found || rec.DeletedAt != nil {
		return Branch{}, ErrNotFound
	}
	if from == "" {
		from = string(BranchDraft)
	}
	var br Branch
	err = g.withWriteLock(id, func() error {
		exists, e := g.branchExists(ctx, id, name)
		if e != nil {
			return wrapGit(e)
		}
		if exists {
			return ErrBranchExists
		}
		fromSHA, e := g.revParse(ctx, id, from)
		if e != nil {
			return ErrNotFound
		}
		if _, e := g.run(ctx, id, "branch", string(name), fromSHA); e != nil {
			return wrapGit(e)
		}
		ci, e := g.commitInfo(ctx, id, fromSHA)
		if e != nil {
			return e
		}
		br = Branch{
			Name:             name,
			HeadSHA:          fromSHA,
			IsPublished:      false,
			LastCommit:       ci,
			PreviewSubdomain: previewSubdomain(Handle(rec.Handle), name),
		}
		return nil
	})
	if err != nil {
		return Branch{}, err
	}
	return br, nil
}

func (g *gitService) DeleteBranch(ctx context.Context, id uuid.UUID, name BranchName) error {
	if name == BranchPublished || name == BranchDraft {
		return &ValidationError{Field: "branch", Reason: "cannot delete the published or draft branch"}
	}
	_, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found {
		return ErrNotFound
	}
	return g.withWriteLock(id, func() error {
		exists, e := g.branchExists(ctx, id, name)
		if e != nil {
			return wrapGit(e)
		}
		if !exists {
			return ErrNotFound
		}
		if _, e := g.run(ctx, id, "branch", "-D", string(name)); e != nil {
			return wrapGit(e)
		}
		// Drop any materialized served worktree for the deleted branch.
		_ = os.RemoveAll(g.servedDir(id, name))
		return nil
	})
}

// ---- Files (read) ----

func (g *gitService) ListFiles(ctx context.Context, in ListFilesInput) ([]FileEntry, ResolvedRef, error) {
	if err := validateDir(in.Dir); err != nil {
		return nil, ResolvedRef{}, err
	}
	var entries []FileEntry
	var resolved ResolvedRef
	err := g.withReadLock(in.SiteID, func() error {
		ref := in.Ref
		if ref == "" {
			ref = string(in.Branch)
		}
		sha, e := g.revParse(ctx, in.SiteID, ref)
		if e != nil {
			return ErrNotFound
		}
		resolved = ResolvedRef{SHA: sha}

		args := []string{"ls-tree", "--long"}
		if in.Recursive {
			args = append(args, "-r")
		}
		// sha is a laundered (revParse'd) commit SHA — never option text. The "--"
		// separator guarantees the dir pathspec that follows can never be parsed as a
		// git option even if validateDir's leading-dash guard were ever bypassed
		// (defense in depth, consistent with GetDiff/GetLog).
		args = append(args, sha, "--")
		if in.Dir != "" {
			// ls-tree expects the dir with a trailing slash to list its contents.
			args = append(args, strings.TrimSuffix(in.Dir, "/")+"/")
		}
		out, e := g.run(ctx, in.SiteID, args...)
		if e != nil {
			return wrapGit(e)
		}
		entries = parseLsTree(out)
		return nil
	})
	if err != nil {
		return nil, ResolvedRef{}, err
	}
	return entries, resolved, nil
}

func (g *gitService) ReadFile(ctx context.Context, id uuid.UUID, branch BranchName, ref, path string) (FileContent, error) {
	if err := validateReadPath(path); err != nil {
		return FileContent{}, err
	}
	var fc FileContent
	err := g.withReadLock(id, func() error {
		resolveRef := ref
		if resolveRef == "" {
			resolveRef = string(branch)
		}
		sha, e := g.revParse(ctx, id, resolveRef)
		if e != nil {
			return ErrNotFound
		}
		spec := sha + ":" + path
		// blob hash + size via cat-file -s/--batch is two calls; keep it simple and
		// read content then derive size/binary from it. Missing blob => ErrNotFound.
		content, e := g.git.Run(ctx, g.repoDir(id), nil, "cat-file", "-p", spec)
		if e != nil {
			return ErrNotFound
		}
		blobSHA, e := g.run(ctx, id, "rev-parse", "--verify", "--quiet", spec)
		if e != nil {
			blobSHA = ""
		}
		fc = FileContent{
			Path:     path,
			Content:  content,
			SHA:      sha,
			BlobSHA:  strings.TrimSpace(blobSHA),
			Size:     int64(len(content)),
			IsBinary: isBinaryContent(content),
		}
		return nil
	})
	if err != nil {
		return FileContent{}, err
	}
	return fc, nil
}

// ---- Files (write) ----

func (g *gitService) WriteFile(ctx context.Context, in WriteFileInput) (CommitInfo, error) {
	if err := refuseProtectedBranch(in.Branch); err != nil {
		return CommitInfo{}, err
	}
	// MCP writes use the stricter text-first allowlist; others use the full upload
	// allowlist.
	var pathErr error
	if in.Actor.Via == SourceMCP {
		pathErr = validateMCPWritePath(in.Path)
	} else {
		pathErr = validatePath(in.Path)
	}
	if pathErr != nil {
		return CommitInfo{}, pathErr
	}
	if in.BaseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required (never treated as force)"}
	}

	var ci CommitInfo
	err := g.withWriteLock(in.SiteID, func() error {
		if _, e := g.requireSite(ctx, in.SiteID); e != nil {
			return e
		}
		if e := g.checkBaseSHA(ctx, in.SiteID, in.Branch, in.BaseSHA, []string{in.Path}); e != nil {
			return e
		}
		// Write the blob to the worktree and stage it. Operating on the live
		// worktree is safe because the branch is checked out lazily below.
		if e := g.checkoutBranch(ctx, in.SiteID, in.Branch); e != nil {
			return e
		}
		// M5 quota gate (BEFORE the write): the current on-disk repo size plus the
		// incoming blob must stay within SiteQuotaBytes. Charging the full content
		// length is a conservative upper bound (an overwrite of an existing path may
		// add less, but never more).
		if e := g.enforceQuota(in.SiteID, int64(len(in.Content))); e != nil {
			return e
		}
		full := filepath.Join(g.repoDir(in.SiteID), filepath.FromSlash(in.Path))
		if e := os.MkdirAll(filepath.Dir(full), 0o755); e != nil {
			return fmt.Errorf("%w: mkdir: %v", ErrGit, e)
		}
		if e := os.WriteFile(full, in.Content, 0o644); e != nil {
			return fmt.Errorf("%w: write blob: %v", ErrGit, e)
		}
		if _, e := g.run(ctx, in.SiteID, "add", "--", in.Path); e != nil {
			return wrapGit(e)
		}
		msg := in.Message
		if msg == "" {
			msg = "Update " + in.Path
		}
		newCI, e := g.commitStaged(ctx, in.SiteID, in.Branch, in.Actor, msg)
		if e != nil {
			return e
		}
		ci = newCI
		return nil
	})
	if err != nil {
		return CommitInfo{}, err
	}
	g.refreshServed(ctx, in.SiteID, in.Branch)
	g.bestEffortMirror(ctx, in.SiteID, in.Branch)
	return ci, nil
}

func (g *gitService) DeleteFile(ctx context.Context, id uuid.UUID, branch BranchName, path, baseSHA string, actor Actor) (CommitInfo, error) {
	if err := refuseProtectedBranch(branch); err != nil {
		return CommitInfo{}, err
	}
	if err := validateReadPath(path); err != nil {
		return CommitInfo{}, err
	}
	if baseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required (never treated as force)"}
	}
	var ci CommitInfo
	err := g.withWriteLock(id, func() error {
		if _, e := g.requireSite(ctx, id); e != nil {
			return e
		}
		if e := g.checkBaseSHA(ctx, id, branch, baseSHA, []string{path}); e != nil {
			return e
		}
		if e := g.checkoutBranch(ctx, id, branch); e != nil {
			return e
		}
		// git rm fails if the path is absent; treat that as ErrNotFound.
		if _, e := g.run(ctx, id, "rm", "--", path); e != nil {
			return ErrNotFound
		}
		newCI, e := g.commitStaged(ctx, id, branch, actor, "Delete "+path)
		if e != nil {
			return e
		}
		ci = newCI
		return nil
	})
	if err != nil {
		return CommitInfo{}, err
	}
	g.refreshServed(ctx, id, branch)
	g.bestEffortMirror(ctx, id, branch)
	return ci, nil
}

func (g *gitService) Commit(ctx context.Context, in CommitInput) (CommitInfo, error) {
	if err := refuseProtectedBranch(in.Branch); err != nil {
		return CommitInfo{}, err
	}
	if in.BaseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required (never treated as force)"}
	}
	var ci CommitInfo
	err := g.withWriteLock(in.SiteID, func() error {
		if _, e := g.requireSite(ctx, in.SiteID); e != nil {
			return e
		}
		if e := g.checkBaseSHA(ctx, in.SiteID, in.Branch, in.BaseSHA, nil); e != nil {
			return e
		}
		if e := g.checkoutBranch(ctx, in.SiteID, in.Branch); e != nil {
			return e
		}
		// Nothing staged AND nothing changed in the worktree => ErrNothingToCommit.
		status, e := g.run(ctx, in.SiteID, "status", "--porcelain")
		if e != nil {
			return wrapGit(e)
		}
		if strings.TrimSpace(status) == "" {
			return ErrNothingToCommit
		}
		// Stage any pending worktree changes for the batch "Save" verb.
		if _, e := g.run(ctx, in.SiteID, "add", "-A"); e != nil {
			return wrapGit(e)
		}
		msg := in.Message
		if msg == "" {
			msg = "Save"
		}
		newCI, e := g.commitStaged(ctx, in.SiteID, in.Branch, in.Actor, msg)
		if e != nil {
			return e
		}
		ci = newCI
		return nil
	})
	if err != nil {
		return CommitInfo{}, err
	}
	g.refreshServed(ctx, in.SiteID, in.Branch)
	g.bestEffortMirror(ctx, in.SiteID, in.Branch)
	return ci, nil
}

// ---- Publish ----

func (g *gitService) Publish(ctx context.Context, in PublishInput) (CommitInfo, error) {
	from := in.From
	if from == "" {
		from = BranchDraft
	}
	if from == BranchPublished {
		return CommitInfo{}, &ValidationError{Field: "from", Reason: "cannot publish from the published branch"}
	}
	if in.BaseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required (tip of the source branch)"}
	}
	var ci CommitInfo
	err := g.withWriteLock(in.SiteID, func() error {
		if _, e := g.requireSite(ctx, in.SiteID); e != nil {
			return e
		}
		srcTip, e := g.revParse(ctx, in.SiteID, string(from))
		if e != nil {
			return ErrNotFound
		}
		// Optimistic lock on the SOURCE tip: refuse to publish a stale snapshot.
		if srcTip != in.BaseSHA {
			return &ConflictError{Branch: from, Expected: in.BaseSHA, Actual: srcTip}
		}

		pubExists, e := g.branchExists(ctx, in.SiteID, BranchPublished)
		if e != nil {
			return wrapGit(e)
		}
		if !pubExists {
			// First publish: create published at the source tip.
			if _, e := g.run(ctx, in.SiteID, "update-ref", "refs/heads/"+string(BranchPublished), srcTip); e != nil {
				return wrapGit(e)
			}
			ci, e = g.commitInfo(ctx, in.SiteID, srcTip)
			if e != nil {
				return e
			}
			return g.finishPublish(ctx, in.SiteID, srcTip)
		}

		pubTip, e := g.revParse(ctx, in.SiteID, string(BranchPublished))
		if e != nil {
			return ErrNotFound
		}
		// Idempotent: published already equals the source tip.
		if pubTip == srcTip {
			ci, e = g.commitInfo(ctx, in.SiteID, pubTip)
			if e != nil {
				return e
			}
			return nil
		}
		// Fast-forward when published is an ancestor of the source tip.
		ffErr := g.runErr(ctx, in.SiteID, "merge-base", "--is-ancestor", string(BranchPublished), srcTip)
		if ffErr == nil {
			if _, e := g.run(ctx, in.SiteID, "update-ref", "refs/heads/"+string(BranchPublished), srcTip); e != nil {
				return wrapGit(e)
			}
			ci, e = g.commitInfo(ctx, in.SiteID, srcTip)
			if e != nil {
				return e
			}
			return g.finishPublish(ctx, in.SiteID, srcTip)
		}
		// Diverged: create a merge commit on published. A content conflict =>
		// ErrPublishConflict carrying the conflicting paths.
		mergeSHA, paths, e := g.mergeIntoPublished(ctx, in.SiteID, from, srcTip, in.Actor, in.Message)
		if e != nil {
			if len(paths) > 0 {
				return &PublishConflictError{Paths: paths}
			}
			return e
		}
		ci, e = g.commitInfo(ctx, in.SiteID, mergeSHA)
		if e != nil {
			return e
		}
		return g.finishPublish(ctx, in.SiteID, mergeSHA)
	})
	if err != nil {
		return CommitInfo{}, err
	}
	g.bestEffortMirror(ctx, in.SiteID, BranchPublished)
	return ci, nil
}

// finishPublish refreshes the served worktree and updates the published pointer
// metadata after the published ref move succeeds.
func (g *gitService) finishPublish(ctx context.Context, id uuid.UUID, sha string) error {
	g.refreshServed(ctx, id, BranchPublished)
	s := sha
	if e := g.store.SetPublished(ctx, id, &s); e != nil {
		return fmt.Errorf("%w: set published pointer: %v", ErrGit, e)
	}
	return nil
}

// ---- History / diff / rollback ----

func (g *gitService) GetDiff(ctx context.Context, in DiffOptions) (DiffResult, error) {
	var res DiffResult
	err := g.withReadLock(in.SiteID, func() error {
		fromSHA, e := g.revParse(ctx, in.SiteID, in.From)
		if e != nil {
			return ErrNotFound
		}
		res.FromSHA = fromSHA

		args := []string{"diff", "--numstat"}
		var rangeArgs []string
		if in.To == "" {
			// Working-tree diff of From's branch (staged + unstaged vs From).
			rangeArgs = []string{fromSHA}
			res.ToSHA = ""
		} else {
			toSHA, e := g.revParse(ctx, in.SiteID, in.To)
			if e != nil {
				// The `To` ref does not exist yet (e.g. diffing draft↔published on a
				// never-published site: the `published` branch is created only by the
				// first publish). That is NOT "site not found" — it means everything
				// in `from` is new relative to an empty target. Fall back to git's
				// well-known empty tree so the diff renders all of `from`'s content as
				// changes (exactly "what the first publish will introduce"), instead
				// of 404ing and hiding the publish CTA in the dashboard.
				toSHA = emptyTreeSHA
			}
			res.ToSHA = toSHA
			rangeArgs = []string{fromSHA, toSHA}
		}
		numArgs := append(append([]string{}, args...), rangeArgs...)
		if in.Path != "" {
			numArgs = append(numArgs, "--", in.Path)
		}
		numOut, e := g.run(ctx, in.SiteID, numArgs...)
		if e != nil {
			return wrapGit(e)
		}
		// name-status to classify add/modify/delete/rename.
		nsArgs := append([]string{"diff", "--name-status", "-M"}, rangeArgs...)
		if in.Path != "" {
			nsArgs = append(nsArgs, "--", in.Path)
		}
		nsOut, e := g.run(ctx, in.SiteID, nsArgs...)
		if e != nil {
			return wrapGit(e)
		}
		files := mergeDiff(numOut, nsOut)

		// Unified patch unless name-status mode requested.
		if !in.NameStatus {
			ctxLines := in.ContextLines
			if ctxLines == 0 {
				ctxLines = 3
			}
			puArgs := append([]string{"diff", "--unified=" + strconv.Itoa(ctxLines)}, rangeArgs...)
			if in.Path != "" {
				puArgs = append(puArgs, "--", in.Path)
			}
			puOut, e := g.run(ctx, in.SiteID, puArgs...)
			if e != nil {
				return wrapGit(e)
			}
			attachPatches(files, puOut)
		}
		res.Files = files
		return nil
	})
	if err != nil {
		return DiffResult{}, err
	}
	return res, nil
}

func (g *gitService) GetLog(ctx context.Context, in LogOptions) ([]CommitInfo, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = g.cfg.ListLimit
	}
	if limit > g.cfg.MaxLogLimit {
		limit = g.cfg.MaxLogLimit
	}
	var commits []CommitInfo
	err := g.withReadLock(in.SiteID, func() error {
		// Resolve the branch tip first (and validate the branch exists).
		if _, e := g.revParse(ctx, in.SiteID, string(in.Branch)); e != nil {
			return ErrNotFound
		}
		// The revision we feed to `git log`. Default to the branch name (a validated
		// BranchName, never user-controlled option text).
		ref := string(in.Branch)
		if in.Before != "" {
			// H1: the pagination cursor in.Before is ATTACKER-CONTROLLED. Passed raw
			// (e.g. "--output=/path" or a crafted "-foo" ref) it would be parsed by git
			// as an OPTION — an argument-injection / arbitrary-file-write surface. We
			// LAUNDER it through revParse (exactly as GetDiff does its endpoints): the
			// returned value is a resolved 40-hex commit SHA that can never begin with a
			// dash nor smuggle an option. We FAIL CLOSED — an unresolvable/malicious
			// cursor is rejected as ErrNotFound rather than reaching the git invocation.
			beforeSHA, e := g.revParse(ctx, in.SiteID, in.Before)
			if e != nil {
				return ErrNotFound
			}
			// Strictly older than Before: log Before^ ..., but Before may be the tip;
			// use the parent range so Before itself is excluded. beforeSHA is hex, so
			// appending "^" yields a safe revision expression (still no leading dash).
			ref = beforeSHA + "^"
		}
		// "--" separates the revision from any pathspec so no user value (here the
		// already-laundered ref, and the structurally-validated in.Path) is ever
		// parsed as an option. The ref precedes "--" because it is a revision, not a
		// path; it is safe because it is either a validated BranchName or a laundered
		// SHA(^).
		args := []string{"log", "-n", strconv.Itoa(limit), logFormat, ref, "--"}
		if in.Path != "" {
			args = append(args, in.Path)
		}
		out, e := g.run(ctx, in.SiteID, args...)
		if e != nil {
			// A bad Before cursor (Before^ of a root commit) yields a clean empty log.
			var ge *GitError
			if errors.As(e, &ge) {
				return nil
			}
			return wrapGit(e)
		}
		commits = parseLog(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return commits, nil
}

func (g *gitService) Rollback(ctx context.Context, id uuid.UUID, branch BranchName, toSHA, baseSHA string, actor Actor) (CommitInfo, error) {
	if err := refuseProtectedBranch(branch); err != nil {
		return CommitInfo{}, err
	}
	if baseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required"}
	}
	if toSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "toSha", Reason: "required"}
	}
	var ci CommitInfo
	err := g.withWriteLock(id, func() error {
		if _, e := g.requireSite(ctx, id); e != nil {
			return e
		}
		if e := g.checkBaseSHA(ctx, id, branch, baseSHA, nil); e != nil {
			return e
		}
		target, e := g.revParse(ctx, id, toSHA)
		if e != nil {
			return ErrNotFound
		}
		// Ancestor-only: toSHA must be reachable from the current branch tip.
		if e := g.runErr(ctx, id, "merge-base", "--is-ancestor", target, string(branch)); e != nil {
			return ErrNotFound
		}
		if e := g.checkoutBranch(ctx, id, branch); e != nil {
			return e
		}
		// Revert-to-tree: load the target tree into the index + worktree, then commit
		// a NEW commit (history preserved, not rewritten).
		if _, e := g.run(ctx, id, "read-tree", target); e != nil {
			return wrapGit(e)
		}
		if _, e := g.run(ctx, id, "checkout-index", "-a", "-f"); e != nil {
			return wrapGit(e)
		}
		// Remove worktree files no longer in the target tree.
		if _, e := g.run(ctx, id, "clean", "-fd"); e != nil {
			return wrapGit(e)
		}
		short := target
		if len(short) > 7 {
			short = short[:7]
		}
		newCI, e := g.commitStaged(ctx, id, branch, actor, "Rollback to "+short)
		if e != nil {
			return e
		}
		ci = newCI
		return nil
	})
	if err != nil {
		return CommitInfo{}, err
	}
	g.refreshServed(ctx, id, branch)
	g.bestEffortMirror(ctx, id, branch)
	return ci, nil
}

// ---- Data-plane read side ----

func (g *gitService) ServedTree(ctx context.Context, id uuid.UUID, branch BranchName) (TreeHandle, error) {
	rec, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return TreeHandle{}, fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found || rec.DeletedAt != nil {
		return TreeHandle{}, ErrNotFound
	}
	var th TreeHandle
	err = g.withReadLock(id, func() error {
		sha, e := g.revParse(ctx, id, string(branch))
		if e != nil {
			return ErrNotFound
		}
		// Materialize the served worktree if it is missing or stale, then return it.
		root := g.servedDir(id, branch)
		if e := g.ensureServed(ctx, id, branch, sha); e != nil {
			return e
		}
		ci, e := g.commitInfo(ctx, id, sha)
		if e != nil {
			return e
		}
		th = TreeHandle{
			Root:       root,
			CommitSHA:  sha,
			CommitTime: ci.Committed,
			Exists:     true,
		}
		return nil
	})
	if err != nil {
		return TreeHandle{}, err
	}
	return th, nil
}

// ResolveForServing parses a Host header OR a "/host/{handle}/..." path, resolves
// the (current or redirected) handle -> uuid + branch, and returns a ServeTarget
// pointing at the read-only served worktree root. It is NOT part of the frozen
// CANONICAL §1 Service interface (which exposes ServedTree); it is an additional
// convenience the data plane uses directly. Errors: ErrNotFound (unknown
// handle/branch), ErrValidation (malformed host/path).
func (g *gitService) ResolveForServing(ctx context.Context, hostOrPath string) (ServeTarget, error) {
	handle, branch, err := parseHostOrPath(hostOrPath)
	if err != nil {
		return ServeTarget{}, err
	}
	// {handle}--published is not addressable via "--" (CANONICAL §5.2).
	if branch == BranchPublished && strings.Contains(hostOrPath, doubleHyphen) {
		return ServeTarget{}, &ValidationError{Field: "branch", Reason: "published is not addressable via --"}
	}
	if err := ValidateHandleForResolver(handle); err != nil {
		return ServeTarget{}, err
	}
	if branch != BranchPublished {
		if err := validateBranchName(branch); err != nil {
			return ServeTarget{}, err
		}
	}
	rec, found, err := g.store.GetSiteByHandle(ctx, strings.ToLower(string(handle)))
	if err != nil {
		return ServeTarget{}, fmt.Errorf("%w: resolve handle: %v", ErrGit, err)
	}
	if !found {
		// Try a redirect (former handle); the data plane emits a 301 upstream.
		rec, found, err = g.store.GetSiteByRedirect(ctx, strings.ToLower(string(handle)))
		if err != nil {
			return ServeTarget{}, fmt.Errorf("%w: resolve redirect: %v", ErrGit, err)
		}
		if !found {
			return ServeTarget{}, ErrNotFound
		}
	}
	th, err := g.ServedTree(ctx, rec.ID, branch)
	if err != nil {
		return ServeTarget{}, err
	}
	return ServeTarget{
		SiteID:  rec.ID,
		Handle:  Handle(rec.Handle),
		Branch:  branch,
		Root:    th.Root,
		HeadSHA: th.CommitSHA,
	}, nil
}

// GitHub mirror / remote methods (SetRemote / MirrorPush / FetchAndUpdate) live
// in mirror.go to keep this file focused on the local-git core.
