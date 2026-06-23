package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

func TestTokens(t *testing.T) {
	t.Run("user issues a token; plaintext shown once with correct prefix", func(t *testing.T) {
		e := newTestEnv(t)
		user := e.newUser()

		rec := e.request(http.MethodPost, "/api/tokens").as(user).
			json(openapi.CreateTokenRequest{Name: "laptop", Scopes: []openapi.TokenScope{openapi.Read, openapi.Write}}).do()
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.CreatedToken
		decodeBody(t, rec, &got)
		if !strings.HasPrefix(got.Token, tokenPrefixStr) {
			t.Fatalf("token = %q, want kotoji_pat_ prefix", got.Token)
		}
		if got.TokenPrefix != got.Token[:tokenPrefixLen] {
			t.Fatalf("tokenPrefix %q != first 12 of plaintext %q", got.TokenPrefix, got.Token[:tokenPrefixLen])
		}
		if len(got.Token) < len(tokenPrefixStr)+tokenRandomLen {
			t.Fatalf("token too short: %q", got.Token)
		}
	})

	t.Run("invalid scope is 422", func(t *testing.T) {
		e := newTestEnv(t)
		user := e.newUser()
		rec := e.request(http.MethodPost, "/api/tokens").as(user).
			json(openapi.CreateTokenRequest{Name: "x", Scopes: []openapi.TokenScope{"admin"}}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})

	t.Run("canCreateSites is capped by the user's account flag", func(t *testing.T) {
		e := newTestEnv(t)
		// A user who is NOT allowed to create sites requests canCreateSites=true.
		user := e.newUser(withNoCreate)
		want := true
		rec := e.request(http.MethodPost, "/api/tokens").as(user).
			json(openapi.CreateTokenRequest{Name: "ci", Scopes: []openapi.TokenScope{openapi.Read}, CanCreateSites: &want}).do()
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.CreatedToken
		decodeBody(t, rec, &got)
		if got.CanCreateSites {
			t.Fatalf("canCreateSites = true, want false (capped by users.can_create_sites)")
		}
	})

	t.Run("canCreateSites granted when the user is allowed", func(t *testing.T) {
		e := newTestEnv(t)
		user := e.newUser() // default CanCreateSites=true
		want := true
		rec := e.request(http.MethodPost, "/api/tokens").as(user).
			json(openapi.CreateTokenRequest{Name: "ci", Scopes: []openapi.TokenScope{openapi.Write}, CanCreateSites: &want}).do()
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.CreatedToken
		decodeBody(t, rec, &got)
		if !got.CanCreateSites {
			t.Fatalf("canCreateSites = false, want true (user is create-capable)")
		}
	})

	t.Run("list returns only the caller's own tokens, never the secret; revoke is idempotent 204", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		other := e.newUser()

		crec := e.request(http.MethodPost, "/api/tokens").as(owner).
			json(openapi.CreateTokenRequest{Name: "ci", Scopes: []openapi.TokenScope{openapi.Publish}}).do()
		var created openapi.CreatedToken
		decodeBody(t, crec, &created)

		// owner sees exactly one token; the secret is never echoed.
		lrec := e.request(http.MethodGet, "/api/tokens").as(owner).do()
		if lrec.Code != http.StatusOK {
			t.Fatalf("list status = %d", lrec.Code)
		}
		if strings.Contains(lrec.Body.String(), created.Token) {
			t.Fatalf("token list leaked the plaintext secret")
		}
		var owned struct {
			Tokens []openapi.TokenSummary `json:"tokens"`
		}
		decodeBody(t, lrec, &owned)
		if len(owned.Tokens) != 1 {
			t.Fatalf("owner token count = %d, want 1", len(owned.Tokens))
		}

		// A DIFFERENT user's list does NOT include owner's token (per-user scoping).
		orec := e.request(http.MethodGet, "/api/tokens").as(other).do()
		var others struct {
			Tokens []openapi.TokenSummary `json:"tokens"`
		}
		decodeBody(t, orec, &others)
		if len(others.Tokens) != 0 {
			t.Fatalf("other user saw %d tokens, want 0 (cross-user leak)", len(others.Tokens))
		}

		// owner revokes their own token -> 204; idempotent.
		rrec := e.request(http.MethodDelete, "/api/tokens/"+created.Id.String()).as(owner).do()
		if rrec.Code != http.StatusNoContent {
			t.Fatalf("revoke status = %d", rrec.Code)
		}
	})
}

// TestGenerateTokenEntropy asserts the token codec produces distinct, prefixed,
// hash-matching plaintexts.
func TestGenerateTokenEntropy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		pt, prefix, hash, err := generateToken()
		if err != nil {
			t.Fatalf("generateToken: %v", err)
		}
		if seen[pt] {
			t.Fatalf("duplicate token generated: %q", pt)
		}
		seen[pt] = true
		if !strings.HasPrefix(pt, tokenPrefixStr) {
			t.Fatalf("missing prefix: %q", pt)
		}
		if prefix != pt[:tokenPrefixLen] {
			t.Fatalf("prefix mismatch")
		}
		if len(hash) != 32 {
			t.Fatalf("hash len = %d, want 32", len(hash))
		}
	}
}
