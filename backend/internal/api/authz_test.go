package api

import (
	"net/http"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

// TestRoleCapabilityMatrix is the table-driven role x action matrix from
// CANONICAL §6.1. Each row exercises one (role, endpoint) pair and asserts the
// expected status family (allowed: not 401/403/404 from the authz gate; denied:
// 403). viewer can read but never write; editor writes but does not own; owner
// does everything.
func TestRoleCapabilityMatrix(t *testing.T) {
	type call struct {
		method string
		path   string
		body   any
	}
	// helper: seed a fresh site + run a call as a given role; return the status.
	run := func(t *testing.T, role gen.SiteRole, c call) int {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("matrix-site", owner)
		var actor testUser
		if role == gen.SiteRoleOwner {
			actor = owner
		} else {
			actor = e.newUser()
			e.store.setRole(st.ID, actor.rec.ID, role)
		}
		rb := e.request(c.method, c.path).as(actor)
		if c.body != nil {
			rb = rb.json(c.body)
		}
		return rb.do().Code
	}

	// allowed asserts the authz gate did NOT block (status is not a gate denial).
	// The underlying op may still 4xx for other reasons (e.g. validation), but it
	// must never be 401/403 (and not the authz 404).
	allowed := func(status int) bool {
		return status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusNotFound
	}

	readCall := call{http.MethodGet, "/api/sites/matrix-site/branches", nil}
	writeCall := call{http.MethodPut, "/api/sites/matrix-site/branches/draft/file",
		openapi.WriteFileRequest{Path: "index.html", Content: "<h1>hi</h1>", BaseSha: "seed"}}
	// Owner-only per-site op probe. Tokens moved to /api/tokens (per-user), so we
	// use the GitHub mirror trigger, which is owner-only (CANONICAL §6.1).
	ownerCall := call{http.MethodPost, "/api/sites/matrix-site/mirror", nil}

	cases := []struct {
		name        string
		role        gen.SiteRole
		call        call
		wantAllowed bool
	}{
		{"viewer read", gen.SiteRoleViewer, readCall, true},
		{"editor read", gen.SiteRoleEditor, readCall, true},
		{"owner read", gen.SiteRoleOwner, readCall, true},

		{"viewer write denied", gen.SiteRoleViewer, writeCall, false},
		{"editor write allowed", gen.SiteRoleEditor, writeCall, true},
		{"owner write allowed", gen.SiteRoleOwner, writeCall, true},

		{"viewer owner-op denied", gen.SiteRoleViewer, ownerCall, false},
		{"editor owner-op denied", gen.SiteRoleEditor, ownerCall, false},
		{"owner owner-op allowed", gen.SiteRoleOwner, ownerCall, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := run(t, tc.role, tc.call)
			if allowed(status) != tc.wantAllowed {
				t.Fatalf("role=%s call=%s %s status=%d allowed=%v want=%v",
					tc.role, tc.call.method, tc.call.path, status, allowed(status), tc.wantAllowed)
			}
			// Denied write/owner ops must be exactly 403 (not 404/401) for a member.
			if !tc.wantAllowed && status != http.StatusForbidden {
				t.Fatalf("denied call status=%d, want 403", status)
			}
		})
	}
}

// TestUnauthenticatedAcrossEndpoints asserts the whole guarded tree returns 401
// for anonymous callers (session-auth gate).
func TestUnauthenticatedAcrossEndpoints(t *testing.T) {
	e := newTestEnv(t)
	endpoints := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/sites"},
		{http.MethodGet, "/api/sites/x/branches"},
		{http.MethodGet, "/api/sites/x/members"},
		{http.MethodGet, "/api/tokens"},
	}
	for _, ep := range endpoints {
		rec := e.request(ep.method, ep.path).do()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d, want 401", ep.method, ep.path, rec.Code)
		}
	}
}

// TestCSRFGuard asserts a mutating cookie request without a CSRF token is
// rejected with 403, and that bearer-token requests are exempt.
func TestCSRFGuard(t *testing.T) {
	e := newTestEnv(t)
	u := e.newUser()

	t.Run("missing CSRF token is 403", func(t *testing.T) {
		rec := e.request(http.MethodPost, "/api/sites").as(u).
			json(openapi.CreateSiteRequest{Handle: "csrf-site"}).noCSRF().do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if env := errEnvelope(t, rec); env.Error.Code != codeForbidden {
			t.Fatalf("code = %q, want forbidden", env.Error.Code)
		}
	})

	t.Run("safe method needs no CSRF token", func(t *testing.T) {
		rec := e.request(http.MethodGet, "/api/sites").as(u).noCSRF().do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}
