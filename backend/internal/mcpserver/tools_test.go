package mcpserver

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/site"
)

// callCtx is a background context for handler calls (claims travel via reqFor).
func callCtx() context.Context { return context.Background() }

func TestListSites_ReturnsOnlyPinnedSite(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	mine, _ := seedSite(t, fake, owner, "mine")
	_, _ = seedSite(t, fake, owner, "other") // a second site the token must NOT see
	r := newTestRegistry(fake)
	c := claims(mine.ID, owner, scopeRead, false)

	_, out, err := r.listSites(callCtx(), c, ListSitesArgs{})
	require.NoError(t, err)
	require.Len(t, out.Sites, 1)
	assert.Equal(t, "mine", out.Sites[0].Handle)
	assert.Equal(t, mine.ID.String(), out.Sites[0].UUID)
	assert.Equal(t, "https://mine--draft.hosting.test", out.Sites[0].DraftURL)
	assert.Equal(t, "https://mine.hosting.test", out.Sites[0].PublishedURL)
}

func TestListSites_DeletedSite_NotFound(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, _ := seedSite(t, fake, owner, "gone")
	require.NoError(t, fake.DeleteSite(callCtx(), s.ID, site.Actor{UserID: owner}))
	r := newTestRegistry(fake)

	res, _, err := r.listSites(callCtx(), claims(s.ID, owner, scopeRead, false), ListSitesArgs{})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeNotFound, decodeErrBody(t, res.StructuredContent).Code)
}

func TestReadFile_EchoesCommitAsBaseSHA(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "rf-site")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeRead, false)

	_, out, err := r.readFile(callCtx(), c, ReadFileArgs{Path: "index.html"})
	require.NoError(t, err)
	assert.Equal(t, base, out.Commit, "read_file.commit is the value to pass back as base_sha")
	assert.Equal(t, encodingUTF8, out.Encoding)
	assert.Contains(t, out.Content, "<")
	assert.False(t, out.Truncated)
}

func TestReadFile_BinaryIsBase64(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "bin")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	// Write a binary file (NUL byte → IsBinary) via base64, then read it back.
	raw := []byte{0x89, 0x50, 0x00, 0x01, 0x02}
	_, wr, err := r.writeFile(callCtx(), c, WriteFileArgs{
		Path:     "img.png",
		Content:  base64.StdEncoding.EncodeToString(raw),
		Encoding: strptr(encodingBase64),
		BaseSHA:  base,
	})
	require.NoError(t, err)

	_, out, err := r.readFile(callCtx(), c, ReadFileArgs{Path: "img.png", Branch: strptr("draft"), Ref: strptr(wr.Commit)})
	require.NoError(t, err)
	assert.Equal(t, encodingBase64, out.Encoding)
	decoded, derr := base64.StdEncoding.DecodeString(out.Content)
	require.NoError(t, derr)
	assert.Equal(t, raw, decoded)
}

func TestReadFile_TruncatedAtReadLimit(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "big")
	r := newTestRegistry(fake)
	r.limits = Limits{MaxReadBytes: 4, Limiter: NewMemoryLimiter()}.withDefaults()
	c := claims(s.ID, owner, scopeWrite, false)

	_, wr, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "big.txt", Content: "0123456789", BaseSHA: base})
	require.NoError(t, err)
	_, out, err := r.readFile(callCtx(), c, ReadFileArgs{Path: "big.txt", Ref: strptr(wr.Commit)})
	require.NoError(t, err)
	assert.True(t, out.Truncated)
	assert.Len(t, out.Content, 4)
}

func TestWriteFile_MissingBaseSHA_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, _ := seedSite(t, fake, owner, "wf-site")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "a.html", Content: "x"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestWriteFile_PublishedBranch_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "wfp")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "a.html", Content: "x", BaseSHA: base, Branch: strptr("published")})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestWriteFile_StaleBaseSHA_ConflictWithDetails(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "conf")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	// First write advances the tip.
	_, first, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "index.html", Content: "v1", BaseSHA: base})
	require.NoError(t, err)
	// Second write with the OLD base_sha is stale → conflict.
	res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "index.html", Content: "v2", BaseSHA: base})
	require.NoError(t, err)
	require.True(t, res.IsError)
	body := decodeErrBody(t, res.StructuredContent)
	assert.Equal(t, codeConflict, body.Code)
	assert.True(t, body.Retryable)
	assert.Equal(t, base, body.Details["expected"])
	assert.Equal(t, first.Commit, body.Details["actual"])
	assert.Equal(t, first.Commit, body.Details["current_sha"])
	assert.NotNil(t, body.Details["changed_paths"])
}

func TestWriteFile_Success(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "ok-site")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	_, out, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "page.html", Content: "<h1>hi</h1>", BaseSHA: base})
	require.NoError(t, err)
	assert.True(t, out.Committed)
	assert.NotEmpty(t, out.Commit)
	assert.NotEqual(t, base, out.Commit)
	assert.True(t, out.Pushed)
	assert.Equal(t, len("<h1>hi</h1>"), out.BytesWritten)
}

func TestWriteFile_DisallowedExtension_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "ext")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	// .php is not in the served allowlist; .mp4 is allowlisted but media-denied for MCP.
	for _, p := range []string{"shell.php", "movie.mp4"} {
		res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: p, Content: "x", BaseSHA: base})
		require.NoError(t, err)
		require.True(t, res.IsError, "path %q", p)
		assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code, "path %q", p)
	}
}

func TestWriteFile_TooLarge(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "tl-site")
	r := newTestRegistry(fake)
	r.limits = Limits{MaxFileBytes: 4, Limiter: NewMemoryLimiter()}.withDefaults()
	c := claims(s.ID, owner, scopeWrite, false)

	res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "a.html", Content: "abcdef", BaseSHA: base})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeTooLarge, decodeErrBody(t, res.StructuredContent).Code)
}

func TestWriteFile_PathTraversal_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "trav")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	for _, p := range []string{"../escape.html", "/abs.html", ".git/config", "a\x00b.html", `back\slash.html`} {
		res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: p, Content: "x", BaseSHA: base})
		require.NoError(t, err)
		require.True(t, res.IsError, "path %q must be rejected", p)
		assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code, "path %q", p)
	}
}

func TestCreateSite_CapabilityOff_Forbidden(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	c := claims(uuid.New(), owner, scopeWrite, false) // canCreate=false

	res, _, err := r.createSite(callCtx(), c, CreateSiteArgs{Handle: "fresh"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeForbidden, decodeErrBody(t, res.StructuredContent).Code)
}

func TestCreateSite_CapabilityOn_Success(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	c := claims(uuid.New(), owner, scopeWrite, true) // canCreate=true

	_, out, err := r.createSite(callCtx(), c, CreateSiteArgs{Handle: "fresh-site"})
	require.NoError(t, err)
	assert.Equal(t, "fresh-site", out.Handle)
	assert.NotEmpty(t, out.UUID)
	assert.Equal(t, "draft", out.DefaultBranch)
	assert.NotEmpty(t, out.TokenHint, "create_site must NOT mint a token, only hint")
}

func TestCreateSite_HandleTaken_Conflict(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	seedSite(t, fake, owner, "dup")
	r := newTestRegistry(fake)
	c := claims(uuid.New(), owner, scopeWrite, true)

	res, _, err := r.createSite(callCtx(), c, CreateSiteArgs{Handle: "dup"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeHandleTaken, decodeErrBody(t, res.StructuredContent).Code)
}

func TestSave_CleanTree_NoOp(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "save")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	_, out, err := r.save(callCtx(), c, SaveArgs{BaseSHA: base})
	require.NoError(t, err)
	assert.True(t, out.NoOp, "a clean working tree surfaces as no_op")
	assert.Equal(t, base, out.Commit)
}

func TestSave_PublishedBranch_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "savep")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	res, _, err := r.save(callCtx(), c, SaveArgs{BaseSHA: base, Branch: strptr("published")})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestPublish_StaleBaseSHA_Conflict(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "pub")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopePublish, false)

	// Advance draft so the original base is stale.
	_, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "index.html", Content: "v2", BaseSHA: base})
	require.NoError(t, err)
	res, _, err := r.publish(callCtx(), c, PublishArgs{BaseSHA: base})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeConflict, decodeErrBody(t, res.StructuredContent).Code)
}

func TestPublish_Success(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "pubok")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopePublish, false)

	_, out, err := r.publish(callCtx(), c, PublishArgs{BaseSHA: base})
	require.NoError(t, err)
	assert.NotEmpty(t, out.PublishedCommit)
	assert.Equal(t, "draft", out.From)
	assert.Equal(t, base, out.FromCommit)
	assert.Equal(t, "https://pubok.hosting.test", out.PublishedURL)
	assert.Equal(t, "live", out.Redeploy)

	// The site is now flagged published in the service (DB parity).
	got, gerr := fake.GetSite(callCtx(), s.ID)
	require.NoError(t, gerr)
	assert.True(t, got.HasPublished)
}

func TestRollback_UnreachableToSHA_NotFound(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "rb-site")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	res, _, err := r.rollback(callCtx(), c, RollbackArgs{ToSHA: "deadbeef", BaseSHA: base})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeNotFound, decodeErrBody(t, res.StructuredContent).Code)
}

func TestRollback_Success_ForwardCommit(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "rbok")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	_, w1, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "index.html", Content: "v2", BaseSHA: base})
	require.NoError(t, err)
	// Roll back to the original base; produces a NEW forward commit.
	_, out, err := r.rollback(callCtx(), c, RollbackArgs{ToSHA: base, BaseSHA: w1.Commit})
	require.NoError(t, err)
	assert.Equal(t, base, out.RestoredFrom)
	assert.NotEqual(t, base, out.Commit, "rollback is a forward commit, not a reset")
	assert.True(t, out.Pushed)
}

func TestGetLog_Pagination_And_Via(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "log")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	prev := base
	for i := 0; i < 3; i++ {
		_, w, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "index.html", Content: "v", BaseSHA: prev})
		require.NoError(t, err)
		prev = w.Commit
	}
	// Page 1: limit 2 → cursor present.
	_, page1, err := r.getLog(callCtx(), c, GetLogArgs{Limit: intptr(2)})
	require.NoError(t, err)
	require.Len(t, page1.Commits, 2)
	require.NotNil(t, page1.NextBefore)
	assert.Equal(t, "mcp", page1.Commits[0].Via, "MCP commits carry via=mcp provenance")

	// Page 2: continue from cursor.
	_, page2, err := r.getLog(callCtx(), c, GetLogArgs{Limit: intptr(2), Before: page1.NextBefore})
	require.NoError(t, err)
	assert.NotEmpty(t, page2.Commits)
	assert.NotEqual(t, page1.Commits[0].SHA, page2.Commits[0].SHA)
}

func TestGetLog_LimitOutOfRange_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, _ := seedSite(t, fake, owner, "logv")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeRead, false)

	res, _, err := r.getLog(callCtx(), c, GetLogArgs{Limit: intptr(101)})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestGetDiff_TwoRefMode(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "diff")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	_, w1, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: "extra.html", Content: "<x>", BaseSHA: base})
	require.NoError(t, err)
	_, out, err := r.getDiff(callCtx(), c, GetDiffArgs{From: strptr(base), To: strptr(w1.Commit)})
	require.NoError(t, err)
	assert.Equal(t, base, out.FromCommit)
	assert.NotEmpty(t, out.Files)
}

func TestGetDiff_NoFromOrCommit_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, _ := seedSite(t, fake, owner, "diffv")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeRead, false)

	res, _, err := r.getDiff(callCtx(), c, GetDiffArgs{})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestCreateBranch_Success(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, _ := seedSite(t, fake, owner, "br-site")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeWrite, false)

	_, out, err := r.createBranch(callCtx(), c, CreateBranchArgs{Name: "feature-x"})
	require.NoError(t, err)
	assert.Equal(t, "feature-x", out.Name)
	assert.NotEmpty(t, out.HeadSHA)
}

func TestListFiles_PathTraversal_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, _ := seedSite(t, fake, owner, "lf-site")
	r := newTestRegistry(fake)
	c := claims(s.ID, owner, scopeRead, false)

	res, _, err := r.listFiles(callCtx(), c, ListFilesArgs{Path: strptr("../etc")})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestListFiles_TruncatedAtMaxItems(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "lftrunc")
	r := newTestRegistry(fake)
	r.limits = Limits{MaxListItems: 1, Limiter: NewMemoryLimiter()}.withDefaults()
	c := claims(s.ID, owner, scopeWrite, false)

	prev := base
	for _, name := range []string{"a.html", "b.html"} {
		_, w, err := r.writeFile(callCtx(), c, WriteFileArgs{Path: name, Content: "x", BaseSHA: prev})
		require.NoError(t, err)
		prev = w.Commit
	}
	_, out, err := r.listFiles(callCtx(), c, ListFilesArgs{Recursive: boolptr(true)})
	require.NoError(t, err)
	assert.True(t, out.Truncated)
	assert.Len(t, out.Files, 1)
}

// ---- small pointer helpers for optional args ----

func strptr(s string) *string { return &s }
func intptr(i int) *int       { return &i }
func boolptr(b bool) *bool    { return &b }
