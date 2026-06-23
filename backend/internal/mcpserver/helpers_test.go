package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// reqFor synthesizes a CallToolRequest carrying the verified principal in
// Extra.TokenInfo, exactly as RequireBearerToken would after a successful Verify.
// This lets unit tests exercise the guard + handlers without the HTTP transport.
func reqFor(c TokenInfo) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{
			TokenInfo: &auth.TokenInfo{
				Scopes: c.Scopes,
				Extra:  map[string]any{claimsKey: c},
			},
		},
	}
}

// fakeTokenStore is an in-memory tokenQuerier for verifier/handler tests. It maps
// plaintext tokens to their stored row so tests model the real prefix→hash flow.
type fakeTokenStore struct {
	mu sync.Mutex
	// rows keyed by the 12-char prefix; the slice models the non-unique-prefix case.
	rows map[string][]gen.GetUserTokenByPrefixRow
	// prefixErr, when set, is returned from GetUserTokenByPrefix (DB-error path).
	prefixErr error
	// touched counts TouchUserToken calls (async last_used_at bump assertions).
	touched int64
	// touchErr makes TouchUserToken fail (must never affect the request).
	touchErr error
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{rows: make(map[string][]gen.GetUserTokenByPrefixRow)}
}

func (f *fakeTokenStore) GetUserTokenByPrefix(_ context.Context, prefix string) ([]gen.GetUserTokenByPrefixRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prefixErr != nil {
		return nil, f.prefixErr
	}
	return f.rows[prefix], nil
}

func (f *fakeTokenStore) TouchUserToken(_ context.Context, _ uuid.UUID) error {
	atomic.AddInt64(&f.touched, 1)
	return f.touchErr
}

// dropAll clears every stored token, modeling instant revocation (the real query
// excludes revoked/expired rows, so a revoked token simply stops being returned).
func (f *fakeTokenStore) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = make(map[string][]gen.GetUserTokenByPrefixRow)
}

// tokenOpts configures a seeded token. Zero value = active, non-revoked.
type tokenOpts struct {
	scopes        []string
	expiresAt     *time.Time
	canCreateSites bool // token-level flag
	userCanCreate  bool // owning-user flag (effective = AND of both)
	userInactive   bool // when true the row is NOT returned (models the DB filter)
	revoked        bool // when true the row is NOT returned (models the DB filter)
}

// seedToken creates a token with the given plaintext, owner and options, storing
// its sha256 hash + prefix exactly as production does. A token is now per-USER (no
// site binding). Returns the plaintext.
func (f *fakeTokenStore) seedToken(plaintext string, userID, tokenID uuid.UUID, o tokenOpts) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The real GetUserTokenByPrefix query excludes revoked tokens AND tokens whose
	// owning user is inactive; model both by NOT storing the row in those cases.
	if o.revoked || o.userInactive {
		return plaintext
	}
	sum := sha256.Sum256([]byte(plaintext))
	row := gen.GetUserTokenByPrefixRow{
		ID:                 tokenID,
		UserID:             userID,
		Name:               "test",
		TokenPrefix:        plaintext[:prefixLen],
		TokenHash:          sum[:],
		Scopes:             o.scopes,
		CanCreateSites:     o.canCreateSites,
		ExpiresAt:          timestamptz(o.expiresAt),
		UserActive:         true,
		UserCanCreateSites: o.userCanCreate,
	}
	pfx := plaintext[:prefixLen]
	f.rows[pfx] = append(f.rows[pfx], row)
	return plaintext
}

// ---- handler-test scaffolding ----

// scopeChain returns the full subset chain for a top scope (read ⊂ write ⊂
// publish), matching issuance-time semantics.
func scopeChain(top scope) []string {
	switch top {
	case scopePublish:
		return []string{"read", "write", "publish"}
	case scopeWrite:
		return []string{"read", "write"}
	default:
		return []string{"read"}
	}
}

// fakeMembers is an in-memory membershipQuerier for handler/authz tests: per-site
// roles, a membership list per user, and a user table for the create-site gate.
type fakeMembers struct {
	mu sync.Mutex
	// roles keyed by (siteID|userID) -> role; absence => no membership (pgx.ErrNoRows).
	roles map[string]gen.SiteRole
	// siteRows keyed by siteID -> the row template used to build a list result.
	siteRows map[uuid.UUID]gen.ListSitesForUserRow
	// users by id (account flags for create_site).
	users map[uuid.UUID]gen.User
	// roleErr, when set, is returned from GetRole (infra-error path).
	roleErr error
}

func newFakeMembers() *fakeMembers {
	return &fakeMembers{
		roles:    map[string]gen.SiteRole{},
		siteRows: map[uuid.UUID]gen.ListSitesForUserRow{},
		users:    map[uuid.UUID]gen.User{},
	}
}

func memberKey(siteID, userID uuid.UUID) string { return siteID.String() + "|" + userID.String() }

// grant records a user's role on a site (and a site row for list_sites). The site
// metadata (handle, default branch) is taken from the FakeService site so URLs and
// the list result line up with the seeded site.
func (m *fakeMembers) grant(s site.Site, userID uuid.UUID, role gen.SiteRole) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roles[memberKey(s.ID, userID)] = role
	m.siteRows[s.ID] = gen.ListSitesForUserRow{
		ID:            s.ID,
		Handle:        string(s.Handle),
		OwnerID:       s.OwnerID,
		DefaultBranch: string(s.DefaultBranch),
		UpdatedAt:     pgtype.Timestamptz{Time: s.UpdatedAt, Valid: true},
	}
}

// addUser registers a user's account flags (for the create_site gate).
func (m *fakeMembers) addUser(u gen.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[u.ID] = u
}

func (m *fakeMembers) GetRole(_ context.Context, arg gen.GetRoleParams) (gen.SiteRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.roleErr != nil {
		return "", m.roleErr
	}
	role, ok := m.roles[memberKey(arg.SiteID, arg.UserID)]
	if !ok {
		return "", pgx.ErrNoRows
	}
	return role, nil
}

func (m *fakeMembers) ListSitesForUser(_ context.Context, arg gen.ListSitesForUserParams) ([]gen.ListSitesForUserRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []gen.ListSitesForUserRow{}
	for key, role := range m.roles {
		// key is "siteID|userID"; only this user's memberships.
		var sid, uid uuid.UUID
		parts := strings.SplitN(key, "|", 2)
		sid, _ = uuid.Parse(parts[0])
		uid, _ = uuid.Parse(parts[1])
		if uid != arg.UserID {
			continue
		}
		row := m.siteRows[sid]
		row.Role = role
		out = append(out, row)
	}
	return out, nil
}

func (m *fakeMembers) GetUserByID(_ context.Context, id uuid.UUID) (gen.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return gen.User{}, pgx.ErrNoRows
	}
	return u, nil
}

// newTestRegistry builds a registry over a FakeService for direct handler tests
// (bypassing the SDK transport). Limits use a permissive deterministic limiter
// unless overridden. A fresh fakeMembers is attached; tests grant roles via it.
func newTestRegistry(svc site.Service) *registry {
	return &registry{
		svc:     svc,
		members: newFakeMembers(),
		limits:  DefaultLimits(),
		log:     nil,
		cfg:     Deps{BaseDomain: "hosting.test", Scheme: "https"},
	}
}

// fakeMembersOf returns the registry's membership fake (set by newTestRegistry).
func fakeMembersOf(r *registry) *fakeMembers { return r.members.(*fakeMembers) }

// claims builds a TokenInfo principal for userID with the given scope chain. A
// token is per-USER now (no site binding); the site is supplied per call.
func claims(userID uuid.UUID, top scope, canCreate bool) TokenInfo {
	return TokenInfo{
		UserID:         userID,
		TokenID:        uuid.New(),
		Scopes:         scopeChain(top),
		CanCreateSites: canCreate,
	}
}

// seedSite creates a site in the FakeService and returns it plus the draft tip SHA.
func seedSite(t testingTB, svc *site.FakeService, owner uuid.UUID, handle string) (site.Site, string) {
	t.Helper()
	s, err := svc.CreateSite(context.Background(), site.CreateSiteInput{
		Handle:  site.Handle(handle),
		OwnerID: owner,
		Actor:   site.Actor{UserID: owner, Via: site.SourceEditor},
	})
	if err != nil {
		t.Fatalf("seedSite: %v", err)
	}
	files, ref, err := svc.ListFiles(context.Background(), site.ListFilesInput{SiteID: s.ID, Branch: site.BranchDraft})
	if err != nil {
		t.Fatalf("seedSite list: %v", err)
	}
	_ = files
	return s, ref.SHA
}

// seedSiteR creates a site in the registry's FakeService AND grants `owner` the
// owner role on it (plus registers the owner as a create-capable user), so the
// membership-cap authz lets the owner act on it. Returns the site + draft tip SHA.
// This is the per-USER-token replacement for the old "token == site" seeding: a
// content tool call must now BOTH name the site AND have a membership on it.
func seedSiteR(t testingTB, r *registry, owner uuid.UUID, handle string) (site.Site, string) {
	t.Helper()
	fake := r.svc.(*site.FakeService)
	s, base := seedSite(t, fake, owner, handle)
	m := fakeMembersOf(r)
	m.grant(s, owner, gen.SiteRoleOwner)
	m.addUser(gen.User{ID: owner, CanCreateSites: true, IsActive: true})
	return s, base
}

// testingTB is the subset of testing.TB the helpers need (keeps the helper file
// importable without pulling the whole testing surface into signatures).
type testingTB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// decodeErrBody extracts the structured error body from an IsError result. It
// accepts the toolError value stored in CallToolResult.StructuredContent (or any
// shape that JSON-round-trips into one).
func decodeErrBody(t testingTB, structured any) toolErrorBody {
	t.Helper()
	if te, ok := structured.(toolError); ok {
		return te.Error
	}
	raw, _ := json.Marshal(structured)
	var te toolError
	if err := json.Unmarshal(raw, &te); err != nil {
		t.Fatalf("decodeErrBody: not a toolError: %v", err)
	}
	return te.Error
}
