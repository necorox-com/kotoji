package api

import (
	"net/http"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

func TestMembers(t *testing.T) {
	t.Run("owner adds an existing user as editor", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("mem-site", owner)
		invitee := e.newUser() // already known (signed in once)

		rec := e.request(http.MethodPost, "/api/sites/mem-site/members").as(owner).
			json(openapi.AddMemberJSONRequestBody{Email: openapiEmail(invitee.rec.Email), Role: openapi.SiteRoleEditor}).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.Member
		decodeBody(t, rec, &got)
		if got.Role != openapi.SiteRoleEditor || got.UserId != invitee.rec.ID {
			t.Fatalf("got %+v, want editor for invitee", got)
		}
	})

	t.Run("unknown email is 422", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("mem-unknown-site", owner)
		rec := e.request(http.MethodPost, "/api/sites/mem-unknown-site/members").as(owner).
			json(openapi.AddMemberJSONRequestBody{Email: "ghost@example.com", Role: openapi.SiteRoleViewer}).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})

	t.Run("editor cannot manage members (403)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("mem-deny-site", owner)
		editor := e.newUser()
		e.store.setRole(st.ID, editor.rec.ID, gen.SiteRoleEditor)
		invitee := e.newUser()
		rec := e.request(http.MethodPost, "/api/sites/mem-deny-site/members").as(editor).
			json(openapi.AddMemberJSONRequestBody{Email: openapiEmail(invitee.rec.Email), Role: openapi.SiteRoleViewer}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("list includes the owner", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		e.createSite("mem-list-site", owner)
		rec := e.request(http.MethodGet, "/api/sites/mem-list-site/members").as(owner).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got struct {
			Members []openapi.Member `json:"members"`
		}
		decodeBody(t, rec, &got)
		if len(got.Members) == 0 {
			t.Fatalf("expected at least the owner member")
		}
	})

	t.Run("cannot remove the sole owner (409)", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("sole-owner-site", owner)
		rec := e.request(http.MethodDelete, "/api/sites/sole-owner-site/members/"+st.OwnerID.String()).as(owner).do()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("update role and remove a non-owner member", func(t *testing.T) {
		e := newTestEnv(t)
		owner := e.newUser()
		st := e.createSite("mem-mut-site", owner)
		member := e.newUser()
		e.store.setRole(st.ID, member.rec.ID, gen.SiteRoleViewer)

		// promote viewer -> editor
		prec := e.request(http.MethodPatch, "/api/sites/mem-mut-site/members/"+member.rec.ID.String()).as(owner).
			json(openapi.UpdateMemberRoleJSONRequestBody{Role: openapi.SiteRoleEditor}).do()
		if prec.Code != http.StatusOK {
			t.Fatalf("patch status = %d (body=%s)", prec.Code, prec.Body.String())
		}
		// remove
		drec := e.request(http.MethodDelete, "/api/sites/mem-mut-site/members/"+member.rec.ID.String()).as(owner).do()
		if drec.Code != http.StatusNoContent {
			t.Fatalf("delete status = %d", drec.Code)
		}
	})
}
