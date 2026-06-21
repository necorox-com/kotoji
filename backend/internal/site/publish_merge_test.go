package site

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitOnPublished is a test helper that commits a file DIRECTLY on the published
// branch of a real git repo, bypassing the Service's protected-branch guard, so we
// can manufacture a divergence (a hotfix landed on published, e.g. via a GitHub
// merge) and exercise the merge-commit / merge-conflict publish paths.
func commitOnPublished(t *testing.T, repoDir, path, content string) {
	t.Helper()
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(scrubbedEnv(),
			"GIT_AUTHOR_NAME=Hotfix", "GIT_AUTHOR_EMAIL=h@e.com",
			"GIT_COMMITTER_NAME=Hotfix", "GIT_COMMITTER_EMAIL=h@e.com")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	runGit("checkout", "published")
	require.NoError(t, writeFileForTest(repoDir, path, content))
	runGit("add", "-A")
	runGit("commit", "-m", "hotfix on published")
	runGit("checkout", "draft")
}

func writeFileForTest(repoDir, path, content string) error {
	full := filepath.Join(repoDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

// TestPublish_MergeCommit (real git): published diverges from draft on DIFFERENT
// files -> clean merge commit on published (2 parents). CANONICAL §6 step 4.
func TestPublish_MergeCommit(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git binary not available")
	}
	ctx := context.Background()
	root := t.TempDir()
	store := newMemStore()
	g := NewServiceWithClock(store, newExecRunner("git"), Config{Root: root}, fixedClock())

	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "merge-clean", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)
	// First publish (published == draft tip).
	_, err = g.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
	require.NoError(t, err)

	// Diverge: a hotfix on published touches hotfix.html; draft advances index.html.
	commitOnPublished(t, g.repoDir(s.ID), "hotfix.html", "<p>hotfix</p>")
	c1, err := g.WriteFile(ctx, WriteFileInput{
		SiteID: s.ID, Branch: BranchDraft, Path: "feature.html",
		Content: []byte("<p>feature</p>"), BaseSHA: base, Commit: true, Actor: testActor(),
	})
	require.NoError(t, err)

	// Publish draft -> merge commit (different files merge cleanly).
	mc, err := g.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: c1.SHA, Actor: testActor()})
	require.NoError(t, err)
	assert.Len(t, mc.Parents, 2, "merge commit has two parents")

	// Both files present on published after the merge.
	fc, err := g.ReadFile(ctx, s.ID, BranchPublished, "", "hotfix.html")
	require.NoError(t, err)
	assert.Equal(t, "<p>hotfix</p>", string(fc.Content))
	fc2, err := g.ReadFile(ctx, s.ID, BranchPublished, "", "feature.html")
	require.NoError(t, err)
	assert.Equal(t, "<p>feature</p>", string(fc2.Content))
}

// TestPublish_MergeConflict (real git): published and draft change the SAME file
// to different content -> ErrPublishConflict carrying the path. CANONICAL §6.
func TestPublish_MergeConflict(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git binary not available")
	}
	ctx := context.Background()
	root := t.TempDir()
	store := newMemStore()
	g := NewServiceWithClock(store, newExecRunner("git"), Config{Root: root}, fixedClock())

	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "merge-conflict", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)
	_, err = g.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
	require.NoError(t, err)

	// Both sides edit index.html differently -> conflict.
	commitOnPublished(t, g.repoDir(s.ID), "index.html", "<p>published version</p>")
	c1, err := g.WriteFile(ctx, WriteFileInput{
		SiteID: s.ID, Branch: BranchDraft, Path: "index.html",
		Content: []byte("<p>draft version</p>"), BaseSHA: base, Commit: true, Actor: testActor(),
	})
	require.NoError(t, err)

	_, err = g.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: c1.SHA, Actor: testActor()})
	require.True(t, errors.Is(err, ErrPublishConflict), "got %v", err)
	var pce *PublishConflictError
	require.True(t, errors.As(err, &pce))
	assert.Contains(t, pce.Paths, "index.html")

	// Published is unchanged (still the hotfix version) — never force-overwritten.
	fc, err := g.ReadFile(ctx, s.ID, BranchPublished, "", "index.html")
	require.NoError(t, err)
	assert.Equal(t, "<p>published version</p>", string(fc.Content))
}

// TestFakePublish_MergeCommit exercises the FakeService divergent clean-merge path
// (it can manufacture divergence without a real remote via a feature branch that
// it publishes, then advances draft on a disjoint file).
func TestFakePublish_MergeCommit(t *testing.T) {
	ctx := context.Background()
	f := NewFakeServiceWithClock(fixedClock())
	s, err := f.CreateSite(ctx, CreateSiteInput{Handle: "fake-merge", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, f, s.ID)
	_, err = f.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
	require.NoError(t, err)

	// Simulate a published-side hotfix by committing directly on the fake's
	// published branch (test-only access to manufacture divergence).
	f.mu.Lock()
	pub := f.sites[s.ID].branches[BranchPublished]
	tip, _ := pub.tip()
	tree := cloneTree(tip.tree)
	tree["hotfix.html"] = []byte("hotfix")
	f.commit(pub, tree, testActor(), "hotfix")
	f.mu.Unlock()

	// Advance draft on a disjoint file.
	c1, err := f.WriteFile(ctx, WriteFileInput{
		SiteID: s.ID, Branch: BranchDraft, Path: "feature.html",
		Content: []byte("feature"), BaseSHA: base, Commit: true, Actor: testActor(),
	})
	require.NoError(t, err)

	mc, err := f.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: c1.SHA, Actor: testActor()})
	require.NoError(t, err)
	assert.Len(t, mc.Parents, 2)
}

// TestFakePublish_MergeConflict exercises the FakeService conflict path.
func TestFakePublish_MergeConflict(t *testing.T) {
	ctx := context.Background()
	f := NewFakeServiceWithClock(fixedClock())
	s, err := f.CreateSite(ctx, CreateSiteInput{Handle: "fake-conflict", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, f, s.ID)
	_, err = f.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
	require.NoError(t, err)

	// Published-side edit of index.html.
	f.mu.Lock()
	pub := f.sites[s.ID].branches[BranchPublished]
	tip, _ := pub.tip()
	tree := cloneTree(tip.tree)
	tree["index.html"] = []byte("published version")
	f.commit(pub, tree, testActor(), "hotfix")
	f.mu.Unlock()

	// Draft-side edit of the SAME file -> conflict.
	c1, err := f.WriteFile(ctx, WriteFileInput{
		SiteID: s.ID, Branch: BranchDraft, Path: "index.html",
		Content: []byte("draft version"), BaseSHA: base, Commit: true, Actor: testActor(),
	})
	require.NoError(t, err)

	_, err = f.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: c1.SHA, Actor: testActor()})
	require.True(t, errors.Is(err, ErrPublishConflict), "got %v", err)
	var pce *PublishConflictError
	require.True(t, errors.As(err, &pce))
	assert.Contains(t, pce.Paths, "index.html")
}
