// Package config loads and validates the backend's runtime configuration from
// the environment. All variables use the KOTOJI_ prefix and map 1:1 to the env
// matrix in docs/architecture.md §6. Load fails fast: in production it refuses
// to boot with missing/invalid required values; in development it fills sane
// defaults so the stack runs with zero external services.
package config

import (
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

// OIDCConfig holds OIDC provider settings (used when AuthMode == oidc).
type OIDCConfig struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	AllowedDomain string   // restrict to a Google Workspace domain (hd hint)
	AllowedEmails []string // explicit email allowlist (alt/extra to AllowedDomain)
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
	AllowedExt     []string
}

// Config is the fully-parsed, validated backend configuration.
type Config struct {
	Env  Environment
	Mode RunMode

	ControlAddr string
	ServeAddr   string

	BaseDomain     string // base for {handle}[--{branch}].<base> parsing
	ControlBaseURL string // external URL of the control host
	// ControlHost is derived from ControlBaseURL's host (no port) and is the
	// host-only Domain for the session cookie. It must never be the wildcard
	// parent (see §8.1 isolation).
	ControlHost string

	DatabaseURL string
	DBMaxConns  int
	AutoMigrate bool // run embedded goose migrations on boot (default true)

	DataDir string
	GitBin  string

	AuthMode      AuthMode
	OIDC          OIDCConfig
	AdminPassword string // bcrypt-checked single-admin password (mode=password)
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

// IsProduction reports whether the environment is production.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

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

	// --- domains / URLs ---
	c.BaseDomain = getString(get, "KOTOJI_BASE_DOMAIN", defaultBaseDomain)
	c.ControlBaseURL = getString(get, "KOTOJI_CONTROL_BASE_URL", defaultControlBaseURL)

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

	// --- auth ---
	authMode := strings.ToLower(getString(get, "KOTOJI_AUTH_MODE", string(AuthModeOIDC)))
	switch AuthMode(authMode) {
	case AuthModeOIDC, AuthModePassword, AuthModeNone:
		c.AuthMode = AuthMode(authMode)
	default:
		return Config{}, fmt.Errorf("KOTOJI_AUTH_MODE: invalid value %q (want oidc|password|none)", authMode)
	}

	c.OIDC = OIDCConfig{
		Issuer:        getString(get, "KOTOJI_AUTH_OIDC_ISSUER", defaultOIDCIssuer),
		ClientID:      getString(get, "KOTOJI_AUTH_OIDC_CLIENT_ID", ""),
		ClientSecret:  getString(get, "KOTOJI_AUTH_OIDC_CLIENT_SECRET", ""),
		RedirectURL:   getString(get, "KOTOJI_AUTH_OIDC_REDIRECT_URL", defaultRedirect(c.ControlBaseURL)),
		AllowedDomain: getString(get, "KOTOJI_AUTH_GOOGLE_HD", ""),
		AllowedEmails: getCSV(get, "KOTOJI_AUTH_ALLOWED_EMAILS"),
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
		AllowedExt:     splitCSV(getString(get, "KOTOJI_ZIP_ALLOWED_EXT", defaultZipAllowedExt)),
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
		// Disallow the dev-only no-auth mode on a production control plane.
		if c.ServesControl() && c.AuthMode == AuthModeNone {
			errs = append(errs, errors.New("KOTOJI_AUTH_MODE=none is not allowed in production"))
		}
	}

	// Auth-mode-specific requirements (enforced whenever the control plane runs,
	// in any environment — these are correctness requirements, not hardening).
	if c.ServesControl() {
		switch c.AuthMode {
		case AuthModeOIDC:
			if c.OIDC.Issuer == "" {
				errs = append(errs, errors.New("KOTOJI_AUTH_OIDC_ISSUER is required when AUTH_MODE=oidc"))
			}
			if c.OIDC.ClientID == "" {
				errs = append(errs, errors.New("KOTOJI_AUTH_OIDC_CLIENT_ID is required when AUTH_MODE=oidc"))
			}
			if c.OIDC.ClientSecret == "" {
				errs = append(errs, errors.New("KOTOJI_AUTH_OIDC_CLIENT_SECRET is required when AUTH_MODE=oidc"))
			}
			// At least one allowlist gate must exist or any Google account could log in.
			if c.OIDC.AllowedDomain == "" && len(c.OIDC.AllowedEmails) == 0 {
				errs = append(errs, errors.New("KOTOJI_AUTH_GOOGLE_HD or KOTOJI_AUTH_ALLOWED_EMAILS must be set when AUTH_MODE=oidc"))
			}
		case AuthModePassword:
			// The env password is OPTIONAL: when empty, the instance enters the
			// first-run "setup required" state and the admin sets the password via
			// the GUI (stored hashed in the DB). When provided, it must meet the
			// same minimum length the setup flow enforces.
			if c.AdminPassword != "" && len(c.AdminPassword) < AdminPasswordMinLen {
				errs = append(errs, fmt.Errorf("KOTOJI_AUTH_ADMIN_PASSWORD must be at least %d characters", AdminPasswordMinLen))
			}
		case AuthModeNone:
			// Allowed in development only (guarded above for production).
		}
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
