//go:build integration

package site

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run REAL git in a temp dir against the crafted fixtures under
// backend/testdata. They are build-tagged `integration` so they stay out of the
// default fast `go test ./...` path; run them with `go test -tags=integration`.
// (They need no Postgres — they use the in-memory memStore.)

// fixturePath resolves a testdata fixture relative to the backend module root.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	// internal/site -> ../../testdata
	p := filepath.Join("..", "..", "testdata", name)
	abs, err := filepath.Abs(p)
	require.NoError(t, err)
	require.FileExists(t, abs)
	return abs
}

// zipSourceFromFile loads a fixture zip into a ZipSource (with the real declared
// size, so the size gate behaves as in production).
func zipSourceFromFile(t *testing.T, name string) ZipSource {
	t.Helper()
	b, err := os.ReadFile(fixturePath(t, name))
	require.NoError(t, err)
	return ZipSource{Reader: bytes.NewReader(b), Size: int64(len(b)), Filename: name}
}

func newRealService(t *testing.T) (*gitService, *memStore) {
	t.Helper()
	if !gitAvailable() {
		t.Skip("git binary not available")
	}
	store := newMemStore()
	clk := fixedClock()
	store.clock = clk
	g := NewServiceWithClock(store, newExecRunner("git"), Config{Root: t.TempDir()}, clk)
	return g, store
}

// TestIntegration_RoundTrip exercises create -> write -> commit -> publish ->
// diff -> log -> rollback against the real git binary.
func TestIntegration_RoundTrip(t *testing.T) {
	ctx := context.Background()
	g, _ := newRealService(t)

	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "round-trip", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)

	c1, err := g.WriteFile(ctx, WriteFileInput{
		SiteID: s.ID, Branch: BranchDraft, Path: "index.html",
		Content: []byte("v1\n"), BaseSHA: base, Commit: true, Actor: testActor(),
	})
	require.NoError(t, err)
	c2, err := g.WriteFile(ctx, WriteFileInput{
		SiteID: s.ID, Branch: BranchDraft, Path: "index.html",
		Content: []byte("v1\nv2\n"), BaseSHA: c1.SHA, Commit: true, Actor: testActor(),
	})
	require.NoError(t, err)

	// Publish.
	pub, err := g.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: c2.SHA, Actor: testActor()})
	require.NoError(t, err)
	assert.Equal(t, c2.SHA, pub.SHA)

	// Diff c1..c2 shows a modification with a patch.
	dr, err := g.GetDiff(ctx, DiffOptions{SiteID: s.ID, From: c1.SHA, To: c2.SHA})
	require.NoError(t, err)
	require.Len(t, dr.Files, 1)
	assert.Equal(t, "modified", dr.Files[0].Status)
	assert.NotEmpty(t, dr.Files[0].UnifiedPatch)

	// Log newest-first.
	log, err := g.GetLog(ctx, LogOptions{SiteID: s.ID, Branch: BranchDraft, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, c2.SHA, log[0].SHA)
	assert.Equal(t, string(SourceEditor), log[0].Via)

	// Rollback draft to c1's tree.
	rb, err := g.Rollback(ctx, s.ID, BranchDraft, c1.SHA, c2.SHA, testActor())
	require.NoError(t, err)
	fc, err := g.ReadFile(ctx, s.ID, BranchDraft, "", "index.html")
	require.NoError(t, err)
	assert.Equal(t, "v1\n", string(fc.Content))
	assert.NotEqual(t, c1.SHA, rb.SHA, "rollback is a NEW forward commit")
}

// TestIntegration_ServedTree materializes the served worktree and asserts files
// land on disk WITHOUT a .git directory.
func TestIntegration_ServedTree(t *testing.T) {
	ctx := context.Background()
	g, _ := newRealService(t)
	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "served", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)
	_, err = g.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
	require.NoError(t, err)

	th, err := g.ServedTree(ctx, s.ID, BranchPublished)
	require.NoError(t, err)
	require.True(t, th.Exists)
	require.NotEmpty(t, th.Root)
	assert.FileExists(t, filepath.Join(th.Root, "index.html"))
	// .git must NEVER be inside the served tree.
	_, statErr := os.Stat(filepath.Join(th.Root, ".git"))
	assert.True(t, os.IsNotExist(statErr), "served tree must exclude .git")
}

// TestIntegration_Fixture_ZipSlip proves the crafted zipslip.zip is rejected and
// nothing is committed.
func TestIntegration_Fixture_ZipSlip(t *testing.T) {
	ctx := context.Background()
	g, _ := newRealService(t)
	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "fx-slip", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)
	_, err = g.ImportZip(ctx, s.ID, BranchDraft, zipSourceFromFile(t, "zipslip.zip"), base, testActor())
	assert.True(t, errors.Is(err, ErrZipSlip), "got %v", err)
	// Tip unchanged (nothing committed).
	assert.Equal(t, base, draftTip(t, g, s.ID))
}

// TestIntegration_Fixture_ZipBomb proves the crafted zipbomb.zip is rejected.
func TestIntegration_Fixture_ZipBomb(t *testing.T) {
	ctx := context.Background()
	g, _ := newRealService(t)
	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "fx-bomb", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)
	_, err = g.ImportZip(ctx, s.ID, BranchDraft, zipSourceFromFile(t, "zipbomb.zip"), base, testActor())
	assert.True(t, errors.Is(err, ErrZipTooLarge), "got %v", err)
}

// TestIntegration_Fixture_BadType proves the crafted badtype.zip is rejected.
func TestIntegration_Fixture_BadType(t *testing.T) {
	ctx := context.Background()
	g, _ := newRealService(t)
	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "fx-type", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)
	_, err = g.ImportZip(ctx, s.ID, BranchDraft, zipSourceFromFile(t, "badtype.zip"), base, testActor())
	assert.True(t, errors.Is(err, ErrZipBadType), "got %v", err)
}

// TestIntegration_Fixture_Valid proves the crafted valid.zip imports cleanly.
func TestIntegration_Fixture_Valid(t *testing.T) {
	ctx := context.Background()
	g, _ := newRealService(t)
	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "fx-valid", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)
	ci, err := g.ImportZip(ctx, s.ID, BranchDraft, zipSourceFromFile(t, "valid.zip"), base, testActor())
	require.NoError(t, err)
	assert.NotEqual(t, base, ci.SHA)
	fc, err := g.ReadFile(ctx, s.ID, BranchDraft, "", "css/app.css")
	require.NoError(t, err)
	assert.Equal(t, "body{margin:0}", string(fc.Content))
}
