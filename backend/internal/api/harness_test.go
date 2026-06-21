package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/necorox-com/kotoji/backend/internal/auth"
	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// testEnv bundles everything a handler test needs: the assembled router plus the
// fakes it can seed/assert against.
type testEnv struct {
	t        *testing.T
	router   http.Handler
	svc      *site.FakeService
	store    *fakeMetaStore
	sessions *fakeSessionStore
	cfg      config.Config
}

// testConfig is the minimal config for the API surface in tests: dev auth, no
// secure cookies (so un-prefixed cookie names work over http), small handle
// bounds, a tiny upload cap for the guard tests.
func testConfig() config.Config {
	return config.Config{
		Env:                config.EnvDevelopment,
		Mode:               config.RunModeControl,
		AuthMode:           config.AuthModeNone,
		BaseDomain:         "hosting.localhost",
		ControlBaseURL:     "http://hosting.localhost:8080",
		SessionCookieName:  "kotoji_session",
		SessionTTL:         time.Hour,
		CSRFCookieName:     "kotoji_csrf",
		CookieSecure:       false,
		MCPPath:            "/mcp",
		CORSAllowedOrigins: []string{"http://hosting.localhost:8080"},
		HandleMinLen:       3,
		HandleMaxLen:       63,
		Zip: config.ZipLimits{
			MaxUploadBytes: 1 << 20, // 1 MiB cap for upload-guard tests
			MaxTotalBytes:  1 << 21,
			MaxFiles:       100,
			MaxRatio:       100,
			AllowedExt:     []string{".html", ".css", ".js", ".png"},
		},
	}
}

// newTestEnv assembles the router with the REAL auth middleware (the only thing
// that can populate auth.CurrentUser) over fake stores + the FakeService.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := testConfig()
	svc := site.NewFakeService()
	store := newFakeMetaStore()
	sessions := newFakeSessionStore()

	provider := auth.NewDevProvider(cfg)
	authSvc := auth.New(cfg, sessions, provider)

	router := NewRouter(Deps{
		Config: cfg,
		Site:   svc,
		Store:  store,
		Auth:   WrapAuth(authSvc),
	})

	return &testEnv{t: t, router: router, svc: svc, store: store, sessions: sessions, cfg: cfg}
}

// user creates a seeded user with a live session and returns it plus the session
// cookie value. The user is registered in BOTH the session store (so the auth
// middleware authenticates the cookie) and the meta store (so role/member/admin
// lookups resolve).
type testUser struct {
	rec       gen.User
	sessionID string
}

func (e *testEnv) newUser(opts ...func(*gen.User)) testUser {
	e.t.Helper()
	u := gen.User{
		ID:             uuid.New(),
		Email:          uuid.NewString() + "@example.com",
		DisplayName:    "Test User",
		IsActive:       true,
		CanCreateSites: true,
		CreatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	for _, o := range opts {
		o(&u)
	}
	sid := uuid.NewString()
	e.sessions.seed(sid, u)
	e.store.addUser(u)
	return testUser{rec: u, sessionID: sid}
}

// withAdmin marks a created user as an instance superuser.
func withAdmin(u *gen.User) { u.IsAdmin = true }

// withNoCreate revokes the site-creation capability.
func withNoCreate(u *gen.User) { u.CanCreateSites = false }

// createSite seeds a site owned by owner (via the FakeService) and records the
// owner's role in the meta store. Extra members can be added with setRole.
func (e *testEnv) createSite(handle string, owner testUser) site.Site {
	return e.createSiteWith(handle, owner)
}

// createSiteWith is createSite with optional CreateSiteInput mutators (e.g. to
// set publish_mode/visibility on the Service-owned record).
func (e *testEnv) createSiteWith(handle string, owner testUser, opts ...func(*site.CreateSiteInput)) site.Site {
	e.t.Helper()
	in := site.CreateSiteInput{
		Handle:  site.Handle(handle),
		OwnerID: owner.rec.ID,
		Actor:   site.Actor{UserID: owner.rec.ID, Name: owner.rec.DisplayName, Email: owner.rec.Email, Via: site.SourceEditor},
	}
	for _, o := range opts {
		o(&in)
	}
	st, err := e.svc.CreateSite(context.Background(), in)
	if err != nil {
		e.t.Fatalf("seed site %q: %v", handle, err)
	}
	e.store.setRole(st.ID, owner.rec.ID, gen.SiteRoleOwner)
	return st
}

// ---- request helpers ----

// req is a fluent request builder that attaches auth + CSRF as configured.
type req struct {
	env    *testEnv
	method string
	path   string
	body   io.Reader
	ctype  string
	user   *testUser
	csrf   bool
}

func (e *testEnv) request(method, path string) *req {
	return &req{env: e, method: method, path: path, csrf: true}
}

func (r *req) as(u testUser) *req { r.user = &u; return r }

func (r *req) json(v any) *req {
	b, err := json.Marshal(v)
	if err != nil {
		r.env.t.Fatalf("marshal body: %v", err)
	}
	r.body = bytes.NewReader(b)
	r.ctype = "application/json"
	return r
}

func (r *req) raw(body io.Reader, ctype string) *req {
	r.body = body
	r.ctype = ctype
	return r
}

// noCSRF disables attaching the CSRF cookie+header (to exercise the guard).
func (r *req) noCSRF() *req { r.csrf = false; return r }

// do executes the request against the router and returns the recorder.
func (r *req) do() *httptest.ResponseRecorder {
	r.env.t.Helper()
	httpReq := httptest.NewRequest(r.method, r.path, r.body)
	if r.ctype != "" {
		httpReq.Header.Set("Content-Type", r.ctype)
	}
	if r.user != nil {
		httpReq.AddCookie(&http.Cookie{Name: r.env.cfg.SessionCookieName, Value: r.user.sessionID})
	}
	// Attach a matching CSRF cookie + header for mutating requests (the double-
	// submit guard requires them on non-safe methods for cookie auth).
	if r.csrf {
		const tok = "test-csrf-token"
		httpReq.AddCookie(&http.Cookie{Name: r.env.cfg.CSRFCookieName, Value: tok})
		httpReq.Header.Set("X-CSRF-Token", tok)
	}
	rec := httptest.NewRecorder()
	r.env.router.ServeHTTP(rec, httpReq)
	return rec
}

// decodeBody unmarshals a JSON response body into dst, failing the test on error.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
}

// errEnvelope decodes the response into an error envelope and returns the code.
func errEnvelope(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	decodeBody(t, rec, &env)
	return env
}

// mustJSON marshals v to bytes or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
