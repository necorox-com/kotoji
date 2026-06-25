// Package config loads and validates the backend's runtime configuration from
// the environment. All variables use the KOTOJI_ prefix and map 1:1 to the env
// matrix in docs/architecture.md §6. Load fails fast: in production it refuses
// to boot with missing/invalid required values; in development it fills sane
// defaults so the stack runs with zero external services.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment is the deployment environment. It gates how strict Load is.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
)

// RunMode selects which HTTP servers boot. See docs/architecture.md §4.2.
type RunMode string

const (
	// RunModeAll serves both the control plane (:8080) and the data plane (:8081).
	RunModeAll RunMode = "all"
	// RunModeControl serves only the control plane (REST API + auth + MCP).
	RunModeControl RunMode = "control"
	// RunModeServe serves only the data plane (static hosting).
	RunModeServe RunMode = "serve"
)

// TLSMode selects how the binary terminates TLS. See docs/architecture.md §4.5.
type TLSMode string

const (
	// TLSModeOff is the DEFAULT: kotoji speaks plain HTTP on the existing
	// control/serve ports and TLS (if any) is terminated by an external edge
	// (Cloudflare, the Traefik overlay, NPM, ...). This is today's behavior and
	// keeps the live, Cloudflare-fronted deployment byte-for-byte unchanged.
	TLSModeOff TLSMode = "off"
	// TLSModeAuto makes kotoji TERMINATE TLS ITSELF via CertMagic on-demand:
	// it binds :443 (single listener fronting both planes via Host routing) and
	// :80 (HTTP-01 challenge + HTTPS redirect), issuing a per-host cert on the
	// fly for any host this instance is authoritative for. Requires RUN_MODE=all.
	TLSModeAuto TLSMode = "auto"
)

// TLSCA selects which ACME certificate authority on-demand issuance uses.
type TLSCA string

const (
	// TLSCAProd is the DEFAULT: the CA's production directory (real, publicly
	// trusted certs; Let's Encrypt production by default).
	TLSCAProd TLSCA = "prod"
	// TLSCAStaging uses the CA's STAGING directory (untrusted test certs, far
	// higher rate limits) so an operator can validate the on-demand flow end to
	// end without burning the production issuance budget.
	TLSCAStaging TLSCA = "staging"
)

// AuthMode selects the authentication provider implementation.
type AuthMode string

const (
	// AuthModeOIDC uses an external OIDC provider (Google by default).
	AuthModeOIDC AuthMode = "oidc"
	// AuthModePassword uses a single bcrypt-checked admin password.
	AuthModePassword AuthMode = "password"
	// AuthModeNone disables auth (development only).
	AuthModeNone AuthMode = "none"
)

// AdminPasswordMinLen is the minimum length of the single-admin password,
// enforced both for the env-provided password and the first-run setup flow.
// Centralized here so the env validation and the /auth/setup handler agree.
const AdminPasswordMinLen = 8

// Default values for optional settings. Centralized so there are no magic
// literals scattered through Load.
const (
	defaultControlAddr      = ":8080"
	defaultServeAddr        = ":8081"
	defaultTLSAddr          = ":443"      // CertMagic HTTPS listener (KOTOJI_TLS_MODE=auto)
	defaultTLSHTTPAddr      = ":80"       // ACME HTTP-01 + HTTPS-redirect listener (auto)
	defaultCertMagicSubdir  = "certmagic" // ${KOTOJI_DATA_DIR}/certmagic for cert/key storage
	defaultBaseDomain       = "hosting.localhost"
	defaultControlBaseURL   = "http://hosting.localhost:8080"
	defaultDBMaxConns       = 10
	defaultDataDir          = "/data"
	defaultGitBin           = "git"
	defaultOIDCIssuer       = "https://accounts.google.com"
	defaultSessionCookie    = "kotoji_session"
	defaultSessionTTL       = 720 * time.Hour // 30d
	defaultCSRFCookie       = "kotoji_csrf"
	defaultMCPPath          = "/mcp"
	defaultMCPTokenTTL      = 2160 * time.Hour // 90d
	defaultMaxUploadBytes   = 52428800         // 50MB
	defaultZipMaxTotalBytes = 209715200        // 200MB
	defaultZipMaxFiles      = 2000
	defaultZipMaxRatio      = 100
	defaultZipMaxEntryBytes = 52428800 // 50MiB per-entry uncompressed cap (site.DefaultMaxEntryUncompressed)
	defaultZipAllowedExt    = ".html,.htm,.css,.js,.mjs,.json,.svg,.png,.jpg,.jpeg,.gif,.webp,.ico,.woff,.woff2,.ttf,.txt,.md,.map,.xml,.csv,.wasm"
	defaultSiteQuotaBytes   = 524288000 // 500MB
	defaultUserSiteQuota    = 50
	defaultRateLimitAPIRPS  = 20
	defaultRateLimitServeR  = 100
	defaultLogLevel         = "info"
	defaultLogFormat        = "json"
	defaultHandleMinLen     = 2
	defaultHandleMaxLen     = 40
	defaultSoftDeleteGrace  = 720 * time.Hour // 30d (decision #3)
	defaultOpsInterval      = time.Hour       // background ops scheduler tick
)

// OIDCConfig holds OIDC provider settings (used when the oidc provider is enabled).
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// AllowedDomains is the set of hosted domains a verified email may belong to
	// (matched against the email domain part and, when present, the Google `hd`
	// claim). Lowercased at parse. Empty disables the domain gate.
	AllowedDomains []string
	// AllowedEmails is an explicit, case-insensitive exact-email allowlist.
	// Lowercasing is applied by the provider at gate time. Empty disables it.
	AllowedEmails []string
	// AdminEmails are verified OIDC emails promoted to is_admin on upsert/login.
	// Empty means no OIDC user is auto-promoted.
	AdminEmails []string
}

// OIDCEnvSet records, per OIDC field, whether the corresponding env var was
// EXPLICITLY set (non-empty). It gates the WordPress-style precedence (env
// OVERRIDES DB) the runtime-configurable OIDC config layer (internal/oidccfg)
// applies: an env-set field WINS and is rendered read-only ("set via environment"),
// while an env-empty field falls through to the instance_settings DB value. This
// mirrors config.BaseDomainEnvSet / ControlBaseURLEnvSet for the domain config.
//
// AuthModeEnvSet is the analogous flag for KOTOJI_AUTH_MODE: when true the enabled
// provider SET is pinned by the env (locked) and the GUI cannot toggle OIDC on/off;
// when false the effective set is { password (always, break-glass) + oidc iff the
// DB enables it with credentials present } (decision #2).
type OIDCEnvSet struct {
	Issuer         bool
	ClientID       bool
	ClientSecret   bool
	RedirectURL    bool
	AllowedDomains bool
	AllowedEmails  bool
	AdminEmails    bool
	AuthMode       bool
}

// GitHubMirror holds the optional GitHub mirror/backup settings.
type GitHubMirror struct {
	Enabled       bool
	Token         string // PAT or app token for push/repo-create
	Org           string // org/owner for created repos
	WebhookSecret string // HMAC secret for /api/webhooks/github
}

// ZipLimits bounds zip upload extraction (security-critical, see §8.1).
type ZipLimits struct {
	MaxUploadBytes int64
	MaxTotalBytes  int64
	MaxFiles       int
	MaxRatio       int
	// MaxEntryBytes is the per-entry UNCOMPRESSED cap (a single file inside the zip).
	// Previously a silent ~50MiB default in the site layer; now operator-configurable
	// via KOTOJI_ZIP_MAX_ENTRY_BYTES so a large single asset can be allowed/tightened
	// without a code change. Maps to site.ZipConfig.MaxEntryUncompressed (M7).
	MaxEntryBytes int64
	AllowedExt    []string
}

// Config is the fully-parsed, validated backend configuration.
type Config struct {
	Env  Environment
	Mode RunMode

	ControlAddr string
	ServeAddr   string

	// --- kotoji-native on-demand TLS (KOTOJI_TLS_MODE, §4.5 third deploy mode) ---
	// TLSMode is "off" (DEFAULT — plain HTTP behind an external edge) or "auto"
	// (kotoji terminates TLS itself via CertMagic on-demand). In auto mode the
	// binary additionally binds TLSAddr (:443) + TLSHTTPAddr (:80); off mode is a
	// pure no-op that leaves the existing :8080/:8081 HTTP path untouched.
	TLSMode     TLSMode
	TLSCA       TLSCA  // prod (default) | staging — which ACME directory to use.
	TLSAddr     string // HTTPS listen addr in auto mode (default :443).
	TLSHTTPAddr string // ACME HTTP-01 + redirect listen addr in auto mode (default :80).
	// ACMEEmail is the OPTIONAL Let's Encrypt account email. It may be empty at
	// boot (the on-demand flow works without it) and set later via env/UI. Shared
	// name with the edge overlay's KOTOJI_ACME_EMAIL so operators learn one var.
	ACMEEmail string
	// CertMagicStorageDir is ${KOTOJI_DATA_DIR}/certmagic — where issued certs/keys
	// + the ACME account persist so they survive restarts on the existing volume.
	CertMagicStorageDir string

	BaseDomain     string // base for {handle}[--{branch}].<base> parsing
	ControlBaseURL string // external URL of the control host
	// ControlHost is derived from ControlBaseURL's host (no port) and is the
	// host-only Domain for the session cookie. It must never be the wildcard
	// parent (see §8.1 isolation).
	ControlHost string

	// BaseDomainEnvSet / ControlBaseURLEnvSet record whether KOTOJI_BASE_DOMAIN /
	// KOTOJI_CONTROL_BASE_URL were EXPLICITLY set in the environment (non-empty),
	// as opposed to falling back to the package default. They gate the WordPress-
	// style precedence (env OVERRIDES DB): when true the value is LOCKED (read-only
	// in the admin GUI) and the static startup value is used per request with no
	// DB lookup — keeping the live deployment (both set) on today's fast path. When
	// false the effective value is resolved dynamically (DB > derived).
	BaseDomainEnvSet     bool
	ControlBaseURLEnvSet bool

	DatabaseURL string
	DBMaxConns  int
	AutoMigrate bool // run embedded goose migrations on boot (default true)

	DataDir string
	GitBin  string

	// AuthMode is the LEGACY single-mode value, retained for back-compat on the
	// /api/me + /api/config `authMode` field and the older single-provider call
	// sites. It is the representative of the enabled set (see AuthModes): "none"
	// when none is enabled, else "oidc" if oidc is enabled (the primary human
	// path), else "password". New code should branch on AuthModes / the Enabled*
	// helpers, never on this single value.
	AuthMode AuthMode
	// AuthModes is the SET of enabled auth providers parsed from KOTOJI_AUTH_MODE
	// (a comma-list, e.g. "oidc,password"). A single legacy value parses to a
	// one-element set. "none" is exclusive (cannot be combined). Order is
	// normalized (oidc, password, none) so it is stable for the public config.
	AuthModes []AuthMode
	OIDC      OIDCConfig
	// OIDCEnvSet records which OIDC fields (and KOTOJI_AUTH_MODE) were EXPLICITLY
	// set in the env. The runtime-config layer (internal/oidccfg) uses it for the
	// per-field env-OVERRIDES-DB precedence + the GUI "locked" flags. An env-set
	// field is read-only; an env-empty one is editable (DB value applies).
	OIDCEnvSet    OIDCEnvSet
	AdminPassword string // bcrypt-checked single-admin password (password provider)
	AdminEmail    string

	SessionCookieName   string
	SessionTTL          time.Duration
	SessionCookieDomain string
	CookieSecure        bool // Secure attribute on cookies (true in production)
	CSRFCookieName      string

	MCPEnabled  bool
	MCPPath     string
	MCPTokenTTL time.Duration

	GitHub GitHubMirror

	// SecretKey is the raw KOTOJI_SECRET_KEY env value (hex/base64, empty when
	// unset). It is the master key used to encrypt secrets at rest (the DB-stored
	// GitHub PAT). The composition root resolves it to a 32-byte key via
	// secretbox.ResolveKey (with a derived fallback); config keeps only the raw
	// string so this package carries no crypto dependency.
	SecretKey string

	Zip            ZipLimits
	SiteQuotaBytes int64
	UserSiteQuota  int

	RateLimitAPIRPS   int
	RateLimitServeRPS int

	// Operability / background jobs (architecture.md §8.4, decision #3).
	SoftDeleteGrace time.Duration // retention before the reaper reclaims a soft-deleted site
	BackupDir       string        // where `git bundle` backups are written before disk reclaim
	OpsInterval     time.Duration // background ops scheduler tick

	CORSAllowedOrigins []string
	// CORSOriginsEnvSet records whether KOTOJI_CORS_ALLOWED_ORIGINS was explicitly
	// configured. When false the default origin is the (effective) control base URL,
	// which the control plane may resolve dynamically on the env-empty path.
	CORSOriginsEnvSet bool

	LogLevel  string
	LogFormat string

	HandleMinLen int
	HandleMaxLen int

	PublishedPublic   bool
	TrustProxyHeaders bool
}

// servesControl reports whether the control plane must boot in this mode.
func (c Config) servesControl() bool { return c.Mode == RunModeAll || c.Mode == RunModeControl }

// ServesControl reports whether the control plane must boot in this mode.
func (c Config) ServesControl() bool { return c.servesControl() }

// ServesData reports whether the data plane must boot in this mode.
func (c Config) ServesData() bool { return c.Mode == RunModeAll || c.Mode == RunModeServe }

// ServesTLS reports whether kotoji-native on-demand TLS must boot: TLS mode is
// "auto" AND the binary fronts BOTH planes (RUN_MODE=all). It is the single guard
// the entry point branches on to start the :443/:80 CertMagic listeners; when it
// is false NOTHING new happens (today's plain-HTTP path is 100% preserved).
func (c Config) ServesTLS() bool { return c.TLSMode == TLSModeAuto && c.Mode == RunModeAll }

// TLSStaging reports whether the staging ACME directory is selected (safe testing).
func (c Config) TLSStaging() bool { return c.TLSCA == TLSCAStaging }

// IsProduction reports whether the environment is production.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// AuthModeEnabled reports whether the given provider mode is in the enabled set.
// BACK-COMPAT: a Config built by hand (e.g. in tests) that sets only the legacy
// single AuthMode field — never populating AuthModes — is treated as a one-element
// set of that legacy value, so the Enabled* helpers stay correct without forcing
// every caller through config.Load.
func (c Config) AuthModeEnabled(m AuthMode) bool {
	modes := c.AuthModes
	if len(modes) == 0 && c.AuthMode != "" {
		modes = []AuthMode{c.AuthMode}
	}
	for _, em := range modes {
		if em == m {
			return true
		}
	}
	return false
}

// OIDCEnabled reports whether the OIDC provider is in the enabled set.
func (c Config) OIDCEnabled() bool { return c.AuthModeEnabled(AuthModeOIDC) }

// PasswordEnabled reports whether the password (break-glass) provider is enabled.
func (c Config) PasswordEnabled() bool { return c.AuthModeEnabled(AuthModePassword) }

// NoAuthEnabled reports whether the dev no-auth provider is enabled.
func (c Config) NoAuthEnabled() bool { return c.AuthModeEnabled(AuthModeNone) }

// AuthModeStrings returns the enabled provider modes as wire strings, in the
// normalized (oidc, password, none) order — the value exposed as authProviders
// in the public config so the login page renders each enabled provider. It uses
// the same legacy-AuthMode fallback as AuthModeEnabled (see there).
func (c Config) AuthModeStrings() []string {
	modes := c.AuthModes
	if len(modes) == 0 && c.AuthMode != "" {
		modes = []AuthMode{c.AuthMode}
	}
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		out = append(out, string(m))
	}
	return out
}

// getenv is the indirection seam: tests inject a map-backed lookup so they
// never touch the real process environment.
type getenv func(key string) (string, bool)

// osGetenv adapts os.LookupEnv to the getenv signature.
func osGetenv(key string) (string, bool) { return os.LookupEnv(key) }

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return load(osGetenv)
}

// LoadFromMap loads configuration from an explicit map. It is the testable core;
// production uses Load.
func LoadFromMap(env map[string]string) (Config, error) {
	return load(func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	})
}

// load is the shared implementation parameterized over the env source.
func load(get getenv) (Config, error) {
	c := Config{}

	// --- environment & mode ---
	env := strings.ToLower(getString(get, "KOTOJI_ENV", string(EnvDevelopment)))
	switch Environment(env) {
	case EnvDevelopment, EnvProduction:
		c.Env = Environment(env)
	default:
		return Config{}, fmt.Errorf("KOTOJI_ENV: invalid value %q (want development|production)", env)
	}
	prod := c.Env == EnvProduction

	mode := strings.ToLower(getString(get, "KOTOJI_RUN_MODE", string(RunModeAll)))
	switch RunMode(mode) {
	case RunModeAll, RunModeControl, RunModeServe:
		c.Mode = RunMode(mode)
	default:
		return Config{}, fmt.Errorf("KOTOJI_RUN_MODE: invalid value %q (want all|control|serve)", mode)
	}

	// --- listen addresses ---
	c.ControlAddr = getString(get, "KOTOJI_CONTROL_ADDR", defaultControlAddr)
	c.ServeAddr = getString(get, "KOTOJI_SERVE_ADDR", defaultServeAddr)

	// --- kotoji-native on-demand TLS (opt-in; DEFAULT off keeps today's behavior) ---
	tlsMode := strings.ToLower(getString(get, "KOTOJI_TLS_MODE", string(TLSModeOff)))
	switch TLSMode(tlsMode) {
	case TLSModeOff, TLSModeAuto:
		c.TLSMode = TLSMode(tlsMode)
	default:
		return Config{}, fmt.Errorf("KOTOJI_TLS_MODE: invalid value %q (want off|auto)", tlsMode)
	}
	tlsCA := strings.ToLower(getString(get, "KOTOJI_TLS_CA", string(TLSCAProd)))
	switch TLSCA(tlsCA) {
	case TLSCAProd, TLSCAStaging:
		c.TLSCA = TLSCA(tlsCA)
	default:
		return Config{}, fmt.Errorf("KOTOJI_TLS_CA: invalid value %q (want prod|staging)", tlsCA)
	}
	c.TLSAddr = getString(get, "KOTOJI_TLS_ADDR", defaultTLSAddr)
	c.TLSHTTPAddr = getString(get, "KOTOJI_TLS_HTTP_ADDR", defaultTLSHTTPAddr)
	// Optional account email; settable later via env/UI. Empty is valid.
	c.ACMEEmail = getString(get, "KOTOJI_ACME_EMAIL", "")

	// --- domains / URLs ---
	c.BaseDomain = getString(get, "KOTOJI_BASE_DOMAIN", defaultBaseDomain)
	c.ControlBaseURL = getString(get, "KOTOJI_CONTROL_BASE_URL", defaultControlBaseURL)
	// Record whether each was EXPLICITLY set (non-empty) in the env. This gates the
	// WordPress-style precedence: an env-set value is LOCKED (read-only in the GUI)
	// and used statically; an unset one is resolved dynamically (DB > derived).
	c.BaseDomainEnvSet = envSet(get, "KOTOJI_BASE_DOMAIN")
	c.ControlBaseURLEnvSet = envSet(get, "KOTOJI_CONTROL_BASE_URL")

	// Derive the bare control host from CONTROL_BASE_URL. Used as the host-only
	// cookie domain; it must NOT be the wildcard parent domain.
	host, err := deriveHost(c.ControlBaseURL)
	if err != nil {
		return Config{}, fmt.Errorf("KOTOJI_CONTROL_BASE_URL: %w", err)
	}
	c.ControlHost = host

	// --- database ---
	c.DatabaseURL = getString(get, "KOTOJI_DATABASE_URL", "")
	c.DBMaxConns = getInt(get, "KOTOJI_DB_MAX_CONNS", defaultDBMaxConns)
	c.AutoMigrate = getBool(get, "KOTOJI_AUTO_MIGRATE", true)

	// --- storage ---
	c.DataDir = getString(get, "KOTOJI_DATA_DIR", defaultDataDir)
	c.GitBin = getString(get, "KOTOJI_GIT_BIN", defaultGitBin)
	// Cert/key + ACME-account storage lives under the existing data volume so
	// on-demand certs survive restarts. Derived (not its own env) to keep the
	// persisted material co-located with the rest of /data by construction.
	c.CertMagicStorageDir = filepath.Join(c.DataDir, defaultCertMagicSubdir)

	// --- auth ---
	// KOTOJI_AUTH_MODE is a comma-SET of enabled providers (e.g. "oidc,password"
	// for OIDC + break-glass password). A single legacy value ("oidc"/"password"/
	// "none") parses to a one-element set unchanged. "none" is exclusive.
	//
	// PRECEDENCE (decision #2): when KOTOJI_AUTH_MODE is SET it WINS — the enabled
	// provider set is pinned by the env (locked) and the runtime config layer cannot
	// toggle providers. When UNSET the default is "password" (break-glass first-run),
	// and the oidccfg layer ADDS oidc dynamically iff the admin enables it in the DB
	// with credentials present — so a fresh install with NO auth env boots into the
	// password setup flow and the admin turns on Google from /settings. The OLD
	// default was "oidc"; it is intentionally changed to "password" so the zero-env
	// install is usable (oidc-with-no-credentials would be unbootable).
	authModeEnvSet := envSet(get, "KOTOJI_AUTH_MODE")
	c.OIDCEnvSet.AuthMode = authModeEnvSet
	modes, primary, err := parseAuthModes(getString(get, "KOTOJI_AUTH_MODE", string(AuthModePassword)))
	if err != nil {
		return Config{}, err
	}
	c.AuthModes = modes
	c.AuthMode = primary // legacy representative (back-compat for authMode field)

	// AllowedDomains: the new KOTOJI_OIDC_ALLOWED_DOMAINS list, OR'd with the
	// legacy single KOTOJI_AUTH_GOOGLE_HD (hd hint) so an existing single-domain
	// config keeps working without rename. Deduped + lowercased by lowerCSV.
	domains := getCSV(get, "KOTOJI_OIDC_ALLOWED_DOMAINS")
	if hd := strings.TrimSpace(getString(get, "KOTOJI_AUTH_GOOGLE_HD", "")); hd != "" {
		domains = append(domains, hd)
	}
	c.OIDC = OIDCConfig{
		// Issuer/client/secret/redirect: accept the new KOTOJI_OIDC_* names
		// (decisions) AND the legacy KOTOJI_AUTH_OIDC_* names already in the wild.
		Issuer:       firstNonEmpty(getString(get, "KOTOJI_OIDC_ISSUER", ""), getString(get, "KOTOJI_AUTH_OIDC_ISSUER", defaultOIDCIssuer)),
		ClientID:     firstNonEmpty(getString(get, "KOTOJI_OIDC_CLIENT_ID", ""), getString(get, "KOTOJI_AUTH_OIDC_CLIENT_ID", "")),
		ClientSecret: firstNonEmpty(getString(get, "KOTOJI_OIDC_CLIENT_SECRET", ""), getString(get, "KOTOJI_AUTH_OIDC_CLIENT_SECRET", "")),
		RedirectURL:  firstNonEmpty(getString(get, "KOTOJI_OIDC_REDIRECT_URL", ""), getString(get, "KOTOJI_AUTH_OIDC_REDIRECT_URL", defaultRedirect(c.ControlBaseURL))),
		// AllowedEmails: the new KOTOJI_OIDC_ALLOWED_EMAILS preferred, with the
		// legacy KOTOJI_AUTH_ALLOWED_EMAILS as the fallback name.
		AllowedDomains: lowerCSV(domains),
		AllowedEmails:  lowerCSV(firstNonEmptyCSV(getCSV(get, "KOTOJI_OIDC_ALLOWED_EMAILS"), getCSV(get, "KOTOJI_AUTH_ALLOWED_EMAILS"))),
		AdminEmails:    lowerCSV(getCSV(get, "KOTOJI_OIDC_ADMIN_EMAILS")),
	}
	// Per-field env-set flags for the WordPress-style env > DB precedence the
	// oidccfg layer applies. A field counts as env-set when EITHER the new
	// KOTOJI_OIDC_* name OR its legacy KOTOJI_AUTH_* alias is set (so an existing
	// env-configured deployment keeps every field locked exactly as today). The hd
	// alias (KOTOJI_AUTH_GOOGLE_HD) feeds AllowedDomains, so it locks that field too.
	c.OIDCEnvSet = OIDCEnvSet{
		Issuer:         envSet(get, "KOTOJI_OIDC_ISSUER") || envSet(get, "KOTOJI_AUTH_OIDC_ISSUER"),
		ClientID:       envSet(get, "KOTOJI_OIDC_CLIENT_ID") || envSet(get, "KOTOJI_AUTH_OIDC_CLIENT_ID"),
		ClientSecret:   envSet(get, "KOTOJI_OIDC_CLIENT_SECRET") || envSet(get, "KOTOJI_AUTH_OIDC_CLIENT_SECRET"),
		RedirectURL:    envSet(get, "KOTOJI_OIDC_REDIRECT_URL") || envSet(get, "KOTOJI_AUTH_OIDC_REDIRECT_URL"),
		AllowedDomains: envSet(get, "KOTOJI_OIDC_ALLOWED_DOMAINS") || envSet(get, "KOTOJI_AUTH_GOOGLE_HD"),
		AllowedEmails:  envSet(get, "KOTOJI_OIDC_ALLOWED_EMAILS") || envSet(get, "KOTOJI_AUTH_ALLOWED_EMAILS"),
		AdminEmails:    envSet(get, "KOTOJI_OIDC_ADMIN_EMAILS"),
		AuthMode:       authModeEnvSet,
	}
	c.AdminPassword = getString(get, "KOTOJI_AUTH_ADMIN_PASSWORD", "")
	c.AdminEmail = getString(get, "KOTOJI_AUTH_ADMIN_EMAIL", "admin@kotoji.local")

	// --- session / cookies ---
	c.SessionCookieName = getString(get, "KOTOJI_SESSION_COOKIE_NAME", defaultSessionCookie)
	ttl, err := getDuration(get, "KOTOJI_SESSION_TTL", defaultSessionTTL)
	if err != nil {
		return Config{}, err
	}
	c.SessionTTL = ttl
	// Session cookie domain defaults to the derived bare control host (host-only).
	c.SessionCookieDomain = getString(get, "KOTOJI_SESSION_COOKIE_DOMAIN", c.ControlHost)
	// Cookies are Secure in production by default; dev over http defaults off.
	c.CookieSecure = getBool(get, "KOTOJI_COOKIE_SECURE", prod)
	c.CSRFCookieName = getString(get, "KOTOJI_CSRF_COOKIE_NAME", defaultCSRFCookie)

	// --- MCP ---
	c.MCPEnabled = getBool(get, "KOTOJI_MCP_ENABLED", true)
	c.MCPPath = getString(get, "KOTOJI_MCP_PATH", defaultMCPPath)
	mcpTTL, err := getDuration(get, "KOTOJI_MCP_TOKEN_TTL", defaultMCPTokenTTL)
	if err != nil {
		return Config{}, err
	}
	c.MCPTokenTTL = mcpTTL

	// --- github mirror ---
	c.GitHub = GitHubMirror{
		Enabled:       getBool(get, "KOTOJI_GITHUB_MIRROR_ENABLED", false),
		Token:         firstNonEmpty(getString(get, "KOTOJI_GITHUB_APP_TOKEN", ""), getString(get, "KOTOJI_GITHUB_PAT", "")),
		Org:           getString(get, "KOTOJI_GITHUB_ORG", ""),
		WebhookSecret: getString(get, "KOTOJI_GITHUB_WEBHOOK_SECRET", ""),
	}

	// --- at-rest secret key (encrypts the DB-stored GitHub PAT) ---
	// Optional. When set it MUST decode (hex or base64) to >= 32 bytes; the
	// secretbox layer validates and falls back to a derived key when unset. The
	// raw env value is carried verbatim and resolved in the composition root so
	// config stays free of the crypto dependency.
	c.SecretKey = getString(get, "KOTOJI_SECRET_KEY", "")

	// --- zip / quotas ---
	c.Zip = ZipLimits{
		MaxUploadBytes: getInt64(get, "KOTOJI_MAX_UPLOAD_BYTES", defaultMaxUploadBytes),
		MaxTotalBytes:  getInt64(get, "KOTOJI_ZIP_MAX_TOTAL_BYTES", defaultZipMaxTotalBytes),
		MaxFiles:       getInt(get, "KOTOJI_ZIP_MAX_FILES", defaultZipMaxFiles),
		MaxRatio:       getInt(get, "KOTOJI_ZIP_MAX_RATIO", defaultZipMaxRatio),
		// Per-entry uncompressed cap, now operator-configurable (M7). Default keeps the
		// previous silent site-layer value (50MiB).
		MaxEntryBytes: getInt64(get, "KOTOJI_ZIP_MAX_ENTRY_BYTES", defaultZipMaxEntryBytes),
		AllowedExt:    splitCSV(getString(get, "KOTOJI_ZIP_ALLOWED_EXT", defaultZipAllowedExt)),
	}
	c.SiteQuotaBytes = getInt64(get, "KOTOJI_SITE_QUOTA_BYTES", defaultSiteQuotaBytes)
	c.UserSiteQuota = getInt(get, "KOTOJI_USER_SITE_QUOTA", defaultUserSiteQuota)

	// --- rate limits ---
	c.RateLimitAPIRPS = getInt(get, "KOTOJI_RATE_LIMIT_API_RPS", defaultRateLimitAPIRPS)
	c.RateLimitServeRPS = getInt(get, "KOTOJI_RATE_LIMIT_SERVE_RPS", defaultRateLimitServeR)

	// --- operability / background jobs ---
	grace, err := getDuration(get, "KOTOJI_SOFT_DELETE_GRACE", defaultSoftDeleteGrace)
	if err != nil {
		return Config{}, err
	}
	c.SoftDeleteGrace = grace
	// Backup dir defaults to DATA_DIR/backups (CANONICAL §4.2 / architecture.md §5).
	c.BackupDir = getString(get, "KOTOJI_BACKUP_DIR", filepath.Join(c.DataDir, "backups"))
	opsInterval, err := getDuration(get, "KOTOJI_OPS_INTERVAL", defaultOpsInterval)
	if err != nil {
		return Config{}, err
	}
	c.OpsInterval = opsInterval

	// --- CORS ---
	origins := getCSV(get, "KOTOJI_CORS_ALLOWED_ORIGINS")
	// Record whether an explicit allowlist was configured. When it was NOT (and the
	// control base URL env is also unset), the control plane resolves the default
	// CORS origin dynamically from the EFFECTIVE control base URL (env > DB >
	// derived) so a runtime-configured instance accepts its own origin.
	c.CORSOriginsEnvSet = len(origins) > 0
	if len(origins) == 0 {
		origins = []string{c.ControlBaseURL}
	}
	c.CORSAllowedOrigins = origins

	// --- logging ---
	c.LogLevel = strings.ToLower(getString(get, "KOTOJI_LOG_LEVEL", defaultLogLevel))
	c.LogFormat = strings.ToLower(getString(get, "KOTOJI_LOG_FORMAT", defaultLogFormat))

	// --- handle bounds ---
	c.HandleMinLen = getInt(get, "KOTOJI_HANDLE_MIN_LEN", defaultHandleMinLen)
	c.HandleMaxLen = getInt(get, "KOTOJI_HANDLE_MAX_LEN", defaultHandleMaxLen)

	// --- behavior toggles ---
	c.PublishedPublic = getBool(get, "KOTOJI_PUBLISHED_PUBLIC", true)
	c.TrustProxyHeaders = getBool(get, "KOTOJI_TRUST_PROXY_HEADERS", true)

	if err := c.validate(prod); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validate enforces cross-field and production-required invariants.
func (c Config) validate(prod bool) error {
	var errs []error

	// Log format / level sanity (cheap, always-on).
	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("KOTOJI_LOG_FORMAT: invalid value %q (want json|text)", c.LogFormat))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("KOTOJI_LOG_LEVEL: invalid value %q (want debug|info|warn|error)", c.LogLevel))
	}
	if c.HandleMinLen < 1 || c.HandleMaxLen < c.HandleMinLen {
		errs = append(errs, fmt.Errorf("KOTOJI_HANDLE_MIN_LEN/MAX_LEN: invalid bounds (%d..%d)", c.HandleMinLen, c.HandleMaxLen))
	}

	// kotoji-native TLS (auto) requires the all-in-one run mode: the single :443
	// listener fronts BOTH planes via Host routing, which only exists when both the
	// control + data planes run in this process. Refuse the nonsensical combination
	// rather than silently boot a listener that can only ever serve one plane.
	if c.TLSMode == TLSModeAuto && c.Mode != RunModeAll {
		errs = append(errs, fmt.Errorf("KOTOJI_TLS_MODE=auto requires KOTOJI_RUN_MODE=all (got %q): the :443 listener fronts both planes via Host routing", c.Mode))
	}

	// The control plane needs a database for sessions/metadata. The data-plane-only
	// mode also reads metadata, so DB is required whenever DB-backed planes run.
	dbRequired := c.ServesControl() || c.ServesData()

	if prod {
		// Production hard-requires the persistence + identity essentials.
		if dbRequired && c.DatabaseURL == "" {
			errs = append(errs, errors.New("KOTOJI_DATABASE_URL is required in production"))
		}
		if c.BaseDomain == "" || c.BaseDomain == defaultBaseDomain {
			errs = append(errs, errors.New("KOTOJI_BASE_DOMAIN must be set to a real domain in production"))
		}
		if c.ControlBaseURL == "" || c.ControlBaseURL == defaultControlBaseURL {
			errs = append(errs, errors.New("KOTOJI_CONTROL_BASE_URL must be set to a real URL in production"))
		}
		// Disallow the dev-only no-auth mode on a production control plane. It is
		// exclusive so this also rejects any "none,oidc" style combination.
		if c.ServesControl() && c.NoAuthEnabled() {
			errs = append(errs, errors.New("KOTOJI_AUTH_MODE=none is not allowed in production"))
		}
		// FAIL-CLOSED: the __Host- cookies' Secure attribute is the SOLE cross-subdomain
		// isolation control (§8.1). If an operator sets KOTOJI_COOKIE_SECURE=false in
		// production, those cookies are silently downgraded and can leak across origins,
		// so we refuse to boot rather than serve session/CSRF cookies without Secure.
		if !c.CookieSecure {
			errs = append(errs, errors.New("KOTOJI_COOKIE_SECURE=false is not allowed in production"))
		}
		// H2 FAIL-CLOSED: in production REQUIRE an explicit, high-entropy
		// KOTOJI_SECRET_KEY whenever at-rest secrets can be stored. Without it the
		// at-rest AES-256-GCM key is DERIVED (sha256) from low-entropy, partly-public
		// config inputs (admin-pw-hash | oidc-secret | control-base-url | base-domain),
		// so an attacker who learns those inputs can reproduce the key and decrypt the
		// stored GitHub PAT / OIDC client secret. At-rest secret storage is reachable on
		// every control-plane instance (the admin GitHub mirror + OIDC config write
		// through secretbox.Seal), so we require the key whenever the control plane runs.
		// The derived fallback remains valid in DEVELOPMENT only.
		if c.ServesControl() && !explicitSecretKey(c.SecretKey) {
			errs = append(errs, errors.New("KOTOJI_SECRET_KEY is required in production: it is the at-rest encryption key for stored secrets (GitHub PAT, OIDC client secret). Generate one with: openssl rand -hex 32"))
		}
	}

	// Auth-provider-specific requirements (enforced whenever the control plane
	// runs, in any environment — these are correctness requirements, not
	// hardening). Each ENABLED provider is validated independently so oidc +
	// password can coexist (break-glass).
	if c.ServesControl() {
		if c.OIDCEnabled() {
			if c.OIDC.Issuer == "" {
				errs = append(errs, errors.New("KOTOJI_AUTH_OIDC_ISSUER is required when oidc is enabled"))
			}
			if c.OIDC.ClientID == "" {
				errs = append(errs, errors.New("KOTOJI_AUTH_OIDC_CLIENT_ID is required when oidc is enabled"))
			}
			if c.OIDC.ClientSecret == "" {
				errs = append(errs, errors.New("KOTOJI_AUTH_OIDC_CLIENT_SECRET is required when oidc is enabled"))
			}
			// FAIL-CLOSED: with neither an email allowlist nor a domain allowlist,
			// ANY Google account could sign in. We require at least one gate.
			if len(c.OIDC.AllowedDomains) == 0 && len(c.OIDC.AllowedEmails) == 0 {
				errs = append(errs, errors.New("KOTOJI_OIDC_ALLOWED_EMAILS or KOTOJI_OIDC_ALLOWED_DOMAINS must be set when oidc is enabled (fail-closed: empty allowlists deny all sign-ins)"))
			}
		}
		if c.PasswordEnabled() {
			// The env password is OPTIONAL: when empty, the instance enters the
			// first-run "setup required" state and the admin sets the password via
			// the GUI (stored hashed in the DB). When provided, it must meet the
			// same minimum length the setup flow enforces.
			if c.AdminPassword != "" && len(c.AdminPassword) < AdminPasswordMinLen {
				errs = append(errs, fmt.Errorf("KOTOJI_AUTH_ADMIN_PASSWORD must be at least %d characters", AdminPasswordMinLen))
			}
		}
		// AuthModeNone needs no provider config (guarded for production above).
	}

	// GitHub mirror, when enabled, needs credentials and a webhook secret.
	if c.GitHub.Enabled {
		if c.GitHub.Token == "" {
			errs = append(errs, errors.New("KOTOJI_GITHUB_APP_TOKEN or KOTOJI_GITHUB_PAT is required when mirror is enabled"))
		}
		if c.GitHub.WebhookSecret == "" {
			errs = append(errs, errors.New("KOTOJI_GITHUB_WEBHOOK_SECRET is required when mirror is enabled"))
		}
	}

	return errors.Join(errs...)
}

// deriveHost extracts the hostname (no port) from a base URL.
func deriveHost(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host in %q", base)
	}
	return u.Hostname(), nil
}

// defaultRedirect builds the OIDC redirect URL from the control base URL.
func defaultRedirect(base string) string {
	return strings.TrimRight(base, "/") + "/auth/callback"
}

// --- typed env getters (small, no external deps) ---

func getString(get getenv, key, def string) string {
	if v, ok := get(key); ok && v != "" {
		return v
	}
	return def
}

// envSet reports whether key is present in the environment with a NON-EMPTY value.
// Used to gate the env-over-DB precedence for the runtime-configurable settings.
func envSet(get getenv, key string) bool {
	v, ok := get(key)
	return ok && strings.TrimSpace(v) != ""
}

func getInt(get getenv, key string, def int) int {
	if v, ok := get(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getInt64(get getenv, key string, def int64) int64 {
	if v, ok := get(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getBool(get getenv, key string, def bool) bool {
	if v, ok := get(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getDuration(get getenv, key string, def time.Duration) (time.Duration, error) {
	v, ok := get(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, v, err)
	}
	return d, nil
}

// getCSV reads a comma-separated env var into a trimmed, empty-filtered slice.
func getCSV(get getenv, key string) []string {
	v, ok := get(key)
	if !ok || v == "" {
		return nil
	}
	return splitCSV(v)
}

// parseAuthModes parses KOTOJI_AUTH_MODE as a comma-set of enabled providers. It
// accepts a single legacy value ("oidc"/"password"/"none") unchanged or a comma
// list ("oidc,password"). Rules: every entry must be one of {oidc,password,none};
// duplicates collapse; "none" is EXCLUSIVE (it cannot be combined with any other
// provider). The returned set is normalized to a stable order (oidc, password,
// none) and the second return value is the LEGACY representative (oidc wins, then
// password, then none) carried on Config.AuthMode for back-compat.
func parseAuthModes(raw string) ([]AuthMode, AuthMode, error) {
	parts := splitCSV(strings.ToLower(raw))
	if len(parts) == 0 {
		return nil, "", fmt.Errorf("KOTOJI_AUTH_MODE: empty (want oidc|password|none or a comma list)")
	}
	seen := map[AuthMode]bool{}
	for _, p := range parts {
		m := AuthMode(p)
		switch m {
		case AuthModeOIDC, AuthModePassword, AuthModeNone:
			seen[m] = true
		default:
			return nil, "", fmt.Errorf("KOTOJI_AUTH_MODE: invalid value %q (want oidc|password|none)", p)
		}
	}
	// "none" is exclusive: disabling auth alongside a real provider is a
	// configuration mistake we refuse rather than silently resolve.
	if seen[AuthModeNone] && len(seen) > 1 {
		return nil, "", fmt.Errorf("KOTOJI_AUTH_MODE: %q is exclusive and cannot be combined with other providers", AuthModeNone)
	}

	// Normalize to a stable order so the public config / set comparisons are
	// deterministic regardless of how the operator ordered the env value.
	var modes []AuthMode
	for _, m := range []AuthMode{AuthModeOIDC, AuthModePassword, AuthModeNone} {
		if seen[m] {
			modes = append(modes, m)
		}
	}
	return modes, modes[0], nil // modes[0] is the highest-priority representative
}

// lowerCSV lowercases + trims every entry, dropping empties and duplicates while
// preserving first-seen order. Used to normalize the OIDC email/domain lists.
func lowerCSV(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstNonEmptyCSV returns the first non-empty CSV slice (preferred name first,
// legacy fallback second) so a renamed env var keeps its old name working.
func firstNonEmptyCSV(preferred, fallback []string) []string {
	if len(preferred) > 0 {
		return preferred
	}
	return fallback
}

// splitCSV trims and drops empty fields from a comma-separated string.
func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// firstNonEmpty returns the first non-empty string of its arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// secretKeyMinBytes is the minimum decoded length an explicit KOTOJI_SECRET_KEY
// must yield to count as a real (non-derived) key — AES-256 needs 32 bytes. It
// MUST match secretbox.KeySize; secretbox.decodeExplicitKey applies the same gate.
// Centralized here so the H2 production check and the crypto layer agree on what
// counts as "an explicit key was provided".
const secretKeyMinBytes = 32

// explicitSecretKey reports whether KOTOJI_SECRET_KEY decodes (hex OR base64) to
// at least 32 bytes — i.e. secretbox.ResolveKey would use the operator-supplied
// key rather than the DERIVED fallback. It mirrors secretbox.decodeExplicitKey
// exactly, but is re-implemented here (not imported) so the config package stays
// free of the crypto dependency (it only needs hex/base64 decoding). The H2
// production validation gates on this: a blank/short/undecodable value is treated
// as "no explicit key" and rejected in production.
func explicitSecretKey(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	// Hex first: a 64-char hex string is the canonical 32-byte key form.
	if b, err := hex.DecodeString(v); err == nil && len(b) >= secretKeyMinBytes {
		return true
	}
	// base64 (std then raw/url) fallback — same set secretbox accepts.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil && len(b) >= secretKeyMinBytes {
			return true
		}
	}
	return false
}
