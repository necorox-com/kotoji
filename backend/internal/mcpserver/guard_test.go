package mcpserver

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/necorox-com/kotoji/backend/internal/site"
)

// toolSpec is the static catalogue used by the matrix + pivot tests: tool name,
// the scope it requires, its rate class, and a zero-value arg struct for
// reflection. It mirrors registerAll exactly.
type toolSpec struct {
	name     string
	required scope
	class    toolClass
	argType  reflect.Type
}

func toolSpecs() []toolSpec {
	return []toolSpec{
		{"list_sites", scopeRead, classRead, reflect.TypeOf(ListSitesArgs{})},
		{"list_files", scopeRead, classRead, reflect.TypeOf(ListFilesArgs{})},
		{"read_file", scopeRead, classRead, reflect.TypeOf(ReadFileArgs{})},
		{"write_file", scopeWrite, classWrite, reflect.TypeOf(WriteFileArgs{})},
		{"create_site", scopeWrite, classCreate, reflect.TypeOf(CreateSiteArgs{})},
		{"save", scopeWrite, classWrite, reflect.TypeOf(SaveArgs{})},
		{"publish", scopePublish, classPublish, reflect.TypeOf(PublishArgs{})},
		{"get_diff", scopeRead, classRead, reflect.TypeOf(GetDiffArgs{})},
		{"get_log", scopeRead, classRead, reflect.TypeOf(GetLogArgs{})},
		{"rollback", scopeWrite, classWrite, reflect.TypeOf(RollbackArgs{})},
		{"create_branch", scopeWrite, classWrite, reflect.TypeOf(CreateBranchArgs{})},
	}
}

// ---- PIVOT TEST: no tool arg struct may carry a site selector ----

func TestPivot_NoToolTakesSiteSelector(t *testing.T) {
	// Any field whose name or json tag hints at a cross-project selector breaks the
	// "one token = one site" wall (mcp.md §4.1 / CANONICAL §6). create_site's
	// `handle` is the ONE allowed identifier because it mints a NEW site (and is
	// capability-gated), not a selector over existing sites.
	banned := []string{"site", "siteid", "site_id", "uuid", "owner", "ownerid"}

	for _, spec := range toolSpecs() {
		for i := 0; i < spec.argType.NumField(); i++ {
			f := spec.argType.Field(i)
			jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
			lowerField := strings.ToLower(f.Name)
			lowerJSON := strings.ToLower(jsonName)
			for _, b := range banned {
				assert.NotEqual(t, b, lowerField, "%s.%s is a forbidden site selector", spec.name, f.Name)
				assert.NotEqual(t, b, lowerJSON, "%s json:%q is a forbidden site selector", spec.name, jsonName)
			}
			// "handle" is allowed ONLY on create_site.
			if lowerField == "handle" || lowerJSON == "handle" {
				assert.Equal(t, "create_site", spec.name, "only create_site may take a handle")
			}
		}
	}
}

// pivotService wraps a FakeService and asserts every git-touching call is pinned
// to the expected site UUID. Any call with a different site fails the test.
type pivotService struct {
	*site.FakeService
	t        *testing.T
	wantSite uuid.UUID
}

func (p *pivotService) check(id uuid.UUID) {
	p.t.Helper()
	require.Equal(p.t, p.wantSite, id, "site.Service called with a site OTHER than the pinned token site")
}

func (p *pivotService) GetSite(ctx context.Context, id uuid.UUID) (site.Site, error) {
	p.check(id)
	return p.FakeService.GetSite(ctx, id)
}
func (p *pivotService) ListFiles(ctx context.Context, in site.ListFilesInput) ([]site.FileEntry, site.ResolvedRef, error) {
	p.check(in.SiteID)
	return p.FakeService.ListFiles(ctx, in)
}
func (p *pivotService) ReadFile(ctx context.Context, id uuid.UUID, b site.BranchName, ref, path string) (site.FileContent, error) {
	p.check(id)
	return p.FakeService.ReadFile(ctx, id, b, ref, path)
}
func (p *pivotService) WriteFile(ctx context.Context, in site.WriteFileInput) (site.CommitInfo, error) {
	p.check(in.SiteID)
	return p.FakeService.WriteFile(ctx, in)
}
func (p *pivotService) Commit(ctx context.Context, in site.CommitInput) (site.CommitInfo, error) {
	p.check(in.SiteID)
	return p.FakeService.Commit(ctx, in)
}
func (p *pivotService) Publish(ctx context.Context, in site.PublishInput) (site.CommitInfo, error) {
	p.check(in.SiteID)
	return p.FakeService.Publish(ctx, in)
}
func (p *pivotService) Rollback(ctx context.Context, id uuid.UUID, b site.BranchName, to, base string, a site.Actor) (site.CommitInfo, error) {
	p.check(id)
	return p.FakeService.Rollback(ctx, id, b, to, base, a)
}
func (p *pivotService) GetDiff(ctx context.Context, in site.DiffOptions) (site.DiffResult, error) {
	p.check(in.SiteID)
	return p.FakeService.GetDiff(ctx, in)
}
func (p *pivotService) GetLog(ctx context.Context, in site.LogOptions) ([]site.CommitInfo, error) {
	p.check(in.SiteID)
	return p.FakeService.GetLog(ctx, in)
}
func (p *pivotService) CreateBranch(ctx context.Context, id uuid.UUID, n site.BranchName, from string) (site.Branch, error) {
	p.check(id)
	return p.FakeService.CreateBranch(ctx, id, n, from)
}

func TestPivot_ServiceAlwaysCalledWithClaimsSiteID(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "pinned-site")
	piv := &pivotService{FakeService: fake, t: t, wantSite: s.ID}
	r := newTestRegistry(piv)
	c := claims(s.ID, owner, scopePublish, false)

	// Exercise read + write + history tools; the pivotService asserts the UUID.
	_, _, err := guard(r, scopeRead, classRead, r.listSites)(context.Background(), reqFor(c), ListSitesArgs{})
	require.NoError(t, err)
	_, _, err = guard(r, scopeRead, classRead, r.readFile)(context.Background(), reqFor(c), ReadFileArgs{Path: "index.html"})
	require.NoError(t, err)
	_, _, err = guard(r, scopeWrite, classWrite, r.writeFile)(context.Background(), reqFor(c), WriteFileArgs{Path: "a.html", Content: "<x>", BaseSHA: base})
	require.NoError(t, err)
	_, _, err = guard(r, scopeRead, classRead, r.getLog)(context.Background(), reqFor(c), GetLogArgs{})
	require.NoError(t, err)
}

// ---- SCOPE × TOOL MATRIX ----

func TestScopeMatrix(t *testing.T) {
	tokenScopes := []scope{scopeRead, scopeWrite, scopePublish}

	// allowed[tool][tokenScope] = should the scope gate permit the call?
	for _, spec := range toolSpecs() {
		for _, tok := range tokenScopes {
			spec, tok := spec, tok
			t.Run(spec.name+"/"+string(tok), func(t *testing.T) {
				want := scopeRank(tok) >= scopeRank(spec.required)
				got := hasScope(scopeChain(tok), spec.required)
				assert.Equal(t, want, got, "tool %q (needs %q) with %q token", spec.name, spec.required, tok)
			})
		}
	}
}

// scopeRank orders the chain read < write < publish for the matrix expectation.
func scopeRank(s scope) int {
	switch s {
	case scopePublish:
		return 3
	case scopeWrite:
		return 2
	default:
		return 1
	}
}

func TestGuard_ReadTokenForbiddenOnWriteTools(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, base := seedSite(t, fake, owner, "scope-site")
	r := newTestRegistry(fake)
	readClaims := claims(s.ID, owner, scopeRead, true) // read-only token

	cases := []struct {
		name string
		call func() (*mcp.CallToolResult, error)
	}{
		{"write_file", func() (*mcp.CallToolResult, error) {
			res, _, err := guard(r, scopeWrite, classWrite, r.writeFile)(context.Background(), reqFor(readClaims), WriteFileArgs{Path: "a.html", Content: "x", BaseSHA: base})
			return res, err
		}},
		{"publish", func() (*mcp.CallToolResult, error) {
			res, _, err := guard(r, scopePublish, classPublish, r.publish)(context.Background(), reqFor(readClaims), PublishArgs{BaseSHA: base})
			return res, err
		}},
		{"rollback", func() (*mcp.CallToolResult, error) {
			res, _, err := guard(r, scopeWrite, classWrite, r.rollback)(context.Background(), reqFor(readClaims), RollbackArgs{ToSHA: base, BaseSHA: base})
			return res, err
		}},
		{"create_site", func() (*mcp.CallToolResult, error) {
			res, _, err := guard(r, scopeWrite, classCreate, r.createSite)(context.Background(), reqFor(readClaims), CreateSiteArgs{Handle: "new-one"})
			return res, err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.call()
			require.NoError(t, err, "scope rejection is a tool error, not a Go error")
			require.NotNil(t, res)
			assert.True(t, res.IsError)
			assert.Equal(t, codeForbidden, decodeErrBody(t, res.StructuredContent).Code)
		})
	}
}

func TestGuard_MissingPrincipal_Unauthenticated(t *testing.T) {
	fake := site.NewFakeService()
	r := newTestRegistry(fake)
	// A request with no Extra.TokenInfo (should never happen behind the bearer
	// middleware) must fail closed to unauthenticated.
	res, _, err := guard(r, scopeRead, classRead, r.listSites)(context.Background(), &mcp.CallToolRequest{}, ListSitesArgs{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	assert.Equal(t, codeUnauthenticated, decodeErrBody(t, res.StructuredContent).Code)
}

func TestGuard_RateLimited(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	s, _ := seedSite(t, fake, owner, "rl-site")
	r := newTestRegistry(fake)
	r.limits.Limiter = denyLimiter{}
	c := claims(s.ID, owner, scopeRead, false)

	res, _, err := guard(r, scopeRead, classRead, r.listSites)(context.Background(), reqFor(c), ListSitesArgs{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	body := decodeErrBody(t, res.StructuredContent)
	assert.Equal(t, codeRateLimited, body.Code)
	assert.True(t, body.Retryable)
	assert.NotNil(t, body.Details["retry_after"])
}

// denyLimiter always denies with a fixed backoff (deterministic rate tests).
type denyLimiter struct{}

func (denyLimiter) Allow(uuid.UUID, toolClass) (bool, time.Duration) { return false, 5 * time.Second }
