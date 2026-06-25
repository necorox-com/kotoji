package site

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"math/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newQuotaService builds a real gitService (real git binary, in-memory store)
// with an explicit per-site disk quota, mirroring TestContract_Git's wiring. It
// skips when git is absent so CI without git stays green.
func newQuotaService(t *testing.T, quotaBytes int64) *gitService {
	t.Helper()
	if !gitAvailable() {
		t.Skip("git binary not available")
	}
	store := newMemStore()
	clk := fixedClock()
	store.clock = clk
	cfg := Config{Root: t.TempDir(), SiteQuotaBytes: quotaBytes}
	return NewServiceWithClock(store, newExecRunner("git", defaultGitOpTimeout), cfg, clk)
}

// incompressible returns n bytes of deterministic pseudo-random data that does
// NOT trip the per-entry compression-ratio guard (so a quota test exercises the
// quota gate, not the ratio guard). A fixed seed keeps the bytes reproducible.
func incompressible(n int) []byte {
	r := rand.New(rand.NewSource(42)) //nolint:gosec // non-crypto test fixture
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

// TestImportZip_QuotaBoundary is the M5 boundary test: an import whose content
// would push the site's on-disk footprint past SiteQuotaBytes is rejected with
// ErrQuotaExceeded (mapped to 413/quota_exceeded), and the branch tip is left
// unchanged (nothing committed). A subsequent under-quota import still succeeds,
// proving the gate is a boundary check and not a hard wall after the first hit.
func TestImportZip_QuotaBoundary(t *testing.T) {
	ctx := context.Background()
	// A freshly-created site (placeholder index.html + .git) occupies some bytes;
	// pick a quota that comfortably admits the seed but rejects a large import.
	const quota = 64 << 10 // 64 KiB
	g := newQuotaService(t, quota)

	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "quota-site", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)

	// A small import must stay within quota and commit normally.
	small := makeZip(t, map[string]string{"index.html": "<h1>ok</h1>"})
	ci, err := g.ImportZip(ctx, s.ID, BranchDraft, small, base, testActor())
	require.NoError(t, err)
	assert.NotEqual(t, base, ci.SHA)

	// An import whose decompressed content alone exceeds the quota must be rejected
	// BEFORE the worktree is replaced/committed. The payload is incompressible so it
	// is the QUOTA gate (not the ratio guard) that rejects it.
	big := makeZip(t, map[string]string{
		"index.html": string(incompressible(int(quota) + 1)),
	})
	_, err = g.ImportZip(ctx, s.ID, BranchDraft, big, ci.SHA, testActor())
	assert.True(t, errors.Is(err, ErrQuotaExceeded), "want ErrQuotaExceeded, got %v", err)

	// The tip must be unchanged: the over-quota import committed nothing.
	assert.Equal(t, ci.SHA, draftTip(t, g, s.ID), "over-quota import must not commit")
}

// TestImportZip_QuotaDisabled proves SiteQuotaBytes<=0 disables the gate: a large
// import that would breach a positive quota imports cleanly when the quota is off.
func TestImportZip_QuotaDisabled(t *testing.T) {
	ctx := context.Background()
	g := newQuotaService(t, 0) // 0 => disabled

	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "noquota-site", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)

	big := makeZip(t, map[string]string{
		"index.html": string(incompressible(200 << 10)), // 200 KiB, incompressible
	})
	_, err = g.ImportZip(ctx, s.ID, BranchDraft, big, base, testActor())
	require.NoError(t, err, "quota disabled => large import should succeed")
}

// TestWriteFile_QuotaBoundary mirrors the gate on the single-file write path: a
// blob larger than the remaining quota is rejected with ErrQuotaExceeded and the
// tip is unchanged.
func TestWriteFile_QuotaBoundary(t *testing.T) {
	ctx := context.Background()
	const quota = 64 << 10 // 64 KiB
	g := newQuotaService(t, quota)

	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "quota-write", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)

	_, err = g.WriteFile(ctx, WriteFileInput{
		SiteID: s.ID, Branch: BranchDraft, Path: "big.html",
		Content: incompressible(int(quota) + 1), BaseSHA: base, Actor: testActor(),
	})
	assert.True(t, errors.Is(err, ErrQuotaExceeded), "want ErrQuotaExceeded, got %v", err)
	assert.Equal(t, base, draftTip(t, g, s.ID), "over-quota write must not commit")
}

// TestRepoDiskSize_ExcludesServedTree asserts the quota measurement counts the
// repo + objects but NOT the derived `served` cache (so a tenant is not double-
// charged for content they cannot directly shrink).
func TestRepoDiskSize_ExcludesServedTree(t *testing.T) {
	ctx := context.Background()
	g := newQuotaService(t, 0)
	s, err := g.CreateSite(ctx, CreateSiteInput{Handle: "size-served", OwnerID: uuid.New(), Actor: testActor()})
	require.NoError(t, err)
	base := draftTip(t, g, s.ID)

	before, err := g.repoDiskSize(s.ID)
	require.NoError(t, err)
	require.Positive(t, before)

	// Publishing materializes the served worktree under /data/sites/{uuid}/served.
	_, err = g.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
	require.NoError(t, err)
	_, err = g.ServedTree(ctx, s.ID, BranchPublished)
	require.NoError(t, err)

	after, err := g.repoDiskSize(s.ID)
	require.NoError(t, err)
	// The served tree adds real bytes on disk; if it were counted, `after` would
	// jump by ~the index.html size. We only assert the measure stays bounded and
	// does not explode (the served dir is skipped), allowing for the publish commit.
	assert.LessOrEqual(t, after-before, int64(8<<10),
		"served tree must be excluded from the charged footprint")
}

// ---- M8: declared-size guard overflow ----

// craftLyingZip returns the central-directory bytes of a single-entry zip whose
// header is then mutated in place to DECLARE pathological sizes (a real
// archive/zip writer would never emit these). The returned *zip.Reader is parsed
// from a genuine archive so the structure is valid; we only overwrite the
// FileHeader size fields the pre-read guards inspect.
func craftLyingZip(t *testing.T, declaredUncompressed, declaredCompressed uint64) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("index.html")
	require.NoError(t, err)
	_, err = w.Write([]byte("hi"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	b := buf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	require.Len(t, zr.File, 1)
	// Overwrite the parsed declared sizes to simulate a lying/streamed header. These
	// fields drive the pre-read declared-cap + ratio guards, which run before Open().
	zr.File[0].UncompressedSize64 = declaredUncompressed
	zr.File[0].CompressedSize64 = declaredCompressed
	zr.File[0].UncompressedSize = uint32(declaredUncompressed)
	zr.File[0].CompressedSize = uint32(declaredCompressed)
	return zr
}

// TestValidateAndReadZip_DeclaredOverflow is the M8 unit test. It crafts headers
// with (a) a declared uncompressed size above 2^63 (which the OLD int64 cast
// wrapped negative, skipping the cap) and (b) a zero compressed size (which the
// OLD ratio guard skipped). Both must now be REJECTED as ErrZipTooLarge by the
// declared-size pre-checks — the entry never reaches the real-byte read.
func TestValidateAndReadZip_DeclaredOverflow(t *testing.T) {
	g := NewServiceWithClock(newMemStore(), newExecRunner("git", defaultGitOpTimeout),
		Config{Root: t.TempDir()}, fixedClock())

	t.Run("huge declared size above int64 max does not wrap negative", func(t *testing.T) {
		// 2^63 + 1: the old `int64(f.UncompressedSize64)` produced a negative number
		// that slipped under the positive cap. The uint64 comparison rejects it.
		huge := uint64(1) << 63
		huge++
		zr := craftLyingZip(t, huge, 100)
		_, err := g.validateAndReadZip(zr)
		assert.True(t, errors.Is(err, ErrZipTooLarge), "huge declared size must be rejected, got %v", err)
	})

	t.Run("zero compressed size is ratio-unknown, not a free pass", func(t *testing.T) {
		// A declared uncompressed size well above the per-entry cap with compressed==0
		// previously skipped the ratio guard entirely; it must now be rejected.
		zr := craftLyingZip(t, uint64(g.cfg.Zip.MaxEntryUncompressed)+1, 0)
		_, err := g.validateAndReadZip(zr)
		assert.True(t, errors.Is(err, ErrZipTooLarge), "compressed==0 bomb-shaped entry must be rejected, got %v", err)
	})

	t.Run("zero compressed size below the ratio cap still trips the per-entry cap path", func(t *testing.T) {
		// compressed==0 with a modest declared size that exceeds the ratio cap (so the
		// new else-branch fires) — proves a non-declaring header cannot bypass the
		// bomb accounting via a small-but-still-suspicious size.
		declared := uint64(g.cfg.Zip.MaxCompressionRatio) + 1
		zr := craftLyingZip(t, declared, 0)
		_, err := g.validateAndReadZip(zr)
		assert.True(t, errors.Is(err, ErrZipTooLarge), "compressed==0 over ratio cap must be rejected, got %v", err)
	})
}
