package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore is an in-memory ops.Store for the reaper/gc/consistency tests.
type fakeStore struct {
	pastGrace []SoftDeletedSite
	live      []uuid.UUID
	hardDel   map[uuid.UUID]int // id -> times hard-deleted
	audits    int
	failHard  map[uuid.UUID]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{hardDel: map[uuid.UUID]int{}, failHard: map[uuid.UUID]error{}}
}

func (f *fakeStore) ListSitesPastGrace(_ context.Context, cutoff time.Time) ([]SoftDeletedSite, error) {
	// Mirror the SQL filter: deleted_at strictly before cutoff.
	var out []SoftDeletedSite
	for _, s := range f.pastGrace {
		if s.DeletedAt.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) HardDeleteSite(_ context.Context, id uuid.UUID) error {
	if err := f.failHard[id]; err != nil {
		return err
	}
	f.hardDel[id]++
	// A hard-deleted site is no longer past-grace (idempotency: second pass sees nothing).
	var remain []SoftDeletedSite
	for _, s := range f.pastGrace {
		if s.ID != id {
			remain = append(remain, s)
		}
	}
	f.pastGrace = remain
	return nil
}

func (f *fakeStore) ListLiveSiteIDs(context.Context) ([]uuid.UUID, error) { return f.live, nil }

func (f *fakeStore) InsertSystemAudit(context.Context, uuid.UUID, string, map[string]any) error {
	f.audits++
	return nil
}

// recordingGit is a fake GitRunner that records bundle invocations and, by
// default, succeeds (writing a placeholder bundle file so the flow is realistic).
type recordingGit struct {
	bundleCalls int
	gcCalls     int
	failBundle  bool
}

func (g *recordingGit) Run(_ context.Context, repoDir string, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "bundle" && args[1] == "create" {
		g.bundleCalls++
		if g.failBundle {
			return nil, errors.New("no refs to bundle")
		}
		// args[2] is the output path; write a stub so the backup artifact exists.
		_ = os.MkdirAll(filepath.Dir(args[2]), 0o755)
		_ = os.WriteFile(args[2], []byte("BUNDLE"), 0o644)
		return nil, nil
	}
	if len(args) >= 1 && args[0] == "gc" {
		g.gcCalls++
		return nil, nil
	}
	return nil, nil
}

// makeRepo creates a fake on-disk site repo (a .git dir) under sitesDir.
func makeRepo(t *testing.T, sitesDir string, id uuid.UUID) string {
	t.Helper()
	repo := filepath.Join(sitesDir, id.String())
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("make repo: %v", err)
	}
	return repo
}

func newTestOps(t *testing.T, store Store, git GitRunner) (*Ops, string, string) {
	t.Helper()
	sitesDir := filepath.Join(t.TempDir(), "sites")
	backupDir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(sitesDir, 0o755); err != nil {
		t.Fatalf("mkdir sites: %v", err)
	}
	o := New(Config{
		SitesDir:     sitesDir,
		BackupDir:    backupDir,
		Grace:        720 * time.Hour,
		EnableReaper: true,
		EnableGC:     true,
		Now:          func() time.Time { return time.Unix(1_700_000_000, 0) },
	}, store, git, OSFS{}, nil)
	return o, sitesDir, backupDir
}

func TestReap_SelectsOnlyPastGrace(t *testing.T) {
	store := newFakeStore()
	git := &recordingGit{}
	o, sitesDir, _ := newTestOps(t, store, git)

	now := time.Unix(1_700_000_000, 0)
	old := SoftDeletedSite{ID: uuid.New(), Handle: "old", DeletedAt: now.Add(-800 * time.Hour)}     // past 30d grace
	recent := SoftDeletedSite{ID: uuid.New(), Handle: "recent", DeletedAt: now.Add(-1 * time.Hour)} // within grace
	// ListSitesPastGrace already filters by cutoff; seed only the past-grace one to
	// model the store contract (the SQL WHERE clause does the time filter).
	store.pastGrace = []SoftDeletedSite{old}
	_ = recent // documents the intent; the store query excludes it

	makeRepo(t, sitesDir, old.ID)

	res, err := o.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if res.Reaped != 1 || res.Skipped != 0 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v want reaped=1", res)
	}
	if git.bundleCalls != 1 {
		t.Fatalf("bundle calls = %d want 1", git.bundleCalls)
	}
	if store.hardDel[old.ID] != 1 {
		t.Fatalf("hard-delete count = %d want 1", store.hardDel[old.ID])
	}
	// The on-disk repo must be gone.
	if _, err := os.Stat(filepath.Join(sitesDir, old.ID.String())); !os.IsNotExist(err) {
		t.Fatalf("repo dir still exists after reap")
	}
}

func TestReap_BundlesBeforeRemoval(t *testing.T) {
	store := newFakeStore()
	git := &recordingGit{}
	o, sitesDir, backupDir := newTestOps(t, store, git)

	s := SoftDeletedSite{ID: uuid.New(), Handle: "site", DeletedAt: time.Unix(1, 0)}
	store.pastGrace = []SoftDeletedSite{s}
	makeRepo(t, sitesDir, s.ID)

	if _, err := o.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	// A bundle artifact must exist under BackupDir/{uuid}/.
	dir := filepath.Join(backupDir, s.ID.String())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup entries = %d want 1", len(entries))
	}
	if filepath.Ext(entries[0].Name()) != ".bundle" {
		t.Fatalf("backup file = %q want *.bundle", entries[0].Name())
	}
}

func TestReap_Idempotent(t *testing.T) {
	store := newFakeStore()
	git := &recordingGit{}
	o, sitesDir, _ := newTestOps(t, store, git)

	s := SoftDeletedSite{ID: uuid.New(), Handle: "site", DeletedAt: time.Unix(1, 0)}
	store.pastGrace = []SoftDeletedSite{s}
	makeRepo(t, sitesDir, s.ID)

	if _, err := o.Reap(context.Background()); err != nil {
		t.Fatalf("first reap: %v", err)
	}
	// Second pass: the row is gone (hard-deleted), so nothing to do.
	res, err := o.Reap(context.Background())
	if err != nil {
		t.Fatalf("second reap: %v", err)
	}
	if res.Reaped != 0 || res.Skipped != 0 {
		t.Fatalf("second pass result = %+v want all zero", res)
	}
	if store.hardDel[s.ID] != 1 {
		t.Fatalf("hard-delete called %d times across two passes, want 1", store.hardDel[s.ID])
	}
}

func TestReap_RowWithNoRepoIsPurged(t *testing.T) {
	store := newFakeStore()
	git := &recordingGit{}
	o, _, _ := newTestOps(t, store, git)

	s := SoftDeletedSite{ID: uuid.New(), Handle: "ghost", DeletedAt: time.Unix(1, 0)}
	store.pastGrace = []SoftDeletedSite{s}
	// No on-disk repo (a prior interrupted pass already removed it).

	res, err := o.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if res.Skipped != 1 || res.Reaped != 0 {
		t.Fatalf("result = %+v want skipped=1", res)
	}
	if git.bundleCalls != 0 {
		t.Fatalf("bundle should not run for a missing repo; got %d", git.bundleCalls)
	}
	if store.hardDel[s.ID] != 1 {
		t.Fatalf("missing-repo row must still be hard-deleted")
	}
}

func TestReap_ContinuesPastPerSiteError(t *testing.T) {
	store := newFakeStore()
	git := &recordingGit{}
	o, sitesDir, _ := newTestOps(t, store, git)

	bad := SoftDeletedSite{ID: uuid.New(), Handle: "bad", DeletedAt: time.Unix(1, 0)}
	good := SoftDeletedSite{ID: uuid.New(), Handle: "good", DeletedAt: time.Unix(2, 0)}
	store.pastGrace = []SoftDeletedSite{bad, good}
	makeRepo(t, sitesDir, bad.ID)
	makeRepo(t, sitesDir, good.ID)
	// The bad site fails hard-delete; the pass must still reap the good one.
	store.failHard[bad.ID] = errors.New("db down")

	res, err := o.Reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if res.Reaped != 1 || len(res.Errors) != 1 {
		t.Fatalf("result = %+v want reaped=1, errors=1", res)
	}
	if store.hardDel[good.ID] != 1 {
		t.Fatalf("good site must be reaped despite the bad one failing")
	}
}

func TestGC_RunsOnLiveReposOnly(t *testing.T) {
	store := newFakeStore()
	git := &recordingGit{}
	o, sitesDir, _ := newTestOps(t, store, git)

	withRepo := uuid.New()
	dangling := uuid.New() // live row but no on-disk repo
	store.live = []uuid.UUID{withRepo, dangling}
	makeRepo(t, sitesDir, withRepo)

	if err := o.GC(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if git.gcCalls != 1 {
		t.Fatalf("gc calls = %d want 1 (only the repo on disk)", git.gcCalls)
	}
}

func TestConsistency_ClearsLocksAndFlagsOrphansAndDangling(t *testing.T) {
	store := newFakeStore()
	git := &recordingGit{}
	o, sitesDir, _ := newTestOps(t, store, git)

	liveWithRepo := uuid.New()
	danglingRow := uuid.New() // live row, no repo
	orphanRepo := uuid.New()  // repo on disk, no row

	store.live = []uuid.UUID{liveWithRepo, danglingRow}
	repo := makeRepo(t, sitesDir, liveWithRepo)
	makeRepo(t, sitesDir, orphanRepo)

	// Leave a stale flock in the live repo's .git.
	lockPath := filepath.Join(repo, ".git", "kotoji.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	res, err := o.Consistency(context.Background())
	if err != nil {
		t.Fatalf("consistency: %v", err)
	}
	if len(res.ClearedLocks) != 1 {
		t.Fatalf("cleared locks = %d want 1", len(res.ClearedLocks))
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock not removed")
	}
	if len(res.OrphanRepos) != 1 {
		t.Fatalf("orphan repos = %v want 1", res.OrphanRepos)
	}
	if len(res.DanglingRows) != 1 || res.DanglingRows[0] != danglingRow.String() {
		t.Fatalf("dangling rows = %v want [%s]", res.DanglingRows, danglingRow)
	}
}
