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

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db/gen"
)

// loginStateTTL bounds how long a started login may take to complete. After this
// the state cookie expires and the callback is rejected (replay/abandon defense).
const loginStateTTL = 10 * time.Minute

// loginStateCookie is the (un-prefixed) name of the short-lived signed cookie
// that binds state+nonce+PKCE+next across the redirect to the IdP.
const loginStateCookie = "kotoji_login"

// stateNonceBytes is the entropy of the login state and nonce (256 bits each).
const stateNonceBytes = 32

// Auth is the assembled auth surface: provider + sessions + CSRF + handlers. It
// is constructed once at composition and exposes RegisterRoutes + Middleware.
type Auth struct {
	cfg      config.Config
	provider AuthProvider
	sessions *SessionManager
	csrf     *CSRF
	upserter UserUpserter

	// signKey signs the login-state cookie (HMAC). Derived from the admin
	// password / a random per-process key so the cookie is tamper-evident.
	signKey []byte
}

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

// New assembles the Auth surface. provider is chosen by the caller per
// config.AuthMode (use ProviderFor). store backs sessions and the login upsert.
func New(cfg config.Config, store StoreDeps, provider AuthProvider) *Auth {
	return &Auth{
		cfg:      cfg,
		provider: provider,
		sessions: NewSessionManager(store, cfg.SessionCookieName, cfg.SessionTTL, cfg.CookieSecure),
		csrf:     NewCSRF(cfg.CSRFCookieName, cfg.CookieSecure),
		upserter: &storeUpserter{store: store},
		signKey:  deriveSignKey(cfg),
	}
}

// StoreDeps is the union of persistence the Auth surface needs. *db.Store
// satisfies it directly (it embeds the generated queries and owns WithTx).
type StoreDeps interface {
	SessionStore
	WithTx(ctx context.Context, fn func(q *gen.Queries) error) error
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

// LoginStart begins login. For an interactive provider (OIDC) it mints
// state+nonce+PKCE, stores them in the signed cookie, and 302s to the IdP. For a
// non-interactive provider (dev no-auth) it logs the user straight in.
func (a *Auth) LoginStart(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))

	if !a.provider.Interactive() {
		// Dev no-auth: instant login. (Password mode uses POST /auth/login.)
		if a.cfg.AuthMode == config.AuthModeNone {
			a.completeLogin(w, r, "", "", "", next)
			return
		}
		// Password mode has no GET handshake; tell the caller to POST credentials.
		writeError(w, r, http.StatusBadRequest, codeValidation, "password mode: POST /auth/login with a password")
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

	http.Redirect(w, r, a.provider.Start(state, nonce, verifier), http.StatusFound)
}

// LoginPassword handles the non-interactive password/dev submit (POST). The
// password is read from the form/JSON body and verified by the provider.
func (a *Auth) LoginPassword(w http.ResponseWriter, r *http.Request) {
	if a.provider.Interactive() {
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

	a.completeLogin(w, r, password, "", "", next)
}

// Callback completes the interactive (OIDC) login: verify the state cookie,
// exchange the code, then materialize the session.
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
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

	a.completeLogin(w, r, code, ls.Verifier, ls.Nonce, ls.Next)
}

// completeLogin is the shared tail of every login path: provider.Exchange ->
// allowlist (inside the provider) -> upsert user+identity -> rotate session id ->
// set cookies -> redirect. credential is the code (OIDC) or password (password
// mode), ignored by the dev provider.
func (a *Auth) completeLogin(w http.ResponseWriter, r *http.Request, credential, verifier, nonce, next string) {
	claims, err := a.provider.Exchange(r.Context(), credential, verifier, nonce)
	if err != nil {
		// Distinguish a bad password (401) from an allowlist/verify reject (403).
		if errors.Is(err, ErrBadPassword) {
			writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "invalid credentials")
			return
		}
		writeError(w, r, http.StatusForbidden, codeForbidden, "login was rejected")
		return
	}

	var avatar *string // OIDC profile may carry a picture; left nil here (least data)
	user, err := a.upserter.UpsertLogin(r.Context(), UpsertLoginInput{
		Provider:    a.provider.Key(),
		Subject:     claims.Subject,
		Email:       claims.Email,
		DisplayName: claims.Name,
		AvatarURL:   avatar,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not establish your account")
		return
	}

	// ROTATE: always mint a fresh session id (Create generates a new one), never
	// reuse a pre-auth cookie value -> defeats session fixation (architecture §8.1).
	sid, err := a.sessions.Create(r.Context(), user.ID, r.UserAgent(), clientIP(r, a.cfg.TrustProxyHeaders))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not start your session")
		return
	}
	a.sessions.SetCookie(w, sid)
	// Issue a CSRF token immediately so the SPA can make mutating calls.
	_, _ = a.csrf.Issue(w, r)

	http.Redirect(w, r, next, http.StatusFound)
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
	writeJSON(w, http.StatusOK, instanceConfigJSON{
		MaxUploadBytes:     a.cfg.Zip.MaxUploadBytes,
		ZipMaxFiles:        a.cfg.Zip.MaxFiles,
		ZipMaxTotalBytes:   a.cfg.Zip.MaxTotalBytes,
		ZipMaxRatio:        a.cfg.Zip.MaxRatio,
		AllowedExtensions:  a.cfg.Zip.AllowedExt,
		HandleMinLen:       a.cfg.HandleMinLen,
		HandleMaxLen:       a.cfg.HandleMaxLen,
		ReservedHandles:    reservedHandles(),
		BaseDomain:         a.cfg.BaseDomain,
		AuthMode:           string(a.cfg.AuthMode),
		DefaultPublishMode: "direct",
		// The instance can mirror only when the feature is enabled AND a push token
		// is configured; the GUI keys per-site linking/sync controls off this so a
		// half-configured instance (enabled but no token) does not advertise mirroring.
		GithubMirrorEnabled: a.cfg.GitHub.Enabled && a.cfg.GitHub.Token != "",
	})
}

// ---- response shapes (mirror openapi.yaml) ----

type meResponse struct {
	User     userJSON `json:"user"`
	AuthMode string   `json:"authMode"`
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
	MaxUploadBytes      int64    `json:"maxUploadBytes"`
	ZipMaxFiles         int      `json:"zipMaxFiles"`
	ZipMaxTotalBytes    int64    `json:"zipMaxTotalBytes"`
	ZipMaxRatio         int      `json:"zipMaxRatio"`
	AllowedExtensions   []string `json:"allowedExtensions"`
	HandleMinLen        int      `json:"handleMinLen"`
	HandleMaxLen        int      `json:"handleMaxLen"`
	ReservedHandles     []string `json:"reservedHandles"`
	BaseDomain          string   `json:"baseDomain"`
	AuthMode            string   `json:"authMode"`
	DefaultPublishMode  string   `json:"defaultPublishMode"`
	GithubMirrorEnabled bool     `json:"githubMirrorEnabled"`
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

// ProviderFor builds the AuthProvider matching cfg.AuthMode. OIDC performs
// discovery (network) so it takes a ctx; dev/password are local. The composition
// root calls this once and passes the result to New.
func ProviderFor(ctx context.Context, cfg config.Config) (AuthProvider, error) {
	switch cfg.AuthMode {
	case config.AuthModeOIDC:
		return NewOIDCProvider(ctx, cfg.OIDC)
	case config.AuthModePassword:
		return NewPasswordProvider(cfg)
	case config.AuthModeNone:
		return NewDevProvider(cfg), nil
	default:
		return nil, fmt.Errorf("auth: unknown auth mode %q", cfg.AuthMode)
	}
}
