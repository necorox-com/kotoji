package site

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedClock returns a deterministic clock for reproducible commit timestamps.
func fixedClock() func() time.Time {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var mu sync.Mutex
	n := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

// gitAvailable reports whether the real git binary is on PATH (gates the
// real-impl arm of the contract suite without failing CI when git is absent).
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// makeZip builds an in-memory zip from a path->content map for tests.
func makeZip(t *testing.T, files map[string]string) ZipSource {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	b := buf.Bytes()
	return ZipSource{Reader: bytes.NewReader(b), Size: int64(len(b)), Filename: "site.zip"}
}

// makeRawZip builds a ZipSource from raw bytes with an explicit declared size
// (used to fake a lying declared size for the too-large gate).
func makeRawZip(b []byte, declaredSize int64) ZipSource {
	return ZipSource{Reader: bytes.NewReader(b), Size: declaredSize, Filename: "site.zip"}
}

// makeBombZip builds a zip with one highly-compressible entry whose declared
// uncompressed size is huge — the per-entry/ratio guard must reject it.
func makeBombZip(t *testing.T) ZipSource {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("index.html")
	require.NoError(t, err)
	// 5 MiB of zeros compresses to almost nothing -> ratio guard trips well before
	// the total-size guard, and the content is large enough to matter.
	_, err = w.Write(bytes.Repeat([]byte{0}, 5<<20))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	b := buf.Bytes()
	return ZipSource{Reader: bytes.NewReader(b), Size: int64(len(b)), Filename: "bomb.zip"}
}

// serviceFactory builds a fresh Service for one subtest run.
type serviceFactory func(t *testing.T) Service

// testActor is the standard editor actor used across the contract tests.
func testActor() Actor {
	return Actor{UserID: uuid.New(), Name: "Tester", Email: "tester@example.com", Via: SourceEditor}
}

// newSite is a contract-test helper that creates a site and returns it.
func newSite(t *testing.T, svc Service, handle string) Site {
	t.Helper()
	s, err := svc.CreateSite(context.Background(), CreateSiteInput{
		Handle:  Handle(handle),
		OwnerID: uuid.New(),
		Actor:   testActor(),
	})
	require.NoError(t, err)
	return s
}

// draftTip reads the current tip SHA of the draft branch (the lock token).
func draftTip(t *testing.T, svc Service, id uuid.UUID) string {
	t.Helper()
	branches, err := svc.ListBranches(context.Background(), id)
	require.NoError(t, err)
	for _, b := range branches {
		if b.Name == BranchDraft {
			return b.HeadSHA
		}
	}
	t.Fatalf("draft branch not found")
	return ""
}

// ---- The shared contract suite ----

// TestContract_Fake runs the full contract against the in-memory FakeService.
func TestContract_Fake(t *testing.T) {
	testContract(t, func(t *testing.T) Service {
		return NewFakeServiceWithClock(fixedClock())
	})
}

// TestContract_Git runs the SAME contract against the real gitService backed by
// the real git binary in a t.TempDir + an in-memory Store. Skipped if git is
// absent.
func TestContract_Git(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git binary not available")
	}
	testContract(t, func(t *testing.T) Service {
		root := t.TempDir()
		store := newMemStore()
		clk := fixedClock()
		store.clock = clk
		return NewServiceWithClock(store, newExecRunner("git", defaultGitOpTimeout), Config{Root: root}, clk)
	})
}

// testContract is the single behavioral contract both implementations satisfy.
func testContract(t *testing.T, factory serviceFactory) {
	ctx := context.Background()

	t.Run("CreateSite_valid", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "my-tool")
		assert.Equal(t, Handle("my-tool"), s.Handle)
		assert.Equal(t, BranchDraft, s.DefaultBranch)

		branches, err := svc.ListBranches(ctx, s.ID)
		require.NoError(t, err)
		require.Len(t, branches, 1)
		assert.Equal(t, BranchDraft, branches[0].Name)
		assert.NotEmpty(t, branches[0].HeadSHA)

		// The initial commit exists and is readable.
		fc, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "index.html")
		require.NoError(t, err)
		assert.NotEmpty(t, fc.Content)
		assert.Equal(t, branches[0].HeadSHA, fc.SHA)
	})

	t.Run("CreateSite_reservedHandle", func(t *testing.T) {
		svc := factory(t)
		for _, h := range ReservedHandles {
			_, err := svc.CreateSite(ctx, CreateSiteInput{Handle: Handle(h), OwnerID: uuid.New(), Actor: testActor()})
			assert.Truef(t, errors.Is(err, ErrReservedHandle), "handle %q should be reserved, got %v", h, err)
		}
	})

	t.Run("CreateSite_invalidHandle", func(t *testing.T) {
		svc := factory(t)
		for _, h := range []string{"UPPER", "a--b", "-lead", "trail-", "ab", "x_y"} {
			_, err := svc.CreateSite(ctx, CreateSiteInput{Handle: Handle(h), OwnerID: uuid.New(), Actor: testActor()})
			assert.Truef(t, errors.Is(err, ErrValidation), "handle %q should be invalid, got %v", h, err)
		}
	})

	t.Run("CreateSite_handleTaken", func(t *testing.T) {
		svc := factory(t)
		newSite(t, svc, "dup-handle")
		_, err := svc.CreateSite(ctx, CreateSiteInput{Handle: "dup-handle", OwnerID: uuid.New(), Actor: testActor()})
		assert.True(t, errors.Is(err, ErrHandleTaken))
	})

	t.Run("CreateSite_fromZip", func(t *testing.T) {
		svc := factory(t)
		zsrc := makeZip(t, map[string]string{
			"index.html":  "<h1>hi</h1>",
			"css/app.css": "body{}",
		})
		s, err := svc.CreateSite(ctx, CreateSiteInput{
			Handle: "zip-site", OwnerID: uuid.New(), Zip: &zsrc, Actor: testActor(),
		})
		require.NoError(t, err)
		fc, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "index.html")
		require.NoError(t, err)
		assert.Equal(t, "<h1>hi</h1>", string(fc.Content))
		fc2, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "css/app.css")
		require.NoError(t, err)
		assert.Equal(t, "body{}", string(fc2.Content))
	})

	t.Run("WriteFile_happy", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "wf-happy")
		base := draftTip(t, svc, s.ID)
		ci, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "page.html",
			Content: []byte("<p>new</p>"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		assert.NotEqual(t, base, ci.SHA, "tip must advance")
		fc, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "page.html")
		require.NoError(t, err)
		assert.Equal(t, "<p>new</p>", string(fc.Content))
		assert.Equal(t, ci.SHA, fc.SHA, "read returns the new lock token")
	})

	t.Run("WriteFile_staleBaseSHA", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "wf-stale")
		base := draftTip(t, svc, s.ID)
		// First write advances the tip.
		_, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "a.html",
			Content: []byte("a"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		// Second write reuses the STALE base -> conflict.
		_, err = svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "b.html",
			Content: []byte("b"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.True(t, errors.Is(err, ErrConflict))
		var ce *ConflictError
		require.True(t, errors.As(err, &ce))
		assert.Equal(t, base, ce.Expected)
		assert.NotEqual(t, base, ce.Actual)
	})

	t.Run("WriteFile_emptyBaseSHA", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "wf-empty")
		_, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "x.html",
			Content: []byte("x"), BaseSHA: "", Commit: true, Actor: testActor(),
		})
		assert.True(t, errors.Is(err, ErrValidation))
		assert.False(t, errors.Is(err, ErrConflict), "empty base is validation, never force")
	})

	t.Run("WriteFile_pathTraversal", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "wf-trav")
		base := draftTip(t, svc, s.ID)
		for _, p := range []string{"../escape.html", "/abs.html", `a\b.html`, "../../etc.html"} {
			_, err := svc.WriteFile(ctx, WriteFileInput{
				SiteID: s.ID, Branch: BranchDraft, Path: p,
				Content: []byte("x"), BaseSHA: base, Commit: true, Actor: testActor(),
			})
			assert.Truef(t, errors.Is(err, ErrValidation), "path %q should be rejected, got %v", p, err)
		}
	})

	t.Run("WriteFile_badExtension", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "wf-ext")
		base := draftTip(t, svc, s.ID)
		for _, p := range []string{"shell.php", "bin.exe", "run.sh"} {
			_, err := svc.WriteFile(ctx, WriteFileInput{
				SiteID: s.ID, Branch: BranchDraft, Path: p,
				Content: []byte("x"), BaseSHA: base, Commit: true, Actor: testActor(),
			})
			assert.Truef(t, errors.Is(err, ErrValidation), "ext %q should be rejected, got %v", p, err)
		}
	})

	t.Run("WriteFile_publishedRefused", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "wf-pub")
		_, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchPublished, Path: "x.html",
			Content: []byte("x"), BaseSHA: "deadbeef", Commit: true, Actor: testActor(),
		})
		assert.True(t, errors.Is(err, ErrValidation))
	})

	t.Run("WriteFile_lostUpdate_serialized", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "wf-lost")
		base := draftTip(t, svc, s.ID)
		_, err1 := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "one.html",
			Content: []byte("1"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		_, err2 := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "two.html",
			Content: []byte("2"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		// Exactly one wins; the other sees a conflict.
		assert.NoError(t, err1)
		assert.True(t, errors.Is(err2, ErrConflict))
	})

	t.Run("DeleteFile_happy_and_stale", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "df-site")
		base := draftTip(t, svc, s.ID)
		ci, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "gone.html",
			Content: []byte("bye"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		// Stale delete.
		_, err = svc.DeleteFile(ctx, s.ID, BranchDraft, "gone.html", base, testActor())
		assert.True(t, errors.Is(err, ErrConflict))
		// Happy delete with the fresh tip.
		_, err = svc.DeleteFile(ctx, s.ID, BranchDraft, "gone.html", ci.SHA, testActor())
		require.NoError(t, err)
		_, err = svc.ReadFile(ctx, s.ID, BranchDraft, "", "gone.html")
		assert.True(t, errors.Is(err, ErrNotFound))
	})

	t.Run("ReadFile_notFound", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "rf-site")
		_, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "missing.html")
		assert.True(t, errors.Is(err, ErrNotFound))
		_, err = svc.ReadFile(ctx, uuid.New(), BranchDraft, "", "index.html")
		assert.True(t, errors.Is(err, ErrNotFound))
	})

	t.Run("ListFiles_rootAndRecursive", func(t *testing.T) {
		svc := factory(t)
		zsrc := makeZip(t, map[string]string{
			"index.html":     "i",
			"css/app.css":    "c",
			"js/lib/util.js": "u",
		})
		s, err := svc.CreateSite(ctx, CreateSiteInput{Handle: "lf-site", OwnerID: uuid.New(), Zip: &zsrc, Actor: testActor()})
		require.NoError(t, err)

		// Non-recursive root: index.html + css + js dirs.
		entries, ref, err := svc.ListFiles(ctx, ListFilesInput{SiteID: s.ID, Branch: BranchDraft})
		require.NoError(t, err)
		assert.NotEmpty(t, ref.SHA)
		names := entryNames(entries)
		assert.Contains(t, names, "index.html")
		assert.Contains(t, names, "css")
		assert.Contains(t, names, "js")

		// Recursive: includes nested file.
		all, _, err := svc.ListFiles(ctx, ListFilesInput{SiteID: s.ID, Branch: BranchDraft, Recursive: true})
		require.NoError(t, err)
		var hasNested bool
		for _, e := range all {
			if e.Path == "js/lib/util.js" {
				hasNested = true
			}
		}
		assert.True(t, hasNested, "recursive listing must include nested file")
	})

	t.Run("ImportZip_happy_replaces", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "iz-site")
		base := draftTip(t, svc, s.ID)
		zsrc := makeZip(t, map[string]string{"index.html": "<h1>imported</h1>", "about.html": "about"})
		ci, err := svc.ImportZip(ctx, s.ID, BranchDraft, zsrc, base, testActor())
		require.NoError(t, err)
		assert.NotEqual(t, base, ci.SHA)
		fc, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "index.html")
		require.NoError(t, err)
		assert.Equal(t, "<h1>imported</h1>", string(fc.Content))
		fc2, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "about.html")
		require.NoError(t, err)
		assert.Equal(t, "about", string(fc2.Content))
	})

	t.Run("ImportZip_zipSlip", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "iz-slip")
		base := draftTip(t, svc, s.ID)
		zsrc := makeZip(t, map[string]string{"../evil.html": "x"})
		_, err := svc.ImportZip(ctx, s.ID, BranchDraft, zsrc, base, testActor())
		assert.True(t, errors.Is(err, ErrZipSlip), "got %v", err)
	})

	t.Run("ImportZip_tooLarge_declared", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "iz-large")
		base := draftTip(t, svc, s.ID)
		// Lie about the declared size to trip the pre-read gate.
		zsrc := makeRawZip([]byte("PK\x05\x06"), (50<<20)+1)
		_, err := svc.ImportZip(ctx, s.ID, BranchDraft, zsrc, base, testActor())
		assert.True(t, errors.Is(err, ErrZipTooLarge))
	})

	t.Run("ImportZip_tooManyFiles", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "iz-many")
		base := draftTip(t, svc, s.ID)
		files := map[string]string{}
		for i := 0; i < DefaultMaxZipEntries+1; i++ {
			files["f"+itoa(i)+".html"] = "x"
		}
		zsrc := makeZip(t, files)
		_, err := svc.ImportZip(ctx, s.ID, BranchDraft, zsrc, base, testActor())
		assert.True(t, errors.Is(err, ErrZipTooManyFiles))
	})

	t.Run("ImportZip_zipBomb", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "iz-bomb")
		base := draftTip(t, svc, s.ID)
		_, err := svc.ImportZip(ctx, s.ID, BranchDraft, makeBombZip(t), base, testActor())
		assert.True(t, errors.Is(err, ErrZipTooLarge), "got %v", err)
	})

	t.Run("ImportZip_badType", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "iz-type")
		base := draftTip(t, svc, s.ID)
		zsrc := makeZip(t, map[string]string{"hack.sh": "rm -rf /"})
		_, err := svc.ImportZip(ctx, s.ID, BranchDraft, zsrc, base, testActor())
		assert.True(t, errors.Is(err, ErrZipBadType))
	})

	t.Run("ImportZip_publishedRefused", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "iz-pub")
		zsrc := makeZip(t, map[string]string{"index.html": "x"})
		_, err := svc.ImportZip(ctx, s.ID, BranchPublished, zsrc, "x", testActor())
		assert.True(t, errors.Is(err, ErrValidation))
	})

	t.Run("Publish_firstPublish", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "pub-first")
		base := draftTip(t, svc, s.ID)
		ci, err := svc.Publish(ctx, PublishInput{SiteID: s.ID, From: BranchDraft, BaseSHA: base, Actor: testActor()})
		require.NoError(t, err)
		assert.Equal(t, base, ci.SHA, "first publish points published at draft tip")
		got, err := svc.GetSite(ctx, s.ID)
		require.NoError(t, err)
		assert.True(t, got.HasPublished)
		assert.Equal(t, base, got.PublishedSHA)
	})

	t.Run("Publish_fastForward", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "pub-ff")
		base := draftTip(t, svc, s.ID)
		_, err := svc.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
		require.NoError(t, err)
		// Advance draft, then publish again -> fast-forward (no new commit, published == new draft tip).
		ci, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "v2.html",
			Content: []byte("v2"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		pub, err := svc.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: ci.SHA, Actor: testActor()})
		require.NoError(t, err)
		assert.Equal(t, ci.SHA, pub.SHA, "fast-forward published to draft tip")
	})

	t.Run("Publish_idempotent", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "pub-idem")
		base := draftTip(t, svc, s.ID)
		first, err := svc.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
		require.NoError(t, err)
		second, err := svc.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
		require.NoError(t, err)
		assert.Equal(t, first.SHA, second.SHA, "double publish is a no-op")
	})

	t.Run("Publish_staleBase", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "pub-stale")
		base := draftTip(t, svc, s.ID)
		_, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "z.html",
			Content: []byte("z"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		// Publish with the now-stale draft base.
		_, err = svc.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
		assert.True(t, errors.Is(err, ErrConflict))
	})

	t.Run("Rollback_createsNewCommit", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "rb-site")
		base := draftTip(t, svc, s.ID)
		c1, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "r.html",
			Content: []byte("first"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		c2, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "r.html",
			Content: []byte("second"), BaseSHA: c1.SHA, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		// Roll back to c1's tree.
		rb, err := svc.Rollback(ctx, s.ID, BranchDraft, c1.SHA, c2.SHA, testActor())
		require.NoError(t, err)
		assert.NotEqual(t, c1.SHA, rb.SHA, "rollback creates a NEW commit, no rewrite")
		fc, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "r.html")
		require.NoError(t, err)
		assert.Equal(t, "first", string(fc.Content), "tree reverted to c1")
	})

	t.Run("Rollback_stale", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "rb-stale")
		base := draftTip(t, svc, s.ID)
		c1, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "r.html",
			Content: []byte("x"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		// Wrong baseSHA (use the original base, which is now stale).
		_, err = svc.Rollback(ctx, s.ID, BranchDraft, base, base, testActor())
		assert.True(t, errors.Is(err, ErrConflict))
		_ = c1
	})

	t.Run("GetLog_pagination", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "log-site")
		tip := draftTip(t, svc, s.ID)
		for i := 0; i < 3; i++ {
			ci, err := svc.WriteFile(ctx, WriteFileInput{
				SiteID: s.ID, Branch: BranchDraft, Path: "p" + itoa(i) + ".html",
				Content: []byte("v"), BaseSHA: tip, Commit: true, Actor: testActor(),
			})
			require.NoError(t, err)
			tip = ci.SHA
		}
		all, err := svc.GetLog(ctx, LogOptions{SiteID: s.ID, Branch: BranchDraft, Limit: 50})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(all), 4) // initial + 3
		// Newest-first.
		assert.Equal(t, tip, all[0].SHA)
		// Limit.
		two, err := svc.GetLog(ctx, LogOptions{SiteID: s.ID, Branch: BranchDraft, Limit: 2})
		require.NoError(t, err)
		assert.Len(t, two, 2)
		// Before cursor (strictly older than the tip).
		older, err := svc.GetLog(ctx, LogOptions{SiteID: s.ID, Branch: BranchDraft, Limit: 50, Before: tip})
		require.NoError(t, err)
		for _, c := range older {
			assert.NotEqual(t, tip, c.SHA, "Before excludes the cursor commit")
		}
	})

	t.Run("GetDiff_addModDelete", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "diff-site")
		base := draftTip(t, svc, s.ID)
		c1, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "d.html",
			Content: []byte("one\n"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		c2, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "d.html",
			Content: []byte("one\ntwo\n"), BaseSHA: c1.SHA, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		dr, err := svc.GetDiff(ctx, DiffOptions{SiteID: s.ID, From: c1.SHA, To: c2.SHA})
		require.NoError(t, err)
		assert.Equal(t, c1.SHA, dr.FromSHA)
		assert.Equal(t, c2.SHA, dr.ToSHA)
		require.Len(t, dr.Files, 1)
		assert.Equal(t, "d.html", dr.Files[0].Path)
		assert.Equal(t, "modified", dr.Files[0].Status)
	})

	// Regression: diffing draft against a not-yet-created `published` branch (a
	// never-published site, as the dashboard publish panel does on load) must NOT
	// 404 — it diffs against the empty tree so all of draft surfaces as additions.
	// Before the fix this returned ErrNotFound, hiding the publish CTA in the UI.
	t.Run("GetDiff_missingTargetRef_diffsAgainstEmptyTree", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "diff-first-publish")
		base := draftTip(t, svc, s.ID)
		c1, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "index.html",
			Content: []byte("<h1>hi</h1>\n"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)

		// `published` does not exist yet on a never-published site.
		dr, err := svc.GetDiff(ctx, DiffOptions{
			SiteID: s.ID, From: string(BranchDraft), To: string(BranchPublished),
		})
		require.NoError(t, err, "diff against a missing published branch must not 404")
		assert.Equal(t, c1.SHA, dr.FromSHA)
		assert.Equal(t, emptyTreeSHA, dr.ToSHA)
		// The diff direction is from→to (draft→published); against the empty
		// published target the seeded file shows as "deleted" (present in draft,
		// absent in the empty tree). The load-bearing property is that the diff
		// SUCCEEDS and is non-empty, so the panel detects changes and shows the
		// publish CTA rather than the resource-not-found error state.
		require.NotEmpty(t, dr.Files)
		var found bool
		for _, f := range dr.Files {
			if f.Path == "index.html" {
				found = true
				assert.Equal(t, "deleted", f.Status)
			}
		}
		assert.True(t, found, "index.html should appear in the first-publish diff")
	})

	t.Run("Branches_createDeleteAndReservedRefused", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "br-site")
		br, err := svc.CreateBranch(ctx, s.ID, "feature-tester-x", string(BranchDraft))
		require.NoError(t, err)
		assert.Equal(t, BranchName("feature-tester-x"), br.Name)
		assert.Equal(t, "br-site--feature-tester-x", br.PreviewSubdomain)

		// Duplicate.
		_, err = svc.CreateBranch(ctx, s.ID, "feature-tester-x", string(BranchDraft))
		assert.True(t, errors.Is(err, ErrBranchExists))

		// Reserved branch creation refused.
		_, err = svc.CreateBranch(ctx, s.ID, BranchPublished, string(BranchDraft))
		assert.True(t, errors.Is(err, ErrValidation))

		// Cannot delete draft/published.
		assert.True(t, errors.Is(svc.DeleteBranch(ctx, s.ID, BranchDraft), ErrValidation))
		assert.True(t, errors.Is(svc.DeleteBranch(ctx, s.ID, BranchPublished), ErrValidation))

		// Delete the feature branch.
		require.NoError(t, svc.DeleteBranch(ctx, s.ID, "feature-tester-x"))
		err = svc.DeleteBranch(ctx, s.ID, "feature-tester-x")
		assert.True(t, errors.Is(err, ErrNotFound))
	})

	t.Run("ListBranches_previewSubdomain", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "prev-site")
		base := draftTip(t, svc, s.ID)
		_, err := svc.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
		require.NoError(t, err)
		branches, err := svc.ListBranches(ctx, s.ID)
		require.NoError(t, err)
		for _, b := range branches {
			if b.Name == BranchPublished {
				assert.Equal(t, "prev-site", b.PreviewSubdomain)
			}
			if b.Name == BranchDraft {
				assert.Equal(t, "prev-site--draft", b.PreviewSubdomain)
			}
		}
	})

	t.Run("RenameHandle_redirectRecorded", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "old-name")
		// Write a file so we can prove files don't move.
		base := draftTip(t, svc, s.ID)
		_, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "keep.html",
			Content: []byte("keep"), BaseSHA: base, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)

		renamed, err := svc.RenameHandle(ctx, s.ID, "new-name")
		require.NoError(t, err)
		assert.Equal(t, Handle("new-name"), renamed.Handle)

		// New handle resolves; old 404s on the control API.
		_, err = svc.GetSiteByHandle(ctx, "new-name")
		require.NoError(t, err)
		_, err = svc.GetSiteByHandle(ctx, "old-name")
		assert.True(t, errors.Is(err, ErrNotFound))

		// Files unmoved.
		fc, err := svc.ReadFile(ctx, s.ID, BranchDraft, "", "keep.html")
		require.NoError(t, err)
		assert.Equal(t, "keep", string(fc.Content))
	})

	t.Run("RenameHandle_takenAndReserved", func(t *testing.T) {
		svc := factory(t)
		a := newSite(t, svc, "site-a")
		newSite(t, svc, "site-b")
		_, err := svc.RenameHandle(ctx, a.ID, "site-b")
		assert.True(t, errors.Is(err, ErrHandleTaken))
		_, err = svc.RenameHandle(ctx, a.ID, "admin")
		assert.True(t, errors.Is(err, ErrReservedHandle))
	})

	t.Run("DeleteSite_soft", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "del-site")
		require.NoError(t, svc.DeleteSite(ctx, s.ID, testActor()))
		_, err := svc.GetSite(ctx, s.ID)
		assert.True(t, errors.Is(err, ErrNotFound))
		// Double delete -> not found.
		assert.True(t, errors.Is(svc.DeleteSite(ctx, s.ID, testActor()), ErrNotFound))
	})

	t.Run("ResolveForServing_via_resolver", func(t *testing.T) {
		// Both impls expose ResolveForServing as a concrete method (not on the
		// frozen interface). Type-assert to reach it.
		svc := factory(t)
		s := newSite(t, svc, "serve-site")
		base := draftTip(t, svc, s.ID)
		_, err := svc.Publish(ctx, PublishInput{SiteID: s.ID, BaseSHA: base, Actor: testActor()})
		require.NoError(t, err)

		rsv, ok := svc.(interface {
			ResolveForServing(context.Context, string) (ServeTarget, error)
		})
		require.True(t, ok, "impl must expose ResolveForServing")

		// Bare host -> published.
		tgt, err := rsv.ResolveForServing(ctx, "serve-site.hosting.example.com")
		require.NoError(t, err)
		assert.Equal(t, BranchPublished, tgt.Branch)
		assert.Equal(t, s.ID, tgt.SiteID)

		// --draft -> draft.
		tgt2, err := rsv.ResolveForServing(ctx, "serve-site--draft.hosting.example.com")
		require.NoError(t, err)
		assert.Equal(t, BranchDraft, tgt2.Branch)

		// Path fallback.
		tgt3, err := rsv.ResolveForServing(ctx, "/host/serve-site--draft/index.html")
		require.NoError(t, err)
		assert.Equal(t, BranchDraft, tgt3.Branch)

		// Unknown handle -> not found.
		_, err = rsv.ResolveForServing(ctx, "nope-nope.hosting.example.com")
		assert.True(t, errors.Is(err, ErrNotFound))

		// --published is not addressable via "--".
		_, err = rsv.ResolveForServing(ctx, "serve-site--published.hosting.example.com")
		assert.True(t, errors.Is(err, ErrValidation))
	})

	t.Run("ConcurrentWrites_sameSite", func(t *testing.T) {
		svc := factory(t)
		s := newSite(t, svc, "conc-site")
		base := draftTip(t, svc, s.ID)
		const n = 8
		var wg sync.WaitGroup
		results := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, results[i] = svc.WriteFile(ctx, WriteFileInput{
					SiteID: s.ID, Branch: BranchDraft, Path: "c" + itoa(i) + ".html",
					Content: []byte("v"), BaseSHA: base, Commit: true, Actor: testActor(),
				})
			}(i)
		}
		wg.Wait()
		wins := 0
		for _, e := range results {
			if e == nil {
				wins++
			} else {
				assert.True(t, errors.Is(e, ErrConflict), "non-winners must conflict, got %v", e)
			}
		}
		assert.Equal(t, 1, wins, "exactly one writer wins per BaseSHA generation")
	})
}

// entryNames extracts the base names of a FileEntry slice.
func entryNames(entries []FileEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// itoa is a tiny int->string helper to avoid importing strconv in the test body.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
