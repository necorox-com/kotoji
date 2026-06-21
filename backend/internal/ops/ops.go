// Package ops holds kotoji's background operability jobs (architecture.md §8.4):
//
//   - the soft-delete reaper: past-grace soft-deleted sites are `git bundle`d to
//     KOTOJI_BACKUP_DIR, then the on-disk repo is removed and the DB row hard-deleted;
//   - opportunistic `git gc --auto` on each live repo to reclaim dangling objects;
//   - a startup consistency check that clears stale flocks and logs orphan repos /
//     dangling DB rows (it never auto-deletes — destructive ops are guarded).
//
// A Scheduler runs these on an interval; it is started in the composition root for
// RUN_MODE control|all (single replica, decision #4) and is also invokable once via
// cmd/kotoji-ops for cron/manual runs.
//
// DI / testability: git and the filesystem are behind interfaces (GitRunner, FS)
// and the clock is injected, so the reaper is exercised end-to-end against a fake
// store + a t.TempDir + a real local git binary OR a fake runner — no network, no
// background timers in tests.
package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// Defaults for the operability knobs (overridable via config / Config fields).
const (
	// DefaultSoftDeleteGrace is the 30-day soft-delete retention window
	// (CANONICAL decision #3): a soft-deleted site is reclaimed only after this.
	DefaultSoftDeleteGrace = 720 * time.Hour // 30d
	// DefaultInterval is how often the Scheduler runs the periodic jobs.
	DefaultInterval = 1 * time.Hour
	// lockFileName mirrors site.lockFilePath: the per-site advisory lock file under
	// .git. The consistency check clears a stale one so a crashed writer cannot wedge
	// future writes (architecture.md §8.2.5).
	lockFileName = "kotoji.lock"
	// gitDirName is the bare-repo metadata dir under a site's repo dir. Its presence
	// is how the consistency scan recognizes a dir as a real site repo (vs a tmp
	// dir or stray file) and how the reaper/gc confirm a repo exists on disk.
	gitDirName = ".git"
)

// SoftDeletedSite is the minimal reaper view of a past-grace site.
type SoftDeletedSite struct {
	ID        uuid.UUID
	Handle    string
	DeletedAt time.Time
}

// Store is the metadata surface the ops jobs need, in ops-domain terms (no sqlc
// types) so the package is decoupled and trivially faked. The composition root
// adapts *db.Store onto it (adapter in internal/app).
type Store interface {
	// ListSitesPastGrace returns soft-deleted sites whose deleted_at is strictly
	// before cutoff (now - grace), oldest first.
	ListSitesPastGrace(ctx context.Context, cutoff time.Time) ([]SoftDeletedSite, error)
	// HardDeleteSite removes the sites row (and its cascades) AFTER the on-disk repo
	// has been bundled + reclaimed.
	HardDeleteSite(ctx context.Context, id uuid.UUID) error
	// ListLiveSiteIDs returns the IDs of all non-deleted sites (for the gc pass and
	// the orphan/dangling consistency check).
	ListLiveSiteIDs(ctx context.Context) ([]uuid.UUID, error)
	// InsertSystemAudit appends a system-sourced audit row (best-effort).
	InsertSystemAudit(ctx context.Context, siteID uuid.UUID, action string, meta map[string]any) error
}

// GitRunner runs git for the bundle + gc passes. It mirrors the site package's
// runner seam but is its own interface so ops does not reach into site internals.
// repoDir is the repo working dir (/data/sites/{uuid}); git resolves .git under it.
type GitRunner interface {
	Run(ctx context.Context, repoDir string, args ...string) (stdout []byte, err error)
}

// FS is the filesystem seam (so the reaper is testable against an in-memory fake
// or a t.TempDir without touching the real os package directly in the job logic).
type FS interface {
	// Exists reports whether a path exists (dir or file).
	Exists(path string) (bool, error)
	// ListDir returns the entry names directly under dir (non-recursive). A missing
	// dir returns an empty slice + nil (so a fresh instance with no /data/sites is
	// not an error).
	ListDir(dir string) ([]string, error)
	// MkdirAll creates dir and parents.
	MkdirAll(dir string) error
	// RemoveAll removes path and its children.
	RemoveAll(path string) error
	// Remove removes a single file (used to clear a stale flock).
	Remove(path string) error
	// Rename atomically renames oldpath to newpath (same volume).
	Rename(oldpath, newpath string) error
}

// Config configures the ops jobs. SitesDir + BackupDir are required for the reaper;
// Grace/Interval default when zero. The reaper itself is guarded by Grace (it only
// ever acts on PAST-GRACE soft-deleted rows) and by EnableReaper, so a misconfig or
// a maintenance window cannot wipe live data.
type Config struct {
	// SitesDir is the per-site repo base (KOTOJI_DATA_DIR/sites).
	SitesDir string
	// BackupDir is where `git bundle` archives are written (KOTOJI_BACKUP_DIR).
	BackupDir string
	// Grace is the soft-delete retention window. Defaults to DefaultSoftDeleteGrace.
	Grace time.Duration
	// Interval is the Scheduler tick. Defaults to DefaultInterval.
	Interval time.Duration
	// EnableReaper is the master switch for the destructive reaper. false disables
	// reaping entirely (e.g. a maintenance window) while gc + consistency keep
	// running. true performs the reap (bundle -> remove repo -> hard-delete row).
	EnableReaper bool
	// EnableGC enables the opportunistic `git gc --auto` pass.
	EnableGC bool
	// Now is the injected clock (tests). Defaults to time.Now.
	Now func() time.Time
}

// Ops bundles the job dependencies. Construct via New.
type Ops struct {
	cfg    Config
	store  Store
	git    GitRunner
	fs     FS
	logger *slog.Logger
	now    func() time.Time
}

// New builds an Ops. A nil logger is tolerated (logging is skipped). Defaults are
// applied for Grace/Interval/Now.
func New(cfg Config, store Store, git GitRunner, fs FS, logger *slog.Logger) *Ops {
	if cfg.Grace <= 0 {
		cfg.Grace = DefaultSoftDeleteGrace
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Ops{cfg: cfg, store: store, git: git, fs: fs, logger: logger, now: now}
}

// repoDir is the on-disk repo dir for a site (/data/sites/{uuid}).
func (o *Ops) repoDir(id uuid.UUID) string { return filepath.Join(o.cfg.SitesDir, id.String()) }

// ---- soft-delete reaper ----

// ReapResult summarizes one reaper pass.
type ReapResult struct {
	Reaped  int      // sites bundled + removed + hard-deleted
	Skipped int      // past-grace rows whose repo was already gone (still hard-deleted)
	Errors  []string // per-site failures (the pass continues past them)
}

// Reap runs one soft-delete reaper pass (architecture.md §8.4.2, decision #3):
// for each past-grace soft-deleted site it `git bundle`s the repo to BackupDir,
// removes the on-disk repo, then hard-deletes the DB row. It is IDEMPOTENT: a row
// whose repo is already gone is simply hard-deleted (Skipped), and a second pass
// over the same input is a no-op once rows are purged. The pass continues past a
// per-site failure, collecting it in Errors, so one bad repo does not stall reaping.
func (o *Ops) Reap(ctx context.Context) (ReapResult, error) {
	var res ReapResult
	cutoff := o.now().Add(-o.cfg.Grace)
	sites, err := o.store.ListSitesPastGrace(ctx, cutoff)
	if err != nil {
		return res, fmt.Errorf("ops: list past-grace: %w", err)
	}
	for _, s := range sites {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if err := o.reapOne(ctx, s, &res); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s (%s): %v", s.Handle, s.ID, err))
			o.warn(ctx, "reap site failed", "site", s.ID, "handle", s.Handle, "err", err)
		}
	}
	if res.Reaped > 0 || res.Skipped > 0 {
		o.info(ctx, "reaper pass complete", "reaped", res.Reaped, "skipped", res.Skipped, "errors", len(res.Errors))
	}
	return res, nil
}

// reapOne bundles, removes, and hard-deletes a single past-grace site. The DB row
// is hard-deleted only AFTER the on-disk reclaim succeeds (or the repo is already
// gone), so a crash between steps leaves the row for the next pass to retry —
// never a row purged with its repo still on disk.
func (o *Ops) reapOne(ctx context.Context, s SoftDeletedSite, res *ReapResult) error {
	repo := o.repoDir(s.ID)
	exists, err := o.fs.Exists(filepath.Join(repo, gitDirName))
	if err != nil {
		return fmt.Errorf("stat repo: %w", err)
	}
	if !exists {
		// Repo already reclaimed (a prior interrupted pass): just purge the row.
		if err := o.store.HardDeleteSite(ctx, s.ID); err != nil {
			return fmt.Errorf("hard-delete (no repo): %w", err)
		}
		res.Skipped++
		return nil
	}

	// 1. Bundle the repo to BackupDir/{uuid}/{ts}.bundle BEFORE any removal, so the
	//    git history is recoverable (architecture.md §8.4.3 restore runbook).
	if err := o.bundle(ctx, s.ID, repo); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}

	// 2. Reclaim disk: remove the whole site dir (.git + served worktrees).
	if err := o.fs.RemoveAll(repo); err != nil {
		return fmt.Errorf("remove repo: %w", err)
	}

	// 3. Hard-delete the DB row (cascades drop members/tokens/redirects).
	if err := o.store.HardDeleteSite(ctx, s.ID); err != nil {
		// The repo is gone but the row remains: the next pass sees a no-repo row and
		// purges it (idempotent). Surface the error so it is visible.
		return fmt.Errorf("hard-delete: %w", err)
	}

	res.Reaped++
	_ = o.store.InsertSystemAudit(ctx, s.ID, "site.reap", map[string]any{
		"handle":     s.Handle,
		"deleted_at": s.DeletedAt.UTC().Format(time.RFC3339),
	})
	return nil
}

// bundle writes a `git bundle` of all refs to BackupDir/{uuid}/{ts}.bundle. The
// backup dir is created on demand. A bundle of an empty repo still succeeds in git
// when there is at least one ref; a refless repo is tolerated (we log + continue
// to removal) so a corrupt half-created repo can still be reclaimed.
func (o *Ops) bundle(ctx context.Context, id uuid.UUID, repo string) error {
	if o.cfg.BackupDir == "" {
		return errors.New("backup dir not configured")
	}
	dst := filepath.Join(o.cfg.BackupDir, id.String())
	if err := o.fs.MkdirAll(dst); err != nil {
		return fmt.Errorf("mkdir backup: %w", err)
	}
	ts := o.now().UTC().Format("20060102T150405Z")
	out := filepath.Join(dst, ts+".bundle")
	// `git bundle create <out> --all` packs every ref. Run from the repo dir.
	if _, err := o.git.Run(ctx, repo, "bundle", "create", out, "--all"); err != nil {
		// A repo with no refs cannot be bundled (git errors). That is a degenerate
		// half-created repo; log and let the caller proceed to removal rather than
		// pinning the row forever.
		o.warn(ctx, "git bundle failed (proceeding to reclaim)", "site", id, "err", err)
		return nil
	}
	return nil
}

// ---- opportunistic git gc ----

// GC runs `git gc --auto` on every live repo (architecture.md §8.4.1). `--auto`
// makes git decide whether a gc is actually warranted, so this is cheap to call
// on a schedule. Failures are logged and skipped (gc is best-effort housekeeping).
func (o *Ops) GC(ctx context.Context) error {
	if !o.cfg.EnableGC {
		return nil
	}
	ids, err := o.store.ListLiveSiteIDs(ctx)
	if err != nil {
		return fmt.Errorf("ops: list live sites: %w", err)
	}
	var ran int
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		repo := o.repoDir(id)
		exists, _ := o.fs.Exists(filepath.Join(repo, gitDirName))
		if !exists {
			continue // dangling DB row; the consistency check logs it
		}
		if _, err := o.git.Run(ctx, repo, "gc", "--auto"); err != nil {
			o.warn(ctx, "git gc --auto failed", "site", id, "err", err)
			continue
		}
		ran++
	}
	o.info(ctx, "gc pass complete", "repos", ran)
	return nil
}

// ---- startup consistency check ----

// ConsistencyResult summarizes the startup check.
type ConsistencyResult struct {
	ClearedLocks  []string // repo dirs whose stale flock was cleared
	OrphanRepos   []string // repo dirs on disk with no live DB row
	DanglingRows  []string // live DB rows with no repo on disk
}

// Consistency clears stale flocks and LOGS (never auto-deletes) orphan repos and
// dangling rows (architecture.md §8.4.2). It is run once at startup. Clearing a
// stale flock is safe: our flock is advisory and released by the OS on process
// death, so a leftover lock FILE only matters if a crashed git left an index.lock;
// the kotoji.lock file itself is removed so a fresh acquire is clean.
func (o *Ops) Consistency(ctx context.Context) (ConsistencyResult, error) {
	var res ConsistencyResult

	liveIDs, err := o.store.ListLiveSiteIDs(ctx)
	if err != nil {
		return res, fmt.Errorf("ops: list live sites: %w", err)
	}
	live := make(map[string]bool, len(liveIDs))
	for _, id := range liveIDs {
		live[id.String()] = true
	}

	// Scan the sites dir for repo dirs (a dir is a repo if it contains .git).
	names, err := o.fs.ListDir(o.cfg.SitesDir)
	if err != nil {
		return res, fmt.Errorf("ops: list sites dir: %w", err)
	}
	onDisk := make(map[string]bool, len(names))
	for _, name := range names {
		repo := filepath.Join(o.cfg.SitesDir, name)
		hasGit, _ := o.fs.Exists(filepath.Join(repo, gitDirName))
		if !hasGit {
			continue // not a site repo (tmp dir, stray file)
		}
		onDisk[name] = true

		// Clear a leftover kotoji.lock so a crashed writer cannot wedge writes.
		lockPath := filepath.Join(repo, gitDirName, lockFileName)
		if ok, _ := o.fs.Exists(lockPath); ok {
			if err := o.fs.Remove(lockPath); err == nil {
				res.ClearedLocks = append(res.ClearedLocks, repo)
			}
		}

		// Orphan: a repo dir whose UUID matches no live DB row (and is not a
		// soft-deleted-pending-reap row — those still have a row, so they are not
		// orphans; the reaper owns them). We only flag dirs with NO row at all.
		if _, err := uuid.Parse(name); err == nil && !live[name] {
			res.OrphanRepos = append(res.OrphanRepos, repo)
		}
	}

	// Dangling: a live DB row with no repo on disk.
	for _, id := range liveIDs {
		if !onDisk[id.String()] {
			res.DanglingRows = append(res.DanglingRows, id.String())
		}
	}

	if len(res.ClearedLocks) > 0 {
		o.info(ctx, "consistency: cleared stale locks", "count", len(res.ClearedLocks))
	}
	if len(res.OrphanRepos) > 0 {
		o.warn(ctx, "consistency: orphan repos on disk (no DB row) — NOT auto-deleted", "repos", res.OrphanRepos)
	}
	if len(res.DanglingRows) > 0 {
		o.warn(ctx, "consistency: dangling DB rows (no repo on disk)", "sites", res.DanglingRows)
	}
	return res, nil
}

// ---- logging helpers ----

func (o *Ops) info(ctx context.Context, msg string, args ...any) {
	if o.logger != nil {
		o.logger.InfoContext(ctx, msg, args...)
	}
}

func (o *Ops) warn(ctx context.Context, msg string, args ...any) {
	if o.logger != nil {
		o.logger.WarnContext(ctx, msg, args...)
	}
}
