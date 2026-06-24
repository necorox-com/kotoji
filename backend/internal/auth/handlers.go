package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// adminPasswordHashKey is the instance_settings key for the first-run admin
// password bcrypt hash. It MUST match db.settingAdminPasswordHash (the store
// helper writes under the same key); a go test asserts the two stay in sync.
const adminPasswordHashKey = "admin_password_hash"

// loginStateTTL bounds how long a started login may take to complete. After this
// the state cookie expires and the callback is rejected (replay/abandon defense).
const loginStateTTL = 10 * time.Minute

// loginStateCookie is the (un-prefixed) name of the short-lived signed cookie
// that binds state+nonce+PKCE+next across the redirect to the IdP.
const loginStateCookie = "kotoji_login"

// stateNonceBytes is the entropy of the login state and nonce (256 bits each).
const stateNonceBytes = 32

// Auth is the assembled auth surface: provider SET + sessions + CSRF + handlers.
// It is constructed once at composition and exposes RegisterRoutes + Middleware.
//
// MULTI-PROVIDER (break-glass): several providers can be enabled at once (e.g.
// OIDC for humans + the single-admin password as emergency access). providers is
// keyed by the auth-mode string ("oidc"/"password"/"none"); the login handlers
// pick the right one per path (interactive GET redirect vs. password POST) so one
// provider never disables the other.
type Auth struct {
	cfg config.Config
	// providers is the set of ENABLED providers keyed by auth-mode string. At
	// most one is interactive (oidc); at most one is the non-interactive
	// password/dev provider.
	providers map[string]AuthProvider
	// interactive is the cached interactive provider (oidc), nil when not enabled.
	// password is the non-interactive credential provider (password or dev).
	interactive AuthProvider
	password    AuthProvider

	sessions *SessionManager
	csrf     *CSRF
	upserter UserUpserter
	// store is the metadata persistence seam used by the first-run setup flow
	// (read/write the admin-password hash) and the setupRequired computation.
	store StoreDeps

	// signKey signs the login-state cookie (HMAC). Derived from the admin
	// password / a random per-process key so the cookie is tamper-evident.
	signKey []byte

	// domain, when non-nil, supplies the EFFECTIVE base domain + control base URL
	// (WordPress-style env > DB > derived) for /api/config. nil falls back to the
	// static cfg values (the env-set fast path / tests that do not wire it).
	domain DomainResolver
}

// DomainResolver resolves the effective base domain + control base URL for the
// current request. *domaincfg.Provider satisfies it; the composition root injects
// it via SetDomainResolver. Kept an interface so the auth package does not depend
// on the domaincfg package (and tests can stub it).
type DomainResolver interface {
	BaseDomainFor(r *http.Request) string
	ControlBaseURLFor(r *http.Request) string
}

// SetDomainResolver installs the effective-domain resolver used by /api/config.
// Optional: a nil resolver (or never calling this) keeps the static cfg behavior,
// so existing tests / the env-set fast path are unaffected.
func (a *Auth) SetDomainResolver(d DomainResolver) { a.domain = d }

// UserUpserter is the atomic "match-or-create user + link identity" seam used at
// callback. *db.Store satisfies it via StoreUpserter; tests inject a fake.
type UserUpserter interface {
	UpsertLogin(ctx context.Context, in UpsertLoginInput) (gen.User, error)
}

// UpsertLoginInput carries the identity to materialize at login.
type UpsertLoginInput struct {
	Provider    string
	Subject     string
	Email       string
	DisplayName string
	AvatarURL   *string
}

// New assembles the Auth surface from a SINGLE provider. It is the back-compat
// constructor (legacy single-mode + every existing unit test): the provider is
// categorized by Interactive() so the right login path finds it. For the
// multi-provider (break-glass) wiring use NewWithProviders.
func New(cfg config.Config, store StoreDeps, provider AuthProvider) *Auth {
	return NewWithProviders(cfg, store, provider)
}

// NewWithProviders assembles the Auth surface from the SET of enabled providers
// (decision #1: OIDC + password may be enabled concurrently). Providers are
// categorized by Interactive(): the (single) interactive one drives the GET
// /auth/login redirect + /auth/callback, the (single) non-interactive one drives
// POST /auth/login. Passing them in any order is fine. store backs sessions and
// the login upsert.
func NewWithProviders(cfg config.Config, store StoreDeps, providers ...AuthProvider) *Auth {
	a := &Auth{
		cfg:       cfg,
		providers: make(map[string]AuthProvider, len(providers)),
		sessions:  NewSessionManager(store, cfg.SessionCookieName, cfg.SessionTTL, cfg.CookieSecure),
		csrf:      NewCSRF(cfg.CSRFCookieName, cfg.CookieSecure),
		upserter:  &storeUpserter{store: store},
		store:     store,
		signKey:   deriveSignKey(cfg),
	}
	for _, p := range providers {
		if p == nil {
			continue
		}
		a.providers[p.Key()] = p
		// Categorize for the two login paths. The interactive provider (oidc) owns
		// the redirect handshake; the non-interactive one (password/dev) owns the
		// credential POST / instant-login. At most one of each is expected.
		if p.Interactive() {
			a.interactive = p
		} else {
			a.password = p
		}
	}
	return a
}

// StoreDeps is the union of persistence the Auth surface needs. *db.Store
// satisfies it directly (it embeds the generated queries and owns WithTx).
type StoreDeps interface {
	SessionStore
	AdminHashStore
	WithTx(ctx context.Context, fn func(q *gen.Queries) error) error
	// SetAdminPasswordHash persists the first-run admin-password bcrypt hash. The
	// caller (the /auth/setup handler) computes the hash; the store only writes it.
	SetAdminPasswordHash(ctx context.Context, hash string) error
	// PromoteUserAdmin sets users.is_admin=true for the given user (leaving
	// can_create_sites untouched). In single-admin PASSWORD mode the admin IS the
	// instance admin, so first-run setup and every password login promote that
	// user; it is NEVER called for oidc/none users. *db.Store satisfies it via the
	// embedded generated query.
	PromoteUserAdmin(ctx context.Context, id uuid.UUID) error
	// GetGitHubConfig reads the DB-stored GitHub mirror config (the token decrypted
	// but never surfaced here — PublicConfig only needs the boolean fold). It backs
	// the EFFECTIVE githubMirrorEnabled value (DB overrides env). *db.Store
	// satisfies it directly.
	GetGitHubConfig(ctx context.Context) (db.GitHubConfig, error)
}

// CSRF exposes the CSRF guard so the composition root can mount its middleware on
// the mutating API surface and issue tokens.
func (a *Auth) CSRF() *CSRF { return a.csrf }

// Sessions exposes the session manager for advanced wiring/tests.
func (a *Auth) Sessions() *SessionManager { return a.sessions }

// RegisterRoutes mounts the auth + identity endpoints on r. The control-plane
// composition root mounts this under the same router as /api/*.
func (a *Auth) RegisterRoutes(r chi.Router) {
	r.Get("/auth/login", a.LoginStart)
	r.Get("/auth/callback", a.Callback)
	r.Post("/auth/login", a.LoginPassword) // non-interactive password/dev submit
	r.Post("/auth/setup", a.Setup)         // first-run admin-password setup (gated by setupRequired)
	r.Post("/auth/logout", a.Logout)
	r.Get("/api/me", a.Me)
	r.Get("/api/config", a.PublicConfig)
}

// ---- login-state cookie (signed, short-lived) ----

// loginState binds the OIDC handshake parameters across the redirect.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"r"`
	Expires  int64  `json:"e"` // unix seconds
}

// setLoginState writes the signed, HttpOnly, short-lived login-state cookie.
func (a *Auth) setLoginState(w http.ResponseWriter, ls loginState) error {
	payload, err := json.Marshal(ls)
	if err != nil {
		return err
	}
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := a.sign(b64)
	http.SetCookie(w, &http.Cookie{
		Name:     a.loginStateCookieName(),
		Value:    b64 + "." + sig,
		Path:     "/",
		MaxAge:   int(loginStateTTL.Seconds()),
		Secure:   a.cfg.CookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// readLoginState reads + verifies the login-state cookie, clearing it. A bad
// signature, malformed body, or expiry returns an error.
func (a *Auth) readLoginState(w http.ResponseWriter, r *http.Request) (loginState, error) {
	defer a.clearLoginState(w)
	ck, err := r.Cookie(a.loginStateCookieName())
	if err != nil {
		return loginState{}, errors.New("missing login state")
	}
	b64, sig, ok := strings.Cut(ck.Value, ".")
	if !ok {
		return loginState{}, errors.New("malformed login state")
	}
	// Constant-time signature check defends against forged state cookies.
	if subtle.ConstantTimeCompare([]byte(sig), []byte(a.sign(b64))) != 1 {
		return loginState{}, errors.New("bad login state signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return loginState{}, errors.New("malformed login state payload")
	}
	var ls loginState
	if err := json.Unmarshal(payload, &ls); err != nil {
		return loginState{}, errors.New("malformed login state json")
	}
	if time.Now().Unix() > ls.Expires {
		return loginState{}, errors.New("login state expired")
	}
	return ls, nil
}

func (a *Auth) clearLoginState(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.loginStateCookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   a.cfg.CookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) loginStateCookieName() string {
	if a.cfg.CookieSecure {
		return "__Host-" + loginStateCookie
	}
	return loginStateCookie
}

// sign returns the base64url HMAC-SHA256 of msg with the process sign key.
func (a *Auth) sign(msg string) string {
	mac := hmac.New(sha256.New, a.signKey)
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ---- handlers ----

// LoginStart begins login. The GET path serves the INTERACTIVE providers: OIDC
// (mint state+nonce+PKCE, store the signed cookie, 302 to the IdP) and the dev
// no-auth instant login. The password break-glass provider has no GET handshake
// (it uses POST /auth/login), so when only password is enabled this returns a 400
// telling the caller to POST — and importantly it does NOT disable the password
// path. An optional `?provider=` selects among enabled providers; it defaults to
// the interactive provider (the primary human path) when present.
func (a *Auth) LoginStart(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))

	// Resolve which provider this GET should drive. Default order: the interactive
	// (oidc) provider, else the non-interactive dev provider for instant login.
	prov := a.selectStartProvider(r.URL.Query().Get("provider"))
	if prov == nil {
		// No GET-capable provider is enabled (only password). Tell the caller to
		// POST credentials; this never affects the still-mounted POST path.
		writeError(w, r, http.StatusBadRequest, codeValidation, "this provider uses POST /auth/login with a credential")
		return
	}

	if !prov.Interactive() {
		// Dev no-auth (AUTH_MODE=none): instant login, no IdP redirect.
		a.completeLogin(w, r, prov, "", "", "", next)
		return
	}

	state, err := randToken()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "login could not be started")
		return
	}
	nonce, err := randToken()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "login could not be started")
		return
	}
	verifier, err := randToken()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "login could not be started")
		return
	}

	if err := a.setLoginState(w, loginState{
		State:    state,
		Nonce:    nonce,
		Verifier: verifier,
		Next:     next,
		Expires:  time.Now().Add(loginStateTTL).Unix(),
	}); err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "login could not be started")
		return
	}

	http.Redirect(w, r, prov.Start(state, nonce, verifier), http.StatusFound)
}

// selectStartProvider resolves which provider the GET /auth/login path drives. An
// explicit `?provider=` key selects that enabled provider IF it is GET-capable
// (interactive oidc or the dev no-auth provider); otherwise it falls back to the
// default order: the interactive provider first (the primary human path), then the
// dev no-auth provider. It returns nil when no GET-capable provider is enabled
// (only the password break-glass provider) so the caller can 400 without
// disabling the still-mounted POST path.
func (a *Auth) selectStartProvider(requested string) AuthProvider {
	if requested != "" {
		if p, ok := a.providers[requested]; ok && a.getCapable(p) {
			return p
		}
	}
	if a.interactive != nil {
		return a.interactive
	}
	// The dev no-auth provider is non-interactive but GET-capable (instant login).
	if a.password != nil && a.password.Key() == devProviderKey {
		return a.password
	}
	return nil
}

// getCapable reports whether a provider can be driven by GET /auth/login: the
// interactive (oidc) one, or the dev no-auth instant-login provider. The password
// provider is NOT (it needs the credential POST).
func (a *Auth) getCapable(p AuthProvider) bool {
	return p.Interactive() || p.Key() == devProviderKey
}

// LoginPassword handles the non-interactive credential submit (POST), i.e. the
// password break-glass provider (and the dev provider's POST form). The password
// is read from the form/JSON body and verified by that provider. It coexists with
// the OIDC GET flow: enabling OIDC never disables this path.
func (a *Auth) LoginPassword(w http.ResponseWriter, r *http.Request) {
	prov := a.password
	if prov == nil {
		// No non-interactive provider is enabled (oidc-only instance). The caller
		// must use the redirect flow; this does not affect GET /auth/login.
		writeError(w, r, http.StatusBadRequest, codeValidation, "this instance uses redirect-based login")
		return
	}
	next := safeNext(r.URL.Query().Get("next"))

	var password string
	// Accept either a form field or a JSON body for the SPA.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Password string `json:"password"`
			Next     string `json:"next"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err == nil {
			password = body.Password
			if body.Next != "" {
				next = safeNext(body.Next)
			}
		}
	} else {
		_ = r.ParseForm()
		password = r.PostFormValue("password")
		if n := r.PostFormValue("next"); n != "" {
			next = safeNext(n)
		}
	}

	a.completeLogin(w, r, prov, password, "", "", next)
}

// Callback completes the interactive (OIDC) login: verify the state cookie,
// exchange the code, then materialize the session. It drives the interactive
// provider; with no interactive provider enabled it 403s (no open callback).
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	if a.interactive == nil {
		writeError(w, r, http.StatusForbidden, codeForbidden, "no redirect-based login is enabled")
		return
	}
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")

	ls, err := a.readLoginState(w, r)
	if err != nil {
		writeError(w, r, http.StatusForbidden, codeForbidden, "login session is invalid or expired")
		return
	}
	// State must match the value bound in the cookie at Start (CSRF on callback).
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(ls.State)) != 1 {
		writeError(w, r, http.StatusForbidden, codeForbidden, "login state mismatch")
		return
	}
	if code == "" {
		writeError(w, r, http.StatusForbidden, codeForbidden, "missing authorization code")
		return
	}

	a.completeLogin(w, r, a.interactive, code, ls.Verifier, ls.Nonce, ls.Next)
}

// completeLogin is the shared tail of every login path: prov.Exchange ->
// access gate (inside the provider) -> upsert user+identity -> admin promotion ->
// rotate session id -> set cookies -> redirect. credential is the code (OIDC) or
// password (break-glass), ignored by the dev provider. prov is the specific
// provider this path used, so the identity is linked under the right key and the
// admin decision uses the right signal.
func (a *Auth) completeLogin(w http.ResponseWriter, r *http.Request, prov AuthProvider, credential, verifier, nonce, next string) {
	claims, err := prov.Exchange(r.Context(), credential, verifier, nonce)
	if err != nil {
		// Distinguish a bad password (401) from an access/verify reject (403).
		if errors.Is(err, ErrBadPassword) {
			writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "invalid credentials")
			return
		}
		writeError(w, r, http.StatusForbidden, codeForbidden, "login was rejected")
		return
	}

	var avatar *string // OIDC profile may carry a picture; left nil here (least data)
	user, err := a.upserter.UpsertLogin(r.Context(), UpsertLoginInput{
		Provider:    prov.Key(),
		Subject:     claims.Subject,
		Email:       claims.Email,
		DisplayName: claims.Name,
		AvatarURL:   avatar,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not establish your account")
		return
	}

	a.applyAdmin(r.Context(), prov, user.ID, claims)

	if !a.establishSession(w, r, user.ID) {
		return // establishSession already wrote the error envelope
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// applyAdmin promotes the user to is_admin when the login warrants it, reusing
// the existing PromoteUserAdmin path for BOTH cases:
//
//   - PASSWORD provider: the single admin IS the instance admin (decision #1), so
//     every successful break-glass login (re)asserts is_admin on that user.
//   - OIDC provider: the verified email is in KOTOJI_OIDC_ADMIN_EMAILS (decision
//     #3); the provider already resolved this into claims.IsAdmin.
//
// For the dev provider and non-admin OIDC users this is a no-op. A promotion
// failure is best-effort (logged-by-omission): it must not break the login it
// rides on, and the next login retries it.
func (a *Auth) applyAdmin(ctx context.Context, prov AuthProvider, userID uuid.UUID, claims Claims) {
	switch prov.Key() {
	case passwordProviderKey:
		_ = a.store.PromoteUserAdmin(ctx, userID)
	case oidcProviderKey:
		if claims.IsAdmin {
			_ = a.store.PromoteUserAdmin(ctx, userID)
		}
	}
}

// establishSession rotates a fresh server-side session for userID, sets the
// __Host- session cookie, and issues a CSRF token. It is the shared session tail
// of every authenticated entry point (login + first-run setup). On failure it
// writes the error envelope and returns false so the caller stops. ROTATE: Create
// always mints a new id, never reusing a pre-auth cookie -> defeats session
// fixation (architecture §8.1).
func (a *Auth) establishSession(w http.ResponseWriter, r *http.Request, userID uuid.UUID) bool {
	sid, err := a.sessions.Create(r.Context(), userID, r.UserAgent(), clientIP(r, a.cfg.TrustProxyHeaders))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not start your session")
		return false
	}
	a.sessions.SetCookie(w, sid)
	// Issue a CSRF token immediately so the SPA can make mutating calls.
	_, _ = a.csrf.Issue(w, r)
	return true
}

// Setup is the first-run admin-password endpoint (POST /auth/setup), mounted
// OUTSIDE the /api CSRF subtree like /auth/login. It ONLY works while the instance
// is in the setupRequired state (password mode, no env password, no DB hash); once
// a credential exists it returns 409 and can never reset the admin. On success it
// stores the bcrypt hash, ensures the admin user row exists (the same upsert path
// LoginPassword uses), establishes a session, and returns 200 so the caller is
// immediately authenticated. The password is NEVER logged.
func (a *Auth) Setup(w http.ResponseWriter, r *http.Request) {
	// GUARD: the endpoint is live ONLY during first-run. Re-checking here (not just
	// in /api/config) closes the window where two requests race the same first run:
	// the second sees the hash written by the first and gets 409.
	if !a.setupRequired(r.Context()) {
		writeError(w, r, http.StatusConflict, codeConflict, "setup already completed")
		return
	}

	var password string
	// Accept a JSON body (the SPA) or a form post. An optional `confirm` is matched
	// client-side; if present here it must equal `password` (defense in depth).
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Password string `json:"password"`
			Confirm  string `json:"confirm"`
		}
		// Decode errors fall through to the empty-password validation below; we do
		// not echo the parse error (it could carry body bytes -> never log the pw).
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err == nil {
			password = body.Password
			if body.Confirm != "" && body.Confirm != password {
				writeError(w, r, http.StatusUnprocessableEntity, codeValidation, "passwords do not match")
				return
			}
		}
	} else {
		_ = r.ParseForm()
		password = r.PostFormValue("password")
		if c := r.PostFormValue("confirm"); c != "" && c != password {
			writeError(w, r, http.StatusUnprocessableEntity, codeValidation, "passwords do not match")
			return
		}
	}

	// Validate against the shared minimum length (config.AdminPasswordMinLen).
	if len(password) < config.AdminPasswordMinLen {
		writeError(w, r, http.StatusUnprocessableEntity, codeValidation,
			fmt.Sprintf("password must be at least %d characters", config.AdminPasswordMinLen))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not set the password")
		return
	}
	password = "" // drop the plaintext as soon as it is hashed (never logged/kept)

	// ORDER MATTERS. Ensure the admin user row exists FIRST (reusing the exact
	// UpsertUser+UpsertIdentity path LoginPassword takes, in its own tx), THEN write
	// the credential hash. Writing the hash last means the setupRequired gate only
	// closes once a usable admin account already exists: a crash between the two
	// leaves the instance still in first-run state (idempotent retry), never with a
	// credential that has no account behind it.
	user, err := a.upserter.UpsertLogin(r.Context(), UpsertLoginInput{
		Provider:    passwordProviderKey,
		Subject:     devSubject,
		Email:       a.adminEmail(),
		DisplayName: "Admin",
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not establish your account")
		return
	}
	// First-run admin IS the instance admin: promote so /settings + the admin API
	// are reachable immediately after setup. Setup is only reachable when the
	// password break-glass provider is enabled and unconfigured (setupRequired),
	// so promoting unconditionally here is correct by construction.
	_ = a.store.PromoteUserAdmin(r.Context(), user.ID)
	if err := a.store.SetAdminPasswordHash(r.Context(), string(hash)); err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not set the password")
		return
	}

	// Log the freshly-configured admin straight in (same session tail as login).
	if !a.establishSession(w, r, user.ID) {
		return
	}
	writeJSON(w, http.StatusOK, setupResponse{OK: true})
}

// adminEmail returns the normalized admin email used for the setup-created admin
// row, mirroring the PasswordProvider's default (lowercased, falling back to the
// local default) so login and setup converge on the same users row.
func (a *Auth) adminEmail() string {
	return defaultIfEmpty(strings.ToLower(a.cfg.AdminEmail), "admin@kotoji.local")
}

// Logout destroys the server session and clears the cookies. Idempotent: a
// request with no session still returns 204.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if id := a.sessions.readCookie(r); id != "" {
		_ = a.sessions.Delete(r.Context(), id)
	}
	a.sessions.ClearCookie(w)
	a.csrf.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// Me returns the authenticated user + auth mode (openapi Me schema). 401 when
// anonymous. It also (re)issues a CSRF token so a freshly-loaded SPA has one.
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	u, ok := CurrentUser(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
		return
	}
	_, _ = a.csrf.Issue(w, r)
	writeJSON(w, http.StatusOK, meResponse{
		User: userJSON{
			ID:             u.UserID.String(),
			Email:          u.Email,
			DisplayName:    u.DisplayName,
			AvatarURL:      u.AvatarURL,
			IsAdmin:        u.IsAdmin,
			CanCreateSites: u.CanCreateSites,
		},
		AuthMode: string(a.cfg.AuthMode),
	})
}

// PublicConfig returns the public-safe instance config (openapi InstanceConfig).
// No auth required (security: [] in the spec).
func (a *Auth) PublicConfig(w http.ResponseWriter, r *http.Request) {
	// EFFECTIVE base domain + control base URL (WordPress-style env > DB > derived).
	// The resolver is nil on the env-set fast path / in tests, where the static cfg
	// values are used unchanged.
	baseDomain := a.cfg.BaseDomain
	controlBaseURL := a.cfg.ControlBaseURL
	if a.domain != nil {
		baseDomain = a.domain.BaseDomainFor(r)
		controlBaseURL = a.domain.ControlBaseURLFor(r)
	}
	writeJSON(w, http.StatusOK, instanceConfigJSON{
		MaxUploadBytes:    a.cfg.Zip.MaxUploadBytes,
		ZipMaxFiles:       a.cfg.Zip.MaxFiles,
		ZipMaxTotalBytes:  a.cfg.Zip.MaxTotalBytes,
		ZipMaxRatio:       a.cfg.Zip.MaxRatio,
		AllowedExtensions: a.cfg.Zip.AllowedExt,
		HandleMinLen:      a.cfg.HandleMinLen,
		HandleMaxLen:      a.cfg.HandleMaxLen,
		ReservedHandles:   reservedHandles(),
		BaseDomain:        baseDomain,
		ControlBaseURL:    controlBaseURL,
		// AuthMode is the LEGACY single representative (back-compat). AuthProviders
		// is the full ENABLED set so the login page renders EACH provider (e.g.
		// ["oidc","password"] for the break-glass config). Existing clients that
		// only read authMode keep working unchanged.
		AuthMode:           string(a.cfg.AuthMode),
		AuthProviders:      a.cfg.AuthModeStrings(),
		DefaultPublishMode: "direct",
		// The instance can mirror only when the feature is enabled AND a push token
		// is configured; the GUI keys per-site linking/sync controls off this so a
		// half-configured instance (enabled but no token) does not advertise mirroring.
		// EFFECTIVE value: DB overrides env (so a runtime admin change is reflected).
		GithubMirrorEnabled: a.githubMirrorEnabled(r.Context()),
		// True only in the first-run state: password mode, no env password, and no
		// DB hash yet. The GUI shows the first-run setup screen when this is true.
		SetupRequired: a.setupRequired(r.Context()),
	})
}

// githubMirrorEnabled computes the EFFECTIVE "can this instance mirror" flag the
// GUI gates on: the feature must be ENABLED and a push TOKEN must be present.
// DB overrides env on BOTH axes — the github_mirror_enabled key (if present)
// wins over cfg.GitHub.Enabled, and a DB-stored token (if set) wins over the env
// token. A DB read error falls back to the env-only computation (fail safe: a
// transient blip never flips the GUI to "no mirroring" if env is configured).
func (a *Auth) githubMirrorEnabled(ctx context.Context) bool {
	enabled := a.cfg.GitHub.Enabled
	tokenPresent := a.cfg.GitHub.Token != ""

	if a.store != nil {
		if gh, err := a.store.GetGitHubConfig(ctx); err == nil {
			if gh.EnabledSet {
				enabled = gh.Enabled
			}
			if gh.TokenSet {
				tokenPresent = true
			}
		}
	}
	return enabled && tokenPresent
}

// setupRequired reports whether the instance is in the first-run "set the admin
// password" state: the password provider is ENABLED (it may be enabled alongside
// oidc — break-glass) AND no env password AND no DB-stored hash. When password is
// not enabled it is always false (no setup screen). A store read error is treated
// as "not required" (fail closed: do NOT advertise an open setup endpoint on a DB
// blip — a real first-run instance reads cleanly).
func (a *Auth) setupRequired(ctx context.Context) bool {
	if !a.cfg.PasswordEnabled() {
		return false
	}
	if a.cfg.AdminPassword != "" {
		return false
	}
	_, found, err := a.store.GetAdminPasswordHash(ctx)
	if err != nil {
		return false
	}
	return !found
}

// ---- response shapes (mirror openapi.yaml) ----

type meResponse struct {
	User     userJSON `json:"user"`
	AuthMode string   `json:"authMode"`
}

// setupResponse is the small success body returned by POST /auth/setup. The
// session + CSRF cookies set alongside it are what actually authenticate the
// caller; the body just confirms completion.
type setupResponse struct {
	OK bool `json:"ok"`
}

type userJSON struct {
	ID             string  `json:"id"`
	Email          string  `json:"email"`
	DisplayName    string  `json:"displayName"`
	AvatarURL      *string `json:"avatarUrl"`
	IsAdmin        bool    `json:"isAdmin"`
	CanCreateSites bool    `json:"canCreateSites"`
}

type instanceConfigJSON struct {
	MaxUploadBytes    int64    `json:"maxUploadBytes"`
	ZipMaxFiles       int      `json:"zipMaxFiles"`
	ZipMaxTotalBytes  int64    `json:"zipMaxTotalBytes"`
	ZipMaxRatio       int      `json:"zipMaxRatio"`
	AllowedExtensions []string `json:"allowedExtensions"`
	HandleMinLen      int      `json:"handleMinLen"`
	HandleMaxLen      int      `json:"handleMaxLen"`
	ReservedHandles   []string `json:"reservedHandles"`
	BaseDomain        string   `json:"baseDomain"`
	// ControlBaseURL is the EFFECTIVE external URL of the control host (env > DB >
	// derived). The frontend reads it to build absolute links / show the configured
	// host on /settings. Always present (derived from the request on a fresh install).
	ControlBaseURL string `json:"controlBaseURL"`
	// AuthMode is the legacy single representative; AuthProviders is the full
	// enabled set ("oidc"/"password"/"none") the login page renders.
	AuthMode            string   `json:"authMode"`
	AuthProviders       []string `json:"authProviders"`
	DefaultPublishMode  string   `json:"defaultPublishMode"`
	GithubMirrorEnabled bool     `json:"githubMirrorEnabled"`
	SetupRequired       bool     `json:"setupRequired"`
}

// reservedHandles is the locked baseline blocklist (CANONICAL §5.1). Returned in
// the public config so the create form can pre-validate.
func reservedHandles() []string {
	return []string{
		"draft", "preview", "published", "www", "api", "internal",
		"host", "admin", "app", "static", "assets", "mcp",
	}
}

// ---- store-backed UserUpserter ----

// storeUpserter implements UserUpserter over the metadata Store's WithTx,
// upserting the user and linking the identity in one transaction (CANONICAL §4).
type storeUpserter struct {
	store StoreDeps
}

func (s *storeUpserter) UpsertLogin(ctx context.Context, in UpsertLoginInput) (gen.User, error) {
	var user gen.User
	err := s.store.WithTx(ctx, func(q *gen.Queries) error {
		u, err := q.UpsertUser(ctx, gen.UpsertUserParams{
			Email:       in.Email,
			DisplayName: in.DisplayName,
			AvatarUrl:   in.AvatarURL,
		})
		if err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}
		emailAtLink := in.Email
		if err := q.UpsertIdentity(ctx, gen.UpsertIdentityParams{
			UserID:      u.ID,
			Provider:    in.Provider,
			Subject:     in.Subject,
			EmailAtLink: &emailAtLink,
		}); err != nil {
			return fmt.Errorf("upsert identity: %w", err)
		}
		user = u
		return nil
	})
	if err != nil {
		return gen.User{}, err
	}
	return user, nil
}

// ---- helpers ----

// randToken returns a 256-bit URL-safe random token (state/nonce/PKCE verifier).
func randToken() (string, error) {
	b := make([]byte, stateNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// safeNext sanitizes the post-login redirect target: only same-site, absolute
// paths are allowed (open-redirect defense). Anything else falls back to
// "/dashboard" (openapi default).
func safeNext(next string) string {
	if next == "" {
		return "/dashboard"
	}
	// Reject protocol-relative ("//host") and absolute URLs ("http://..."); only
	// a leading single slash path is allowed.
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/dashboard"
	}
	return next
}

// clientIP returns the best-effort client IP for the audit/session row. When the
// proxy is trusted, the first X-Forwarded-For hop is used; else RemoteAddr.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, ok := strings.Cut(xff, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	return r.RemoteAddr
}

// deriveSignKey produces the HMAC key for the login-state cookie. It binds to the
// admin password (when present) so restarts with the same config keep cookies
// valid; otherwise a per-process random key is used (login-state is short-lived,
// so a key roll only invalidates in-flight logins).
func deriveSignKey(cfg config.Config) []byte {
	seed := cfg.AdminPassword + "|" + cfg.OIDC.ClientSecret + "|" + cfg.ControlBaseURL
	if strings.Trim(seed, "|") == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return b
	}
	sum := sha256.Sum256([]byte("kotoji-login-state|" + seed))
	return sum[:]
}

// ProviderFor builds the single AuthProvider matching cfg.AuthMode (the LEGACY
// representative). It is retained for back-compat / single-provider callers and
// is implemented in terms of buildProvider. New composition code should call
// ProvidersFor to build the full enabled set.
func ProviderFor(ctx context.Context, cfg config.Config, store AdminHashStore) (AuthProvider, error) {
	return buildProvider(ctx, cfg.AuthMode, cfg, store)
}

// ProvidersFor builds the SET of enabled AuthProviders from cfg.AuthModes
// (decision #1: oidc + password may be enabled concurrently). OIDC performs
// discovery (network) so it takes a ctx; dev/password are local. The composition
// root calls this once and passes the slice to NewWithProviders. store supplies
// the DB-backed admin-password hash to the password provider (first-run setup);
// it is unused by the OIDC/dev providers. Returns the providers in cfg.AuthModes
// order (normalized: oidc, password, none).
func ProvidersFor(ctx context.Context, cfg config.Config, store AdminHashStore) ([]AuthProvider, error) {
	providers := make([]AuthProvider, 0, len(cfg.AuthModes))
	for _, m := range cfg.AuthModes {
		p, err := buildProvider(ctx, m, cfg, store)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, nil
}

// buildProvider constructs one provider for the given mode.
func buildProvider(ctx context.Context, mode config.AuthMode, cfg config.Config, store AdminHashStore) (AuthProvider, error) {
	switch mode {
	case config.AuthModeOIDC:
		return NewOIDCProvider(ctx, cfg.OIDC)
	case config.AuthModePassword:
		return NewPasswordProvider(cfg, store)
	case config.AuthModeNone:
		return NewDevProvider(cfg), nil
	default:
		return nil, fmt.Errorf("auth: unknown auth mode %q", mode)
	}
}
