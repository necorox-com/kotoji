// Package mcpserver is the kotoji control-plane MCP server: a thin, authenticated
// adapter over site.Service that owns NO git logic. It mounts at /mcp on the
// control plane only (never the data plane) and speaks Streamable HTTP via the
// official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// The headline security property (CANONICAL §6, mcp.md §4): a token is OWNED BY A
// USER, carries one scope set, and automatically covers every project the user is
// a member of. Content tools DO take a site selector (a handle), but every call
// is MEMBERSHIP-CAPPED: the site is resolved, the user's membership role is read,
// and the EFFECTIVE scope is intersection(token.scopes, roleScopes(role)) — so a
// token can only ever act within its user's own memberships and never exceed the
// user's access. Non-membership 404s (no existence leak). This membership-capped
// authorization REPLACES the old "no site selector / one token = one site" pin.
// Scope enforcement (read ⊂ write ⊂ publish), the role intersection, and in-site
// path/extension confinement all live in the registry guard (registry.go).
package mcpserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// tokenPrefix is the greppable plaintext prefix every kotoji PAT carries. It lets
// the verifier fast-reject anything that is not ours BEFORE any DB hit, and lets
// us scan logs/repos for leaked tokens (CANONICAL §8, mcp.md §3.1).
const tokenPrefix = "kotoji_pat_"

// prefixLen is the number of leading plaintext chars stored in user_tokens.token_prefix
// (DB CHECK enforces exactly 12). Used to narrow the indexed prefix lookup.
const prefixLen = 12

// noExpiry is the far-future Expiration we hand the SDK for never-expiring tokens.
// The SDK's RequireBearerToken rejects a zero Expiration ("token missing
// expiration"); our tokens may legitimately have a NULL expires_at, so we map
// NULL to this sentinel. Real expiry is still enforced here in Verify against the
// DB value, so this never weakens revocation/expiry semantics.
var noExpiry = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// claimsKey is the map key under TokenInfo.Extra where we stash the typed kotoji
// claims so tool handlers can recover (UserID, TokenID, Scopes, CanCreateSites).
const claimsKey = "kotoji.claims"

// errUnauthenticated is returned to a tool handler when the verified principal is
// missing/garbled in the request — it should never happen behind RequireBearerToken,
// but tools fail closed to a forbidden/unauthenticated result rather than panic.
var errUnauthenticated = errors.New("mcpserver: unauthenticated")

// TokenInfo is the typed principal a verified PAT resolves to. It carries the
// owning user, the token id, the token's scope set, and the create-site flag. It
// does NOT carry a site: a token spans ALL of the user's memberships, and the
// per-site effective scope is resolved at call time (membership-capped, registry.go).
// (Named per the task contract; distinct from the SDK's auth.TokenInfo.)
type TokenInfo struct {
	UserID         uuid.UUID // the human the token acts as (user_tokens.user_id)
	TokenID        uuid.UUID
	Scopes         []string // subset of {read,write,publish}
	CanCreateSites bool     // gates create_site (CANONICAL §6.2 / decision #8)
}

// tokenQuerier is the narrow slice of the generated query surface the verifier
// needs. internal/db.Store (which embeds *gen.Queries) satisfies it, and tests
// inject a fake. We depend on this interface — not the concrete Store — to keep
// the verifier unit-testable without a database (DI per project conventions).
type tokenQuerier interface {
	// GetUserTokenByPrefix returns ACTIVE tokens (not revoked, not expired, owner
	// active) sharing token_prefix. The prefix is NOT unique by design; the
	// verifier constant-time compares token_hash to select the exact row.
	GetUserTokenByPrefix(ctx context.Context, tokenPrefix string) ([]gen.GetUserTokenByPrefixRow, error)
	// TouchUserToken bumps last_used_at; called best-effort, never blocks the request.
	TouchUserToken(ctx context.Context, id uuid.UUID) error
}

// compile-time guarantee: the real generated querier (and thus *db.Store) is a
// valid tokenQuerier.
var _ tokenQuerier = (gen.Querier)(nil)

// Verifier resolves a Bearer 'kotoji_pat_' token to a kotoji principal. It is the
// SDK's auth.TokenVerifier implementation (Verify method bound below).
type Verifier struct {
	q     tokenQuerier
	clock func() time.Time
	// touch runs the best-effort last_used_at bump. Overridable in tests so the
	// async path is assertable/deterministic; defaults to a detached goroutine.
	touch func(id uuid.UUID)
}

// NewVerifier builds a Verifier over the token query surface (internal/db.Store).
func NewVerifier(q tokenQuerier) *Verifier {
	v := &Verifier{q: q, clock: time.Now}
	v.touch = v.touchAsync
	return v
}

// Verify implements auth.TokenVerifier. Verification failures MUST unwrap to
// auth.ErrInvalidToken so the SDK returns 401 with a WWW-Authenticate header
// (RFC 6750). Infrastructure failures (DB down) return a plain error → 500, so
// they surface as protocol errors and get retried/escalated rather than told to
// the client as "your token is bad".
//
// Flow (mcp.md §3.3): prefix fast-reject → indexed prefix lookup → sha256 hash
// constant-time match → expired/revoked checks → typed claims in Extra.
func (v *Verifier) Verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	// 1. Prefix fast-reject: not our token format, no DB hit.
	if !strings.HasPrefix(token, tokenPrefix) {
		return nil, fmt.Errorf("mcpserver: malformed token: %w", auth.ErrInvalidToken)
	}
	if len(token) < prefixLen {
		return nil, fmt.Errorf("mcpserver: short token: %w", auth.ErrInvalidToken)
	}

	// 2. Narrow by the indexed 12-char prefix among ACTIVE tokens. The DB query
	// already filters revoked/expired/inactive-owner, but we re-check below so
	// the contract holds even if the query is swapped.
	rows, err := v.q.GetUserTokenByPrefix(ctx, token[:prefixLen])
	if err != nil {
		// Infrastructure error: NOT ErrInvalidToken → 500.
		return nil, fmt.Errorf("mcpserver: token lookup: %w", err)
	}

	// 3. Constant-time compare sha256(plaintext) against every candidate hash.
	// Prefix is non-unique by design, so iterate; subtle.ConstantTimeCompare
	// avoids leaking which candidate matched via timing.
	sum := sha256.Sum256([]byte(token))
	var match *gen.GetUserTokenByPrefixRow
	for i := range rows {
		if subtle.ConstantTimeCompare(rows[i].TokenHash, sum[:]) == 1 {
			match = &rows[i]
			break
		}
	}
	if match == nil {
		return nil, fmt.Errorf("mcpserver: unknown token: %w", auth.ErrInvalidToken)
	}

	// 4. Defense-in-depth expiry/revocation re-check against the row (in case the
	// query filtering ever changes). Revoked rows are excluded by the query (no
	// revoked_at column returned), so we enforce expiry here.
	exp := noExpiry
	if match.ExpiresAt.Valid {
		if !v.clock().Before(match.ExpiresAt.Time) {
			return nil, fmt.Errorf("mcpserver: expired token: %w", auth.ErrInvalidToken)
		}
		exp = match.ExpiresAt.Time
	}
	// Capability is the AND of the token flag and the owning user's flag
	// (CANONICAL §6.2 / decision #8): a token can never exceed its owner.
	canCreate := match.CanCreateSites && match.UserCanCreateSites

	// 5. Best-effort last_used_at bump; never blocks/fails the request.
	v.touch(match.ID)

	return &auth.TokenInfo{
		Scopes:     match.Scopes,
		Expiration: exp,
		UserID:     match.UserID.String(),
		Extra: map[string]any{
			claimsKey: TokenInfo{
				UserID:         match.UserID,
				TokenID:        match.ID,
				Scopes:         match.Scopes,
				CanCreateSites: canCreate,
			},
		},
	}, nil
}

// touchAsync bumps last_used_at on a detached goroutine with its own bounded
// context, so a slow/failed write never blocks or fails the originating call.
func (v *Verifier) touchAsync(id uuid.UUID) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// Error is intentionally ignored: last_used_at is best-effort telemetry.
		_ = v.q.TouchUserToken(ctx, id)
	}()
}

// claimsFromTokenInfo recovers the typed kotoji claims stashed in Extra. ok is
// false when the principal is missing/garbled (treated as unauthenticated).
func claimsFromTokenInfo(info *auth.TokenInfo) (TokenInfo, bool) {
	if info == nil {
		return TokenInfo{}, false
	}
	c, ok := info.Extra[claimsKey].(TokenInfo)
	return c, ok
}

// timestamptz is a tiny helper used in tests/builders to construct an optional
// expiry; kept here next to the verifier so the time-mapping logic lives in one
// place.
func timestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
