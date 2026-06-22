package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/db"
)

// TestAdminGitHubConfig covers the /api/admin/github contract: admin-only access,
// the secret-safe GET/PUT shapes, the write-only token (never echoed, empty keeps
// existing), and clearToken.
func TestAdminGitHubConfig(t *testing.T) {
	t.Run("anonymous GET is unauthenticated", func(t *testing.T) {
		e := newTestEnv(t)
		rec := e.request(http.MethodGet, "/api/admin/github").do()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("non-admin GET is forbidden", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		rec := e.request(http.MethodGet, "/api/admin/github").as(u).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("non-admin PUT is forbidden", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		rec := e.request(http.MethodPut, "/api/admin/github").as(u).
			json(map[string]any{"token": "ghp_x"}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		// And nothing was persisted.
		if len(e.store.setGitHubInputs) != 0 {
			t.Fatalf("non-admin PUT must not write config")
		}
	})

	t.Run("admin GET returns secret-safe shape", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		// Seed a DB config with a token + webhook secret + org + enabled.
		e.store.github = db.GitHubConfig{
			Enabled: true, EnabledSet: true,
			Token: "ghp_dbsecret", TokenSet: true,
			Org:           "necorox-com",
			WebhookSecret: "whsecret",
		}
		rec := e.request(http.MethodGet, "/api/admin/github").as(admin).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		// The raw body must NOT contain the token or the webhook secret.
		if body := rec.Body.String(); strings.Contains(body, "ghp_dbsecret") || strings.Contains(body, "whsecret") {
			t.Fatalf("response leaked a secret value: %s", body)
		}
		var got githubAdminConfig
		decodeBody(t, rec, &got)
		if !got.Enabled || got.Org != "necorox-com" || !got.TokenSet || !got.WebhookSecretSet {
			t.Fatalf("unexpected config view: %+v", got)
		}
	})

	t.Run("admin PUT persists and never echoes the token", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/github").as(admin).
			json(map[string]any{
				"enabled":       true,
				"org":           "acme",
				"token":         "ghp_brandnew",
				"webhookSecret": "wh-new",
			}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); strings.Contains(body, "ghp_brandnew") || strings.Contains(body, "wh-new") {
			t.Fatalf("PUT response leaked a secret: %s", body)
		}
		var got githubAdminConfig
		decodeBody(t, rec, &got)
		if !got.Enabled || got.Org != "acme" || !got.TokenSet || !got.WebhookSecretSet {
			t.Fatalf("post-PUT view wrong: %+v", got)
		}
		// The store recorded the token write.
		if len(e.store.setGitHubInputs) != 1 {
			t.Fatalf("expected one SetGitHubConfig call, got %d", len(e.store.setGitHubInputs))
		}
		if e.store.github.Token != "ghp_brandnew" {
			t.Fatalf("token not persisted: %q", e.store.github.Token)
		}
	})

	t.Run("PUT with empty token keeps the existing one", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		e.store.github = db.GitHubConfig{Token: "ghp_keepme", TokenSet: true}

		rec := e.request(http.MethodPut, "/api/admin/github").as(admin).
			json(map[string]any{"org": "renamed", "token": ""}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if e.store.github.Token != "ghp_keepme" {
			t.Fatalf("empty token clobbered existing: %q", e.store.github.Token)
		}
		if e.store.github.Org != "renamed" {
			t.Fatalf("org not updated: %q", e.store.github.Org)
		}
	})

	t.Run("PUT clearToken removes the stored token", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		e.store.github = db.GitHubConfig{Token: "ghp_doomed", TokenSet: true}

		rec := e.request(http.MethodPut, "/api/admin/github").as(admin).
			json(map[string]any{"clearToken": true}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got githubAdminConfig
		decodeBody(t, rec, &got)
		if got.TokenSet || e.store.github.TokenSet {
			t.Fatalf("clearToken did not remove the token: %+v / %+v", got, e.store.github)
		}
	})

	t.Run("env token shows TokenSet without a DB token", func(t *testing.T) {
		e := newTestEnvWithConfig(t, func(c *configForTest) {
			c.GitHubEnabled = true
			c.GitHubToken = "ghp_fromenv"
		})
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodGet, "/api/admin/github").as(admin).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); strings.Contains(body, "ghp_fromenv") {
			t.Fatalf("env token leaked into response: %s", body)
		}
		var got githubAdminConfig
		decodeBody(t, rec, &got)
		if !got.Enabled || !got.TokenSet {
			t.Fatalf("env-configured mirror should report enabled+tokenSet: %+v", got)
		}
	})
}
