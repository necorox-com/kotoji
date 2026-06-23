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

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// toolSpec is the static catalogue used by the matrix + selector tests: tool name,
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

// ---- SELECTOR TEST: content tools MUST carry a `site` selector ----

// In the membership-capped model EVERY content tool takes a `site` (handle); the
// site is authorized per call against the token user's membership (authz.go). This
// is the inverse of the old "no tool takes a site selector" pivot guarantee, which
// was replaced. list_sites enumerates memberships (no selector); create_site mints
// a NEW site via `handle` (not a selector over existing sites).
func TestContentToolsCarrySiteSelector(t *testing.T) {
	noSelector := map[string]bool{"list_sites": true, "create_site": true}

	for _, spec := range toolSpecs() {
		hasSite := false
		hasHandle := false
		for i := 0; i < spec.argType.NumField(); i++ {
			f := spec.argType.Field(i)
			jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
			switch strings.ToLower(jsonName) {
			case "site":
				hasSite = true
			case "handle":
				hasHandle = true
			}
		}
		if noSelector[spec.name] {
			assert.False(t, hasSite, "%s must NOT carry a site selector", spec.name)
			if spec.name == "create_site" {
				assert.True(t, hasHandle, "create_site mints a NEW site via handle")
			}
			continue
		}
		assert.True(t, hasSite, "%s must carry a `site` selector (membership-capped)", spec.name)
	}
}

// TestGuard_PerSiteAuthorization exercises the guard + authz path end-to-end for a
// member: read + write + history tools all succeed when the user has the role.
func TestGuard_PerSiteAuthorization(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "pinned-site")
	c := claims(owner, scopePublish, false)
	h := string(s.Handle)

	_, _, err := guard(r, classRead, r.listSites)(context.Background(), reqFor(c), ListSitesArgs{})
	require.NoError(t, err)
	_, _, err = guard(r, classRead, r.readFile)(context.Background(), reqFor(c), ReadFileArgs{Site: h, Path: "index.html"})
	require.NoError(t, err)
	_, _, err = guard(r, classWrite, r.writeFile)(context.Background(), reqFor(c), WriteFileArgs{Site: h, Path: "a.html", Content: "<x>", BaseSHA: base})
	require.NoError(t, err)
	_, _, err = guard(r, classRead, r.getLog)(context.Background(), reqFor(c), GetLogArgs{Site: h})
	require.NoError(t, err)
}

// ---- SCOPE × TOOL MATRIX (token scope only; per-site role caps separately) ----

func TestScopeMatrix(t *testing.T) {
	tokenScopes := []scope{scopeRead, scopeWrite, scopePublish}

	// allowed[tool][tokenScope] = should the token's scope chain include the tool's
	// required scope? (The membership role intersection is tested separately.)
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

// TestAuthzMatrix is the MEMBERSHIP-CAPPED authorization matrix: for each
// (token scope × membership role) the effective scope is the intersection, and a
// tool requiring a scope outside the intersection is forbidden. This replaces the
// old global-scope-only gate.
func TestAuthzMatrix(t *testing.T) {
	roles := []gen.SiteRole{gen.SiteRoleOwner, gen.SiteRoleEditor, gen.SiteRoleViewer}
	tokenScopes := []scope{scopeRead, scopeWrite, scopePublish}
	needs := []scope{scopeRead, scopeWrite, scopePublish}

	for _, role := range roles {
		for _, tok := range tokenScopes {
			for _, need := range needs {
				role, tok, need := role, tok, need
				t.Run(string(role)+"/"+string(tok)+"/need-"+string(need), func(t *testing.T) {
					eff := intersectScopes(scopeChain(tok), role)
					// want = need is granted by BOTH the token chain AND the role.
					tokenGrants := scopeRank(tok) >= scopeRank(need)
					var roleTop scope = scopeRead
					if role == gen.SiteRoleOwner || role == gen.SiteRoleEditor {
						roleTop = scopePublish
					}
					roleGrants := scopeRank(roleTop) >= scopeRank(need)
					want := tokenGrants && roleGrants

					got := false
					for _, sc := range eff {
						if sc == need {
							got = true
						}
					}
					assert.Equal(t, want, got, "role=%s token=%s need=%s", role, tok, need)
				})
			}
		}
	}
}

// TestGuard_WriteTokenViewerRole_WriteDeniedReadOk is the headline authz case from
// the task: token(write) + the user is a VIEWER on the site -> write denied, read
// ok. The effective scope is token(read,write) ∩ viewer(read) = {read}.
func TestGuard_WriteTokenViewerRole_WriteDeniedReadOk(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	// Seed the site (as some owner) and then make the caller a VIEWER on it.
	s, base := seedSite(t, fake, owner, "viewer-site")
	caller := uuid.New()
	fakeMembersOf(r).grant(s, caller, gen.SiteRoleViewer)
	c := claims(caller, scopeWrite, false) // write-capable token, viewer role
	h := string(s.Handle)

	// read_file is permitted (read ∈ effective {read}).
	_, _, rerr := r.readFile(context.Background(), c, ReadFileArgs{Site: h, Path: "index.html"})
	require.NoError(t, rerr)

	// write_file is forbidden (write ∉ effective {read}).
	res, _, werr := r.writeFile(context.Background(), c, WriteFileArgs{Site: h, Path: "a.html", Content: "x", BaseSHA: base})
	require.NoError(t, werr)
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	body := decodeErrBody(t, res.StructuredContent)
	assert.Equal(t, codeForbidden, body.Code)
	assert.Equal(t, "viewer", body.Details["role"])
}

func TestGuard_ReadTokenForbiddenOnWriteTools(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	s, base := seedSiteR(t, r, owner, "scope-site")
	readClaims := claims(owner, scopeRead, true) // read-only token (owner role)
	h := string(s.Handle)

	cases := []struct {
		name string
		call func() (*mcp.CallToolResult, error)
	}{
		{"write_file", func() (*mcp.CallToolResult, error) {
			res, _, err := r.writeFile(context.Background(), readClaims, WriteFileArgs{Site: h, Path: "a.html", Content: "x", BaseSHA: base})
			return res, err
		}},
		{"publish", func() (*mcp.CallToolResult, error) {
			res, _, err := r.publish(context.Background(), readClaims, PublishArgs{Site: h, BaseSHA: base})
			return res, err
		}},
		{"rollback", func() (*mcp.CallToolResult, error) {
			res, _, err := r.rollback(context.Background(), readClaims, RollbackArgs{Site: h, ToSHA: base, BaseSHA: base})
			return res, err
		}},
		// NOTE: create_site is NOT a write-scope gate — it is gated by
		// can_create_sites (token AND user), tested separately in tools_test.go.
		// A read token that holds can_create_sites can legitimately create a site.
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
	res, _, err := guard(r, classRead, r.listSites)(context.Background(), &mcp.CallToolRequest{}, ListSitesArgs{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	assert.Equal(t, codeUnauthenticated, decodeErrBody(t, res.StructuredContent).Code)
}

func TestGuard_RateLimited(t *testing.T) {
	fake := site.NewFakeService()
	owner := uuid.New()
	r := newTestRegistry(fake)
	seedSiteR(t, r, owner, "rl-site")
	r.limits.Limiter = denyLimiter{}
	c := claims(owner, scopeRead, false)

	res, _, err := guard(r, classRead, r.listSites)(context.Background(), reqFor(c), ListSitesArgs{})
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
