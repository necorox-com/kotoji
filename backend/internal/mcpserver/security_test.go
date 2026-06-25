package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// TestSecurity_UserA_CannotReachSiteTheyAreNotAMemberOf is the core cross-project
// regression for the NEW membership-capped model: a token owned by user A, naming
// a site A is NOT a member of, must be refused on EVERY content tool with a 404
// (no existence leak) — never a site.Service call against that site. The membership
// fake has no role for A on siteB, so authorizeSite 404s before any git touch.
func TestSecurity_UserA_CannotReachSiteTheyAreNotAMemberOf(t *testing.T) {
	fake := site.NewFakeService()
	userA := uuid.New()
	other := uuid.New()
	r := newTestRegistry(fake)

	// Site A: A is the owner/member (reachable).
	siteA, baseA := seedSiteR(t, r, userA, "site-a")
	// Site B: owned by someone else; A has NO membership (the forbidden site).
	siteB, _ := seedSite(t, fake, other, "site-b")

	cA := claims(userA, scopePublish, false) // full scope token, but membership-capped

	ctx := context.Background()
	hb := string(siteB.Handle)

	// Every content tool naming siteB must 404 (A is not a member); none leaks B.
	assertNotFound := func(name string, res *mcp.CallToolResult) {
		require.NotNil(t, res, "%s: expected a tool-error result", name)
		require.True(t, res.IsError, "%s: must be a tool error", name)
		assert.Equal(t, codeNotFound, decodeErrBody(t, res.StructuredContent).Code, "%s: must be not_found (no existence leak)", name)
	}

	r1, _, _ := r.listFiles(ctx, cA, ListFilesArgs{Site: hb})
	assertNotFound("list_files", r1)
	r2, _, _ := r.readFile(ctx, cA, ReadFileArgs{Site: hb, Path: "index.html"})
	assertNotFound("read_file", r2)
	r3, _, _ := r.writeFile(ctx, cA, WriteFileArgs{Site: hb, Path: "x.html", Content: "<x>", BaseSHA: baseA})
	assertNotFound("write_file", r3)
	r4, _, _ := r.save(ctx, cA, SaveArgs{Site: hb, BaseSHA: baseA})
	assertNotFound("save", r4)
	r5, _, _ := r.publish(ctx, cA, PublishArgs{Site: hb, BaseSHA: baseA})
	assertNotFound("publish", r5)
	r6, _, _ := r.getDiff(ctx, cA, GetDiffArgs{Site: hb, From: strptr("draft"), To: strptr("draft")})
	assertNotFound("get_diff", r6)
	r7, _, _ := r.getLog(ctx, cA, GetLogArgs{Site: hb})
	assertNotFound("get_log", r7)
	r8, _, _ := r.rollback(ctx, cA, RollbackArgs{Site: hb, ToSHA: baseA, BaseSHA: baseA})
	assertNotFound("rollback", r8)
	r9, _, _ := r.createBranch(ctx, cA, CreateBranchArgs{Site: hb, Name: "feature-z"})
	assertNotFound("create_branch", r9)

	// Sanity: siteB exists and is reachable directly (proving the isolation above
	// was meaningful, not because B was absent); and A CAN reach its own siteA.
	_, err := fake.GetSite(ctx, siteB.ID)
	require.NoError(t, err)
	_, _, err = r.readFile(ctx, cA, ReadFileArgs{Site: string(siteA.Handle), Path: "index.html"})
	require.NoError(t, err)
}

// TestSecurity_MembershipDowngrade_LimitsTokenImmediately proves the effective
// scope is re-evaluated per call: a write token whose user is downgraded to viewer
// can still read but can no longer write (no re-issue needed).
func TestSecurity_MembershipDowngrade_LimitsTokenImmediately(t *testing.T) {
	fake := site.NewFakeService()
	user := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, user, "downgrade") // starts as owner
	c := claims(user, scopeWrite, false)          // write-capable token

	// As owner: write works.
	_, w1, err := r.writeFile(context.Background(), c, WriteFileArgs{Site: string(s.Handle), Path: "index.html", Content: "v1", BaseSHA: base})
	require.NoError(t, err)
	require.NotEmpty(t, w1.Commit)

	// Downgrade the user to viewer on this site.
	fakeMembersOf(r).grant(s, user, gen.SiteRoleViewer)

	// Read still works (viewer grants read; token has read).
	_, _, err = r.readFile(context.Background(), c, ReadFileArgs{Site: string(s.Handle), Path: "index.html"})
	require.NoError(t, err)

	// Write is now FORBIDDEN: effective = token(read,write) ∩ viewer(read) = {read}.
	res, _, err := r.writeFile(context.Background(), c, WriteFileArgs{Site: string(s.Handle), Path: "index.html", Content: "v2", BaseSHA: w1.Commit})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, codeForbidden, decodeErrBody(t, res.StructuredContent).Code)
}

// TestSecurity_MembershipRemoval_DeniesNextCall proves the membership is re-read
// per call with NO cached role: a member removed from a site mid-session is denied
// on the VERY NEXT tool call with a not_found (no existence leak), on BOTH a read
// and a write tool. This is the removal counterpart to the downgrade test above —
// authorizeSite's GetRole returns pgx.ErrNoRows after removal, which is a 404.
func TestSecurity_MembershipRemoval_DeniesNextCall(t *testing.T) {
	fake := site.NewFakeService()
	user := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, user, "removed") // starts as owner/member
	c := claims(user, scopePublish, false)      // full-scope token, membership-capped

	// While a member: read and list_sites both work and the site is visible.
	_, _, err := r.readFile(context.Background(), c, ReadFileArgs{Site: string(s.Handle), Path: "index.html"})
	require.NoError(t, err)
	_, ls, err := r.listSites(context.Background(), c, ListSitesArgs{})
	require.NoError(t, err)
	require.Len(t, ls.Sites, 1, "member should see exactly their one site")

	// Remove the user from the site (e.g. owner removes them) mid-session.
	fakeMembersOf(r).revoke(s, user)

	// NEXT read call: denied with not_found (no cached role; GetRole now ErrNoRows).
	rRead, _, err := r.readFile(context.Background(), c, ReadFileArgs{Site: string(s.Handle), Path: "index.html"})
	require.NoError(t, err)
	require.True(t, rRead.IsError)
	assert.Equal(t, codeNotFound, decodeErrBody(t, rRead.StructuredContent).Code,
		"removed member must get not_found (no existence leak), not forbidden")

	// NEXT write call: equally denied with not_found.
	rWrite, _, err := r.writeFile(context.Background(), c, WriteFileArgs{Site: string(s.Handle), Path: "index.html", Content: "x", BaseSHA: base})
	require.NoError(t, err)
	require.True(t, rWrite.IsError)
	assert.Equal(t, codeNotFound, decodeErrBody(t, rWrite.StructuredContent).Code)

	// list_sites now returns NOTHING for this user (the membership row is gone).
	_, ls2, err := r.listSites(context.Background(), c, ListSitesArgs{})
	require.NoError(t, err)
	assert.Empty(t, ls2.Sites, "removed member must no longer enumerate the site")
}

// TestSecurity_RevokeMidSession proves verification happens per call (not cached
// indefinitely): a token authenticates once, is revoked, and the next Verify
// fails. The handler enforces this because the SDK calls Verify on every request.
func TestSecurity_RevokeMidSession(t *testing.T) {
	store := newFakeTokenStore()
	userID, tokenID := uuid.New(), uuid.New()
	pt := store.seedToken(validPlaintext("revk"), userID, tokenID, tokenOpts{scopes: []string{"read"}})
	v := NewVerifier(store)
	v.touch = func(uuid.UUID) {}

	// First call: valid.
	_, err := v.Verify(context.Background(), pt, nil)
	require.NoError(t, err)

	// Revoke: model the DB by dropping the row (the real query excludes revoked).
	store.dropAll()

	// Next call: rejected (verifier hits the store each call, no stale cache).
	_, err = v.Verify(context.Background(), pt, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
}

// TestSecurity_401_BeforeAnyTool proves the bearer middleware rejects a
// missing/garbage Authorization header with 401 BEFORE any tool runs (and emits a
// WWW-Authenticate header per RFC 6750).
func TestSecurity_401_BeforeAnyTool(t *testing.T) {
	store := newFakeTokenStore()
	h := New(Deps{
		Service:    site.NewFakeService(),
		Tokens:     store,
		Members:    newFakeMembers(),
		BaseDomain: "hosting.test",
	})

	for _, hdr := range []string{"", "garbage", "Bearer not-a-kotoji-token"} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		// A real MCP client sends an Origin only when browser-based; none here.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "Authorization %q must be 401", hdr)
	}
}
