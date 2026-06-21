package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
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
	rows map[string][]gen.GetTokenByPrefixRow
	// prefixErr, when set, is returned from GetTokenByPrefix (DB-error path).
	prefixErr error
	// touched counts TouchToken calls (async last_used_at bump assertions).
	touched int64
	// touchErr makes TouchToken fail (must never affect the request).
	touchErr error
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{rows: make(map[string][]gen.GetTokenByPrefixRow)}
}

func (f *fakeTokenStore) GetTokenByPrefix(_ context.Context, prefix string) ([]gen.GetTokenByPrefixRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prefixErr != nil {
		return nil, f.prefixErr
	}
	return f.rows[prefix], nil
}

func (f *fakeTokenStore) TouchToken(_ context.Context, _ uuid.UUID) error {
	atomic.AddInt64(&f.touched, 1)
	return f.touchErr
}

// dropAll clears every stored token, modeling instant revocation (the real query
// excludes revoked/expired rows, so a revoked token simply stops being returned).
func (f *fakeTokenStore) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = make(map[string][]gen.GetTokenByPrefixRow)
}

// tokenOpts configures a seeded token. Zero value = active, non-revoked.
type tokenOpts struct {
	scopes           []string
	expiresAt        *time.Time
	canCreateSites   bool // token-level flag
	creatorCanCreate bool // creating-user flag (effective = AND of both)
	creatorInactive  bool // when true the row is NOT returned (models the DB filter)
	revoked          bool // when true the row is NOT returned (models the DB filter)
}

// seedToken creates a token with the given plaintext, site and options, storing
// its sha256 hash + prefix exactly as production does. Returns the plaintext.
func (f *fakeTokenStore) seedToken(plaintext string, siteID, userID, tokenID uuid.UUID, o tokenOpts) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The real GetTokenByPrefix query excludes revoked tokens AND tokens whose
	// creating user is inactive; model both by NOT storing the row in those cases.
	if o.revoked || o.creatorInactive {
		return plaintext
	}
	sum := sha256.Sum256([]byte(plaintext))
	row := gen.GetTokenByPrefixRow{
		ID:                    tokenID,
		SiteID:                siteID,
		CreatedBy:             userID,
		Name:                  "test",
		TokenPrefix:           plaintext[:prefixLen],
		TokenHash:             sum[:],
		Scopes:                o.scopes,
		CanCreateSites:        o.canCreateSites,
		ExpiresAt:             timestamptz(o.expiresAt),
		CreatorActive:         true,
		CreatorCanCreateSites: o.creatorCanCreate,
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

// newTestRegistry builds a registry over a FakeService for direct handler tests
// (bypassing the SDK transport). Limits use a permissive deterministic limiter
// unless overridden.
func newTestRegistry(svc site.Service) *registry {
	return &registry{
		svc:    svc,
		limits: DefaultLimits(),
		log:    nil,
		cfg:    Deps{BaseDomain: "hosting.test", Scheme: "https"},
	}
}

// claims builds a TokenInfo principal pinned to siteID with the given scope chain.
func claims(siteID, userID uuid.UUID, top scope, canCreate bool) TokenInfo {
	return TokenInfo{
		SiteID:         siteID,
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
