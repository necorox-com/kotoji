package api

import (
	"net/http"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/openapi"
)

func TestBranches(t *testing.T) {
	e := newTestEnv(t)
	owner := e.newUser()
	e.createSite("branch-site", owner)

	t.Run("list includes draft", func(t *testing.T) {
		rec := e.request(http.MethodGet, "/api/sites/branch-site/branches").as(owner).do()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var got struct {
			Branches []openapi.Branch `json:"branches"`
		}
		decodeBody(t, rec, &got)
		if len(got.Branches) == 0 {
			t.Fatalf("expected at least the draft branch")
		}
	})

	t.Run("create a feature branch from draft", func(t *testing.T) {
		rec := e.request(http.MethodPost, "/api/sites/branch-site/branches").as(owner).
			json(openapi.CreateBranchJSONRequestBody{Name: "feature-test-x", From: "draft"}).do()
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
		}
		var got openapi.Branch
		decodeBody(t, rec, &got)
		if got.Name != "feature-test-x" {
			t.Fatalf("name = %q, want feature-test-x", got.Name)
		}
	})

	t.Run("delete refuses draft (422)", func(t *testing.T) {
		rec := e.request(http.MethodDelete, "/api/sites/branch-site/branches/draft").as(owner).do()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("viewer cannot create a branch (403)", func(t *testing.T) {
		viewer := e.newUser()
		st, _ := e.svc.GetSiteByHandle(t.Context(), "branch-site")
		e.store.setRole(st.ID, viewer.rec.ID, "viewer")
		rec := e.request(http.MethodPost, "/api/sites/branch-site/branches").as(viewer).
			json(openapi.CreateBranchJSONRequestBody{Name: "feature-viewer-y", From: "draft"}).do()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}
