package mcpserver

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// callCtx is a background context for handler calls (claims travel via reqFor).
func callCtx() context.Context { return context.Background() }

func TestListSites_ReturnsOnlyMemberships(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	mine, _ := seedSiteR(t, r, owner, "mine")
	// A second site the user is NOT a member of must NOT appear in list_sites.
	_, _ = seedSite(t, fake, uuid.New(), "other")
	c := claims(owner, scopeRead, false)

	_, out, err := r.listSites(callCtx(), c, ListSitesArgs{})
	require.NoError(t, err)
	require.Len(t, out.Sites, 1)
	assert.Equal(t, "mine", out.Sites[0].Handle)
	assert.Equal(t, mine.ID.String(), out.Sites[0].UUID)
	assert.Equal(t, "owner", out.Sites[0].Role)
	assert.Equal(t, []string{"read"}, out.Sites[0].EffectiveScopes, "effective = token(read) ∩ owner(read,write,publish)")
	assert.Equal(t, "https://mine--draft.hosting.test", out.Sites[0].DraftURL)
	assert.Equal(t, "https://mine.hosting.test", out.Sites[0].PublishedURL)
}

func TestAuthorizeSite_NotAMember_NotFound(t *testing.T) {
	fake := site.NewFakeService()
	r := newTestRegistry(fake)
	// A site owned by someone else; the token user has NO membership on it.
	other := uuid.New()
	s, _ := seedSite(t, fake, other, "stranger")
	caller := uuid.New()
	c := claims(caller, scopeRead, false)

	// Any content tool naming the site must 404 (no existence leak).
	res, _, err := r.readFile(callCtx(), c, ReadFileArgs{Site: string(s.Handle), Path: "index.html"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeNotFound, decodeErrBody(t, res.StructuredContent).Code)
}

func TestAuthorizeSite_MissingSite_Required(t *testing.T) {
	fake := site.NewFakeService()
	r := newTestRegistry(fake)
	c := claims(uuid.New(), scopeRead, false)

	res, _, err := r.readFile(callCtx(), c, ReadFileArgs{Path: "index.html"}) // no Site
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestReadFile_EchoesCommitAsBaseSHA(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "rf-site")
	c := claims(owner, scopeRead, false)

	_, out, err := r.readFile(callCtx(), c, ReadFileArgs{Site: string(s.Handle), Path: "index.html"})
	require.NoError(t, err)
	assert.Equal(t, base, out.Commit, "read_file.commit is the value to pass back as base_sha")
	assert.Equal(t, encodingUTF8, out.Encoding)
	assert.Contains(t, out.Content, "<")
	assert.False(t, out.Truncated)
}

func TestReadFile_BinaryIsBase64(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "bin")
	c := claims(owner, scopeWrite, false)

	// Write a binary file (NUL byte → IsBinary) via base64, then read it back.
	raw := []byte{0x89, 0x50, 0x00, 0x01, 0x02}
	_, wr, err := r.writeFile(callCtx(), c, WriteFileArgs{
		Site:     string(s.Handle),
		Path:     "img.png",
		Content:  base64.StdEncoding.EncodeToString(raw),
		Encoding: strptr(encodingBase64),
		BaseSHA:  base,
	})
	require.NoError(t, err)

	_, out, err := r.readFile(callCtx(), c, ReadFileArgs{Site: string(s.Handle), Path: "img.png", Branch: strptr("draft"), Ref: strptr(wr.Commit)})
	require.NoError(t, err)
	assert.Equal(t, encodingBase64, out.Encoding)
	decoded, derr := base64.StdEncoding.DecodeString(out.Content)
	require.NoError(t, derr)
	assert.Equal(t, raw, decoded)
}

func TestReadFile_TruncatedAtReadLimit(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "big")
	r.limits = Limits{MaxReadBytes: 4, Limiter: NewMemoryLimiter()}.withDefaults()
	c := claims(owner, scopeWrite, false)

	_, wr, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "big.txt", Content: "0123456789", BaseSHA: base})
	require.NoError(t, err)
	_, out, err := r.readFile(callCtx(), c, ReadFileArgs{Site: string(s.Handle), Path: "big.txt", Ref: strptr(wr.Commit)})
	require.NoError(t, err)
	assert.True(t, out.Truncated)
	assert.Len(t, out.Content, 4)
}

func TestWriteFile_MissingBaseSHA_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, _ := seedSiteR(t, r, owner, "wf-site")
	c := claims(owner, scopeWrite, false)

	res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "a.html", Content: "x"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestWriteFile_PublishedBranch_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "wfp")
	c := claims(owner, scopeWrite, false)

	res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "a.html", Content: "x", BaseSHA: base, Branch: strptr("published")})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestWriteFile_StaleBaseSHA_ConflictWithDetails(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "conf")
	c := claims(owner, scopeWrite, false)

	// First write advances the tip.
	_, first, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "index.html", Content: "v1", BaseSHA: base})
	require.NoError(t, err)
	// Second write with the OLD base_sha is stale → conflict.
	res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "index.html", Content: "v2", BaseSHA: base})
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
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "ok-site")
	c := claims(owner, scopeWrite, false)

	_, out, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "page.html", Content: "<h1>hi</h1>", BaseSHA: base})
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
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "ext")
	c := claims(owner, scopeWrite, false)

	// .php is not in the served allowlist; .mp4 is allowlisted but media-denied for MCP.
	for _, p := range []string{"shell.php", "movie.mp4"} {
		res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: p, Content: "x", BaseSHA: base})
		require.NoError(t, err)
		require.True(t, res.IsError, "path %q", p)
		assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code, "path %q", p)
	}
}

func TestWriteFile_TooLarge(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "tl-site")
	r.limits = Limits{MaxFileBytes: 4, Limiter: NewMemoryLimiter()}.withDefaults()
	c := claims(owner, scopeWrite, false)

	res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "a.html", Content: "abcdef", BaseSHA: base})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeTooLarge, decodeErrBody(t, res.StructuredContent).Code)
}

func TestWriteFile_PathTraversal_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "trav")
	c := claims(owner, scopeWrite, false)

	for _, p := range []string{"../escape.html", "/abs.html", ".git/config", "a\x00b.html", `back\slash.html`} {
		res, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: p, Content: "x", BaseSHA: base})
		require.NoError(t, err)
		require.True(t, res.IsError, "path %q must be rejected", p)
		assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code, "path %q", p)
	}
}

func TestCreateSite_CapabilityOff_Forbidden(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	fakeMembersOf(r).addUser(gen.User{ID: owner, CanCreateSites: true, IsActive: true})
	c := claims(owner, scopeWrite, false) // token canCreate=false

	res, _, err := r.createSite(callCtx(), c, CreateSiteArgs{Handle: "fresh"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeForbidden, decodeErrBody(t, res.StructuredContent).Code)
}

func TestCreateSite_UserFlagOff_Forbidden(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	// Token says canCreate=true, but the USER account cannot create sites.
	fakeMembersOf(r).addUser(gen.User{ID: owner, CanCreateSites: false, IsActive: true})
	c := claims(owner, scopeWrite, true)

	res, _, err := r.createSite(callCtx(), c, CreateSiteArgs{Handle: "fresh"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeForbidden, decodeErrBody(t, res.StructuredContent).Code)
}

func TestCreateSite_CapabilityOn_Success(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	fakeMembersOf(r).addUser(gen.User{ID: owner, CanCreateSites: true, IsActive: true})
	c := claims(owner, scopeWrite, true) // token canCreate=true AND user flag on

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
	r := newTestRegistry(fake)
	seedSite(t, fake, owner, "dup")
	fakeMembersOf(r).addUser(gen.User{ID: owner, CanCreateSites: true, IsActive: true})
	c := claims(owner, scopeWrite, true)

	res, _, err := r.createSite(callCtx(), c, CreateSiteArgs{Handle: "dup"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeHandleTaken, decodeErrBody(t, res.StructuredContent).Code)
}

func TestSave_CleanTree_NoOp(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "save")
	c := claims(owner, scopeWrite, false)

	_, out, err := r.save(callCtx(), c, SaveArgs{Site: string(s.Handle), BaseSHA: base})
	require.NoError(t, err)
	assert.True(t, out.NoOp, "a clean working tree surfaces as no_op")
	assert.Equal(t, base, out.Commit)
}

func TestSave_PublishedBranch_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "savep")
	c := claims(owner, scopeWrite, false)

	res, _, err := r.save(callCtx(), c, SaveArgs{Site: string(s.Handle), BaseSHA: base, Branch: strptr("published")})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestPublish_StaleBaseSHA_Conflict(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "pub")
	c := claims(owner, scopePublish, false)

	// Advance draft so the original base is stale.
	_, _, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "index.html", Content: "v2", BaseSHA: base})
	require.NoError(t, err)
	res, _, err := r.publish(callCtx(), c, PublishArgs{Site: string(s.Handle), BaseSHA: base})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeConflict, decodeErrBody(t, res.StructuredContent).Code)
}

func TestPublish_Success(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "pubok")
	c := claims(owner, scopePublish, false)

	_, out, err := r.publish(callCtx(), c, PublishArgs{Site: string(s.Handle), BaseSHA: base})
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

// TestPublish_RequestMode_NonOwnerForbidden is the MCP-path mirror of the REST
// publish_mode gate (internal/api/publish.go, M3): on a site in 'request' mode an
// editor (non-owner) may NOT publish directly — only the owner can. It mirrors the
// REST 403.
func TestPublish_RequestMode_NonOwnerForbidden(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	editor := uuid.New()
	r := newTestRegistry(fake)

	// Seed a 'request'-mode site directly (the default seed is 'direct'), then list
	// its draft tip so the publish carries a fresh base_sha (isolating the gate).
	s, err := fake.CreateSite(callCtx(), site.CreateSiteInput{
		Handle:      site.Handle("reqmode"),
		OwnerID:     owner,
		PublishMode: "request",
		Actor:       site.Actor{UserID: owner, Via: site.SourceEditor},
	})
	require.NoError(t, err)
	_, ref, err := fake.ListFiles(callCtx(), site.ListFilesInput{SiteID: s.ID, Branch: site.BranchDraft})
	require.NoError(t, err)

	m := fakeMembersOf(r)
	m.grant(s, owner, gen.SiteRoleOwner)
	m.grant(s, editor, gen.SiteRoleEditor)

	// Editor (non-owner) with the publish scope is FORBIDDEN to publish directly.
	res, _, err := r.publish(callCtx(), claims(editor, scopePublish, false), PublishArgs{Site: string(s.Handle), BaseSHA: ref.SHA})
	require.NoError(t, err)
	require.True(t, res.IsError)
	body := decodeErrBody(t, res.StructuredContent)
	assert.Equal(t, codeForbidden, body.Code)
	assert.Contains(t, body.Message, "publish requests")

	// The OWNER may always publish directly, even in 'request' mode (REST parity).
	_, out, err := r.publish(callCtx(), claims(owner, scopePublish, false), PublishArgs{Site: string(s.Handle), BaseSHA: ref.SHA})
	require.NoError(t, err)
	assert.NotEmpty(t, out.PublishedCommit)
}

func TestRollback_UnreachableToSHA_NotFound(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "rb-site")
	c := claims(owner, scopeWrite, false)

	res, _, err := r.rollback(callCtx(), c, RollbackArgs{Site: string(s.Handle), ToSHA: "deadbeef", BaseSHA: base})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeNotFound, decodeErrBody(t, res.StructuredContent).Code)
}

func TestRollback_Success_ForwardCommit(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "rbok")
	c := claims(owner, scopeWrite, false)

	_, w1, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "index.html", Content: "v2", BaseSHA: base})
	require.NoError(t, err)
	// Roll back to the original base; produces a NEW forward commit.
	_, out, err := r.rollback(callCtx(), c, RollbackArgs{Site: string(s.Handle), ToSHA: base, BaseSHA: w1.Commit})
	require.NoError(t, err)
	assert.Equal(t, base, out.RestoredFrom)
	assert.NotEqual(t, base, out.Commit, "rollback is a forward commit, not a reset")
	assert.True(t, out.Pushed)
}

func TestGetLog_Pagination_And_Via(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "log")
	c := claims(owner, scopeWrite, false)

	prev := base
	for i := 0; i < 3; i++ {
		_, w, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "index.html", Content: "v", BaseSHA: prev})
		require.NoError(t, err)
		prev = w.Commit
	}
	// Page 1: limit 2 → cursor present.
	_, page1, err := r.getLog(callCtx(), c, GetLogArgs{Site: string(s.Handle), Limit: intptr(2)})
	require.NoError(t, err)
	require.Len(t, page1.Commits, 2)
	require.NotNil(t, page1.NextBefore)
	assert.Equal(t, "mcp", page1.Commits[0].Via, "MCP commits carry via=mcp provenance")

	// Page 2: continue from cursor.
	_, page2, err := r.getLog(callCtx(), c, GetLogArgs{Site: string(s.Handle), Limit: intptr(2), Before: page1.NextBefore})
	require.NoError(t, err)
	assert.NotEmpty(t, page2.Commits)
	assert.NotEqual(t, page1.Commits[0].SHA, page2.Commits[0].SHA)
}

func TestGetLog_LimitOutOfRange_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, _ := seedSiteR(t, r, owner, "logv")
	c := claims(owner, scopeRead, false)

	res, _, err := r.getLog(callCtx(), c, GetLogArgs{Site: string(s.Handle), Limit: intptr(101)})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestGetDiff_TwoRefMode(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "diff")
	c := claims(owner, scopeWrite, false)

	_, w1, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: "extra.html", Content: "<x>", BaseSHA: base})
	require.NoError(t, err)
	_, out, err := r.getDiff(callCtx(), c, GetDiffArgs{Site: string(s.Handle), From: strptr(base), To: strptr(w1.Commit)})
	require.NoError(t, err)
	assert.Equal(t, base, out.FromCommit)
	assert.NotEmpty(t, out.Files)
}

func TestGetDiff_NoFromOrCommit_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, _ := seedSiteR(t, r, owner, "diffv")
	c := claims(owner, scopeRead, false)

	res, _, err := r.getDiff(callCtx(), c, GetDiffArgs{Site: string(s.Handle)})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestCreateBranch_Success(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, _ := seedSiteR(t, r, owner, "br-site")
	c := claims(owner, scopeWrite, false)

	_, out, err := r.createBranch(callCtx(), c, CreateBranchArgs{Site: string(s.Handle), Name: "feature-x"})
	require.NoError(t, err)
	assert.Equal(t, "feature-x", out.Name)
	assert.NotEmpty(t, out.HeadSHA)
}

func TestListFiles_PathTraversal_Validation(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, _ := seedSiteR(t, r, owner, "lf-site")
	c := claims(owner, scopeRead, false)

	res, _, err := r.listFiles(callCtx(), c, ListFilesArgs{Site: string(s.Handle), Path: strptr("../etc")})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeValidation, decodeErrBody(t, res.StructuredContent).Code)
}

func TestListFiles_TruncatedAtMaxItems(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "lftrunc")
	r.limits = Limits{MaxListItems: 1, Limiter: NewMemoryLimiter()}.withDefaults()
	c := claims(owner, scopeWrite, false)

	prev := base
	for _, name := range []string{"a.html", "b.html"} {
		_, w, err := r.writeFile(callCtx(), c, WriteFileArgs{Site: string(s.Handle), Path: name, Content: "x", BaseSHA: prev})
		require.NoError(t, err)
		prev = w.Commit
	}
	_, out, err := r.listFiles(callCtx(), c, ListFilesArgs{Site: string(s.Handle), Recursive: boolptr(true)})
	require.NoError(t, err)
	assert.True(t, out.Truncated)
	assert.Len(t, out.Files, 1)
}

// ---- small pointer helpers for optional args ----

func strptr(s string) *string { return &s }
func intptr(i int) *int       { return &i }
func boolptr(b bool) *bool    { return &b }
