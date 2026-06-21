package api

import (
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

// Token plaintext format (CANONICAL §8): "kotoji_pat_<base62>", >=160 bits of
// CSPRNG entropy. Only sha256(plaintext) + a 12-char token_prefix are stored.
const (
	tokenPrefixStr = "kotoji_pat_"
	// tokenRandomLen is the base62 random suffix length. 30 base62 chars carry
	// ~178 bits of entropy (log2(62)*30), comfortably above the 160-bit floor.
	tokenRandomLen = 30
	// tokenPrefixLen is the stored prefix length (DB CHECK: char_length = 12).
	tokenPrefixLen = 12
)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// listTokens GET /api/sites/{handle}/tokens — owner only; never returns secrets.
func (s *server) listTokens(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}
	rows, err := s.deps.Store.ListTokensForSite(r.Context(), ac.site.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]openapi.TokenSummary, 0, len(rows))
	for _, t := range rows {
		out = append(out, openapi.TokenSummary{
			Id:             t.ID,
			Name:           t.Name,
			TokenPrefix:    t.TokenPrefix,
			Scopes:         toScopes(t.Scopes),
			CanCreateSites: t.CanCreateSites,
			CreatedAt:      ts(t.CreatedAt),
			LastUsedAt:     tsToTimePtr(t.LastUsedAt),
			ExpiresAt:      tsToTimePtr(t.ExpiresAt),
			RevokedAt:      tsToTimePtr(t.RevokedAt),
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Tokens []openapi.TokenSummary `json:"tokens"`
	}{Tokens: out})
}

// createToken POST /api/sites/{handle}/tokens — issue a token (owner only). The
// plaintext is returned ONCE; only the hash + prefix are persisted.
func (s *server) createToken(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}
	var body openapi.CreateTokenRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "name is required", validationDetails{Field: "name", Reason: "required"})
		return
	}
	if len(body.Scopes) == 0 {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "at least one scope is required", validationDetails{Field: "scopes", Reason: "min 1"})
		return
	}
	if !validScopes(body.Scopes) {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid scope", validationDetails{Field: "scopes", Reason: "must be subset of read|write|publish"})
		return
	}

	plaintext, prefix, hash, gerr := generateToken()
	if gerr != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "could not generate a token", nil)
		return
	}

	canCreate := false
	if body.CanCreateSites != nil {
		canCreate = *body.CanCreateSites
	}

	row, err := s.deps.Store.CreateToken(r.Context(), gen.CreateTokenParams{
		SiteID:         ac.site.ID,
		CreatedBy:      ac.user.UserID,
		Name:           body.Name,
		TokenPrefix:    prefix,
		TokenHash:      hash,
		Scopes:         fromScopes(body.Scopes),
		CanCreateSites: canCreate,
		ExpiresAt:      tsFromPtr(body.ExpiresAt),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}

	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		TokenID:     uuidPtr(row.ID),
		Action:      "token.create",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{"name": body.Name, "scopes": fromScopes(body.Scopes)}),
	})

	writeJSON(w, http.StatusCreated, openapi.CreatedToken{
		Id:             row.ID,
		Name:           row.Name,
		TokenPrefix:    row.TokenPrefix,
		Scopes:         toScopes(row.Scopes),
		CanCreateSites: row.CanCreateSites,
		CreatedAt:      ts(row.CreatedAt),
		LastUsedAt:     tsToTimePtr(row.LastUsedAt),
		ExpiresAt:      tsToTimePtr(row.ExpiresAt),
		RevokedAt:      tsToTimePtr(row.RevokedAt),
		Token:          plaintext,
	})
}

// revokeToken DELETE /api/sites/{handle}/tokens/{tokenId} — owner only.
func (s *server) revokeToken(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.resolveAccess(w, r, chi.URLParam(r, "handle"), capOwner)
	if !ok {
		return
	}
	tokenID, perr := uuid.Parse(chi.URLParam(r, "tokenId"))
	if perr != nil {
		writeError(w, http.StatusUnprocessableEntity, codeValidation, "invalid token id", validationDetails{Field: "tokenId", Reason: "must be a uuid"})
		return
	}
	// RevokeToken is scoped to the site (prevents cross-site revocation) and only
	// touches active rows; a missing/already-revoked token is a no-op -> 204
	// idempotent (we do not 404 to avoid token-id enumeration).
	if err := s.deps.Store.RevokeToken(r.Context(), gen.RevokeTokenParams{ID: tokenID, SiteID: ac.site.ID}); err != nil {
		writeServiceError(w, err)
		return
	}
	s.auditBestEffort(r.Context(), gen.InsertAuditParams{
		ActorUserID: uuidPtr(ac.user.UserID),
		SiteID:      uuidPtr(ac.site.ID),
		TokenID:     uuidPtr(tokenID),
		Action:      "token.revoke",
		Source:      gen.AuditSourceEditor,
		Metadata:    auditMeta(map[string]any{}),
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---- token plaintext generation ----

// generateToken mints a fresh token plaintext, returning (plaintext, prefix,
// sha256hash). The prefix is the first 12 chars of the plaintext (DB CHECK len),
// and only the hash is stored (CANONICAL §8).
func generateToken() (plaintext, prefix string, hash []byte, err error) {
	random, err := randBase62(tokenRandomLen)
	if err != nil {
		return "", "", nil, err
	}
	plaintext = tokenPrefixStr + random
	prefix = plaintext[:tokenPrefixLen]
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, prefix, sum[:], nil
}

// randBase62 returns n base62 characters from crypto/rand. Each character is an
// unbiased index drawn via rejection sampling through crypto/rand.Int.
func randBase62(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(base62Alphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = base62Alphabet[idx.Int64()]
	}
	return string(out), nil
}

// ---- scope conversions ----

// validScopes reports whether every scope is one of read|write|publish.
func validScopes(scopes []openapi.TokenScope) bool {
	for _, sc := range scopes {
		switch sc {
		case openapi.Read, openapi.Write, openapi.Publish:
		default:
			return false
		}
	}
	return true
}

// fromScopes maps wire scopes to the DB's text[] column.
func fromScopes(scopes []openapi.TokenScope) []string {
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		out = append(out, string(sc))
	}
	return out
}

// toScopes maps the DB's text[] column back to wire scopes.
func toScopes(scopes []string) []openapi.TokenScope {
	out := make([]openapi.TokenScope, 0, len(scopes))
	for _, sc := range scopes {
		out = append(out, openapi.TokenScope(sc))
	}
	return out
}

// ---- pgtype timestamp helpers (local to the token layer) ----

// ts converts a guaranteed-valid timestamp to time.Time (created_at is NOT NULL).
func ts(t pgtype.Timestamptz) time.Time { return t.Time }

// tsFromPtr converts a *time.Time to a pgtype.Timestamptz (NULL when nil).
func tsFromPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
