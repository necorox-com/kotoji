package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

func TestTokens(t *testing.T) {
	t.Run("owner issues a token; plaintext shown once with correct prefix", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("tok-site", owner)

		rec := e.request(http.MethodPost, "/api/sites/tok-site/tokens").as(owner).
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
		owner := e.newUser()
		e.createSite("tok-bad-site", owner)
		rec := e.request(http.MethodPost, "/api/sites/tok-bad-site/tokens").as(owner).
			json(openapi.CreateTokenRequest{Name: "x", Scopes: []openapi.TokenScope{"admin"}}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})

	t.Run("editor cannot issue tokens (403)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("tok-deny-site", owner)
		editor := e.newUser()
		e.store.setRole(st.ID, editor.rec.ID, "editor")
		rec := e.request(http.MethodPost, "/api/sites/tok-deny-site/tokens").as(editor).
			json(openapi.CreateTokenRequest{Name: "x", Scopes: []openapi.TokenScope{openapi.Read}}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("list never returns the secret, then revoke is idempotent 204", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("tok-life-site", owner)
		crec := e.request(http.MethodPost, "/api/sites/tok-life-site/tokens").as(owner).
			json(openapi.CreateTokenRequest{Name: "ci", Scopes: []openapi.TokenScope{openapi.Publish}}).do()
		var created openapi.CreatedToken
		decodeBody(t, crec, &created)

		lrec := e.request(http.MethodGet, "/api/sites/tok-life-site/tokens").as(owner).do()
		if lrec.Code != http.StatusOK {
			t.Fatalf("list status = %d", lrec.Code)
		}
		if strings.Contains(lrec.Body.String(), created.Token) {
			t.Fatalf("token list leaked the plaintext secret")
		}

		rrec := e.request(http.MethodDelete, "/api/sites/tok-life-site/tokens/"+created.Id.String()).as(owner).do()
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
