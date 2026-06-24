package api

import (
	"net/http"
	"testing"
)

// TestAdminDomainConfig covers the /api/admin/domain contract: admin-only access,
// the effective GET view (value + source + locked flags), PUT validation, the
// env-locked rejection (409), the empty-string revert, and cache invalidation.
func TestAdminDomainConfig(t *testing.T) {
	t.Run("anonymous GET is unauthenticated", func(t *testing.T) {
		e := newTestEnv(t)
		rec := e.request(http.MethodGet, "/api/admin/domain").do()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("non-admin GET is forbidden", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		rec := e.request(http.MethodGet, "/api/admin/domain").as(u).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("non-admin PUT is forbidden and writes nothing", func(t *testing.T) {
		e := newTestEnv(t)
		u := e.newUser()
		rec := e.request(http.MethodPut, "/api/admin/domain").as(u).
			json(map[string]any{"baseDomain": "x.example.com"}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if len(e.store.setDomainInputs) != 0 {
			t.Fatalf("non-admin PUT must not write config")
		}
	})

	t.Run("admin GET derives from request when neither env nor DB set", func(t *testing.T) {
		// Default test config leaves both env flags false, so the effective value
		// derives from the incoming request Host (httptest default: example.com).
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodGet, "/api/admin/domain").as(admin).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got domainAdminConfig
		decodeBody(t, rec, &got)
		if got.BaseDomainSource != "derived" || got.ControlBaseURLSource != "derived" {
			t.Fatalf("sources = %q/%q, want derived/derived", got.BaseDomainSource, got.ControlBaseURLSource)
		}
		if got.BaseDomainLocked || got.ControlBaseURLLocked {
			t.Fatalf("derived fields must not be locked: %+v", got)
		}
		if got.BaseDomain != "example.com" || got.ControlBaseURL != "http://example.com" {
			t.Fatalf("derived values = %q / %q", got.BaseDomain, got.ControlBaseURL)
		}
	})

	t.Run("admin GET reports env source + locked when env set", func(t *testing.T) {
		e := newTestEnvWithConfig(t, func(c *configForTest) {
			c.BaseDomain = "hosting.example.com"
			c.ControlBaseURL = "https://hosting.example.com"
			c.BaseDomainEnvSet = true
			c.ControlBaseURLEnvSet = true
		})
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodGet, "/api/admin/domain").as(admin).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got domainAdminConfig
		decodeBody(t, rec, &got)
		if got.BaseDomainSource != "env" || !got.BaseDomainLocked {
			t.Fatalf("baseDomain should be env/locked: %+v", got)
		}
		if got.ControlBaseURLSource != "env" || !got.ControlBaseURLLocked {
			t.Fatalf("controlBaseURL should be env/locked: %+v", got)
		}
		if got.BaseDomain != "hosting.example.com" || got.ControlBaseURL != "https://hosting.example.com" {
			t.Fatalf("env values wrong: %+v", got)
		}
	})

	t.Run("admin PUT persists, reports db source, and invalidates cache", func(t *testing.T) {
		e := newTestEnv(t) // env unset => editable
		admin := e.newUser(withAdmin)

		// Prime the cache with a derived read so we can prove invalidation works.
		_ = e.request(http.MethodGet, "/api/admin/domain").as(admin).do()

		rec := e.request(http.MethodPut, "/api/admin/domain").as(admin).
			json(map[string]any{
				"baseDomain":     "kotoji.example.com",
				"controlBaseURL": "https://kotoji.example.com/", // trailing slash normalized
			}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got domainAdminConfig
		decodeBody(t, rec, &got)
		if got.BaseDomain != "kotoji.example.com" || got.BaseDomainSource != "db" {
			t.Fatalf("post-PUT baseDomain = %+v, want db kotoji.example.com", got)
		}
		// Trailing slash must be normalized away before persisting.
		if got.ControlBaseURL != "https://kotoji.example.com" || got.ControlBaseURLSource != "db" {
			t.Fatalf("post-PUT controlBaseURL = %+v, want normalized db value", got)
		}
		if len(e.store.setDomainInputs) != 1 {
			t.Fatalf("expected one SetDomainConfig call, got %d", len(e.store.setDomainInputs))
		}
		if e.store.domain.BaseDomain != "kotoji.example.com" {
			t.Fatalf("base domain not persisted: %q", e.store.domain.BaseDomain)
		}
		if e.store.domain.ControlBaseURL != "https://kotoji.example.com" {
			t.Fatalf("control base URL not normalized/persisted: %q", e.store.domain.ControlBaseURL)
		}
	})

	t.Run("PUT to an env-locked field is rejected 409 (not a no-op)", func(t *testing.T) {
		e := newTestEnvWithConfig(t, func(c *configForTest) {
			c.BaseDomain = "hosting.example.com"
			c.BaseDomainEnvSet = true // baseDomain locked
		})
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/domain").as(admin).
			json(map[string]any{"baseDomain": "evil.example.com"}).do()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		// Nothing persisted.
		if len(e.store.setDomainInputs) != 0 {
			t.Fatalf("locked-field PUT must not write: %+v", e.store.setDomainInputs)
		}
		env := errEnvelope(t, rec)
		if env.Error.Code != codeConflict {
			t.Fatalf("error code = %q, want %q", env.Error.Code, codeConflict)
		}
	})

	t.Run("PUT control while base is locked persists only control", func(t *testing.T) {
		e := newTestEnvWithConfig(t, func(c *configForTest) {
			c.BaseDomain = "hosting.example.com"
			c.BaseDomainEnvSet = true // base locked, control editable
		})
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/domain").as(admin).
			json(map[string]any{"controlBaseURL": "https://ctl.example.com"}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if e.store.domain.ControlBaseURL != "https://ctl.example.com" {
			t.Fatalf("control not persisted: %q", e.store.domain.ControlBaseURL)
		}
	})

	t.Run("PUT invalid base domain is 422", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/domain").as(admin).
			json(map[string]any{"baseDomain": "https://not-a-host/x"}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
		if len(e.store.setDomainInputs) != 0 {
			t.Fatalf("invalid PUT must not write")
		}
	})

	t.Run("PUT invalid control base URL is 422", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/domain").as(admin).
			json(map[string]any{"controlBaseURL": "not-a-url"}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("PUT empty string reverts a DB-set field to env/derived", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		// First set a DB value.
		_ = e.request(http.MethodPut, "/api/admin/domain").as(admin).
			json(map[string]any{"baseDomain": "set.example.com"}).do()
		// Now clear it with an empty string -> reverts to derived.
		rec := e.request(http.MethodPut, "/api/admin/domain").as(admin).
			json(map[string]any{"baseDomain": ""}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got domainAdminConfig
		decodeBody(t, rec, &got)
		if got.BaseDomainSource != "derived" {
			t.Fatalf("after empty PUT source = %q, want derived (%+v)", got.BaseDomainSource, got)
		}
		if e.store.domain.BaseDomainSet {
			t.Fatalf("empty PUT should have deleted the DB key")
		}
	})

	t.Run("empty PUT body returns current view without writing", func(t *testing.T) {
		e := newTestEnv(t)
		admin := e.newUser(withAdmin)
		rec := e.request(http.MethodPut, "/api/admin/domain").as(admin).
			json(map[string]any{}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if len(e.store.setDomainInputs) != 0 {
			t.Fatalf("no-field PUT must not write: %+v", e.store.setDomainInputs)
		}
	})
}

// TestPublicConfigExposesControlBaseURL pins that /api/config carries the effective
// baseDomain + controlBaseURL the frontend reads (derived on a fresh install).
func TestPublicConfigExposesControlBaseURL(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodGet, "/api/config").do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		BaseDomain     string `json:"baseDomain"`
		ControlBaseURL string `json:"controlBaseURL"`
	}
	decodeBody(t, rec, &got)
	// Default test config: env flags false => derived from the request Host.
	if got.BaseDomain != "example.com" {
		t.Fatalf("baseDomain = %q, want example.com", got.BaseDomain)
	}
	if got.ControlBaseURL != "http://example.com" {
		t.Fatalf("controlBaseURL = %q, want http://example.com", got.ControlBaseURL)
	}
}
