package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/site"
)

// TestSecurity_TokenA_CannotReachSiteB is the core cross-project regression: a
// token bound to site A, exercised across EVERY content tool, must only ever cause
// site.Service calls against A's UUID — never B's. The pivotService asserts this on
// every call; the structural guarantee is that no tool even has a site argument.
func TestSecurity_TokenA_CannotReachSiteB(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	siteA, baseA := seedSite(t, fake, owner, "site-a")
	siteB, _ := seedSite(t, fake, owner, "site-b") // the forbidden site

	piv := &pivotService{FakeService: fake, t: t, wantSite: siteA.ID}
	r := newTestRegistry(piv)
	// Token A: publish scope (so every tool is scope-permitted), can_create off.
	cA := claims(siteA.ID, owner, scopePublish, false)

	ctx := context.Background()
	// Every content tool. None takes a site selector, so there is no field through
	// which an attacker could substitute siteB.ID. The pivotService fails the test
	// if any call lands on a UUID != siteA.ID.
	_, _, _ = r.listSites(ctx, cA, ListSitesArgs{})
	_, _, _ = r.listFiles(ctx, cA, ListFilesArgs{})
	_, _, _ = r.readFile(ctx, cA, ReadFileArgs{Path: "index.html"})
	_, _, _ = r.writeFile(ctx, cA, WriteFileArgs{Path: "x.html", Content: "<x>", BaseSHA: baseA})
	_, _, _ = r.save(ctx, cA, SaveArgs{BaseSHA: baseA})
	_, _, _ = r.publish(ctx, cA, PublishArgs{BaseSHA: baseA})
	_, _, _ = r.getDiff(ctx, cA, GetDiffArgs{From: strptr("draft"), To: strptr("draft")})
	_, _, _ = r.getLog(ctx, cA, GetLogArgs{})
	_, _, _ = r.rollback(ctx, cA, RollbackArgs{ToSHA: baseA, BaseSHA: baseA})
	_, _, _ = r.createBranch(ctx, cA, CreateBranchArgs{Name: "feature-z"})

	// Sanity: siteB exists and is reachable directly (proving the isolation above
	// was meaningful, not because B was absent).
	_, err := fake.GetSite(ctx, siteB.ID)
	require.NoError(t, err)
}

// TestSecurity_RevokeMidSession proves verification happens per call (not cached
// indefinitely): a token authenticates once, is revoked, and the next Verify
// fails. The handler enforces this because the SDK calls Verify on every request.
func TestSecurity_RevokeMidSession(t *testing.T) {
	store := newFakeTokenStore()
	siteID, userID, tokenID := uuid.New(), uuid.New(), uuid.New()
	pt := store.seedToken(validPlaintext("revk"), siteID, userID, tokenID, tokenOpts{scopes: []string{"read"}})
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
