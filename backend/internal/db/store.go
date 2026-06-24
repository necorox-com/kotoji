// Package db is the metadata data-access layer. It wraps the sqlc-generated
// queries (internal/db/gen) behind a pgxpool-backed Store that the control plane
// (Phase 2/4) depends on. git remains the source of truth for content; this layer
// only ever touches the Postgres metadata store (data-model.md §0).
//
// DI / testability: callers depend on the Querier interface (re-exported from the
// generated package), not the concrete *Store, so a generated mock or a fake can
// be injected in tests. The Store adds the connection lifecycle (pool + ping),
// a transaction helper (tx.go), and a handful of domain-typed convenience methods
// that bundle the common multi-query flows Phase 2/4 need (e.g. atomic site create).
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/secretbox"
)

// Querier is the full generated query surface. Re-exported here so consumers can
// depend on internal/db (not internal/db/gen) and mock a single interface. The
// generated *gen.Queries and *Store both satisfy it.
type Querier = gen.Querier

// pingTimeout bounds the initial connectivity check and the readiness probe so a
// dead/slow database fails fast instead of hanging the caller.
const pingTimeout = 5 * time.Second

// SettingAdminPasswordHash is the instance_settings key under which the first-run
// admin-password bcrypt hash is stored (AUTH_MODE=password, no env password). It
// is exported so the auth layer can assert its own copy of the key stays in sync.
const SettingAdminPasswordHash = "admin_password_hash"

// GitHub mirror config keys in instance_settings. These let the admin configure
// the GitHub mirror at RUNTIME via the GUI (overriding the KOTOJI_GITHUB_* env at
// the effective-config layer). The token is stored ENCRYPTED (AES-256-GCM via
// secretbox); the rest are plain (a feature flag, an org name, and the webhook
// HMAC secret which never leaves the server).
const (
	// SettingGitHubMirrorEnabled is "true"/"false"; absent => fall back to env.
	SettingGitHubMirrorEnabled = "github_mirror_enabled"
	// SettingGitHubOrg is the org/owner for created repos.
	SettingGitHubOrg = "github_org"
	// SettingGitHubWebhookSecret is the HMAC secret for /api/webhooks/github.
	SettingGitHubWebhookSecret = "github_webhook_secret"
	// SettingGitHubToken holds the AES-256-GCM CIPHERTEXT of the push PAT/app token.
	SettingGitHubToken = "github_token"
)

// Domain/URL config keys in instance_settings. WordPress-style runtime config of
// the two routing settings the admin may change in the GUI: the base domain used
// to parse {handle}[--{branch}].<base>, and the external control base URL that
// drives OIDC redirect / cookie host / absolute links / default CORS origin.
// Both are PLAIN strings (NOT secret) — no secretbox is needed. Precedence is
// resolved by the effective-config layer (env OVERRIDES DB); absent => fall back
// to env, then to a per-request derived default.
const (
	// SettingBaseDomain is the runtime base domain (e.g. "hosting.example.com").
	SettingBaseDomain = "base_domain"
	// SettingControlBaseURL is the runtime external control base URL (absolute http(s)).
	SettingControlBaseURL = "control_base_url"
)

// OIDC (Google) auth config keys in instance_settings. WordPress-style runtime
// config of the OIDC provider so an admin enables Google sign-in entirely from the
// GUI (overriding the KOTOJI_OIDC_* / KOTOJI_AUTH_OIDC_* env at the effective-config
// layer, internal/oidccfg). The client SECRET is stored ENCRYPTED (AES-256-GCM via
// secretbox) and is WRITE-ONLY over the API (never returned). Every other field is
// PLAIN: a feature flag, the issuer/client-id/redirect strings, and the
// email/domain/admin CSV allowlists. Precedence is resolved by the effective-config
// layer (env OVERRIDES DB, per field); an absent key falls back to env, then a
// per-request derived default (redirect URL).
const (
	// SettingOIDCEnabled is "true"/"false"; absent => fall back to env/disabled.
	SettingOIDCEnabled = "oidc_enabled"
	// SettingOIDCIssuer is the OIDC issuer URL (e.g. https://accounts.google.com).
	SettingOIDCIssuer = "oidc_issuer"
	// SettingOIDCClientID is the OAuth2 client id.
	SettingOIDCClientID = "oidc_client_id"
	// SettingOIDCClientSecret holds the AES-256-GCM CIPHERTEXT of the OAuth2 client secret.
	SettingOIDCClientSecret = "oidc_client_secret"
	// SettingOIDCRedirectURL is the optional explicit redirect URL; empty => derived.
	SettingOIDCRedirectURL = "oidc_redirect_url"
	// SettingOIDCAllowedEmails is the CSV exact-email allowlist.
	SettingOIDCAllowedEmails = "oidc_allowed_emails"
	// SettingOIDCAllowedDomains is the CSV hosted-domain allowlist.
	SettingOIDCAllowedDomains = "oidc_allowed_domains"
	// SettingOIDCAdminEmails is the CSV admin-email allowlist (auto-promote to is_admin).
	SettingOIDCAdminEmails = "oidc_admin_emails"
)

// boolTrue / boolFalse are the canonical string encodings for boolean settings.
const (
	boolTrue  = "true"
	boolFalse = "false"
)

// Store is the application-facing handle to the metadata database. It embeds the
// generated *gen.Queries (so every named query is available directly) and owns the
// underlying pgxpool for lifecycle (Close) and transactions (WithTx).
type Store struct {
	*gen.Queries
	pool *pgxpool.Pool
	// box encrypts/decrypts secrets stored at rest in instance_settings (the
	// GitHub PAT). Optional: when nil, GetGitHubConfig reports the token as not set
	// and SetGitHubConfig refuses to persist a token (so a misconfigured instance
	// never stores a plaintext credential). The composition root wires it via
	// SetSecretBox after deriving the key.
	box *secretbox.Box
}

// SetSecretBox installs the at-rest encryption box used for secret settings (the
// GitHub token). The composition root calls it once after deriving the key. It is
// a setter (not a New parameter) so existing callers/tests keep working unchanged
// and a Store without a box simply treats secrets as unconfigured.
func (s *Store) SetSecretBox(box *secretbox.Box) { s.box = box }

// compile-time guarantee: a *Store exposes the whole generated query surface.
var _ gen.Querier = (*Store)(nil)

// New opens a pgxpool against dsn, verifies connectivity with a ping, and returns
// a ready Store. The caller owns Close. dsn is a standard pgx connection string
// or URL (e.g. "postgres://user:pass@host:5432/db?sslmode=disable").
func New(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("db: empty DSN")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse DSN: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}

	// Fail fast on a dead database so boot does not silently proceed without
	// persistence. A bounded context guards against an unreachable host hanging.
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return &Store{
		Queries: gen.New(pool),
		pool:    pool,
	}, nil
}

// NewWithPool builds a Store over an already-constructed pool. Useful for tests
// and for callers that manage the pool's lifecycle themselves (it does NOT ping).
func NewWithPool(pool *pgxpool.Pool) *Store {
	return &Store{Queries: gen.New(pool), pool: pool}
}

// Pool exposes the underlying pool for advanced callers (e.g. goose advisory-lock
// migration on boot). Most code should use the query methods or WithTx instead.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases all pooled connections. Safe to call once during shutdown.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ready implements observability.ReadinessChecker: /readyz returns 503 while the
// database is unreachable. The bounded ping keeps the probe responsive.
func (s *Store) Ready(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := s.pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("db: not ready: %w", err)
	}
	return nil
}

// ---- domain-typed convenience wrappers (the flows Phase 2/4 reuse) ----

// CreateSiteWithOwner inserts a site and stamps the owner's site_members row in ONE
// transaction. This is the metadata half of SiteService.CreateSite; the git-init is
// layered on top by Phase 2 (which can pass its own tx via WithTx). The handle must
// already be validated + collision-checked by the caller. Returns the created site.
func (s *Store) CreateSiteWithOwner(ctx context.Context, arg gen.CreateSiteParams) (gen.Site, error) {
	var site gen.Site
	err := s.WithTx(ctx, func(q *gen.Queries) error {
		created, err := q.CreateSite(ctx, arg)
		if err != nil {
			return fmt.Errorf("create site: %w", err)
		}
		// Maintain an owner membership row so a single membership join answers authz
		// uniformly (data-model.md §1.5). created_by = owner = self at creation time.
		if err := q.AddOwnerMembership(ctx, gen.AddOwnerMembershipParams{
			SiteID: created.ID,
			UserID: created.OwnerID,
		}); err != nil {
			return fmt.Errorf("add owner membership: %w", err)
		}
		site = created
		return nil
	})
	if err != nil {
		return gen.Site{}, err
	}
	return site, nil
}

// RenameHandleWithRedirect performs the atomic rename: record old->site in
// handle_redirects, update the live handle, and drop any stale redirect that points
// the NEW handle back at this same site (rename-back). All in one transaction
// (CANONICAL §5.5). The caller validates newHandle and collision-checks beforehand.
func (s *Store) RenameHandleWithRedirect(ctx context.Context, id uuid.UUID, oldHandle, newHandle string) error {
	return s.WithTx(ctx, func(q *gen.Queries) error {
		// Rename-back: if newHandle is a redirect of THIS site, remove it first so the
		// live handle and the redirect set never both claim newHandle.
		if err := q.DeleteRedirect(ctx, gen.DeleteRedirectParams{
			OldHandle: newHandle,
			SiteID:    id,
		}); err != nil {
			return fmt.Errorf("clear rename-back redirect: %w", err)
		}
		if err := q.InsertRedirect(ctx, gen.InsertRedirectParams{
			OldHandle: oldHandle,
			SiteID:    id,
		}); err != nil {
			return fmt.Errorf("insert redirect: %w", err)
		}
		if err := q.RenameHandle(ctx, gen.RenameHandleParams{
			NewHandle: newHandle,
			ID:        id,
		}); err != nil {
			return fmt.Errorf("rename handle: %w", err)
		}
		return nil
	})
}

// IsNotFound reports whether err is pgx's no-rows sentinel, the canonical "row does
// not exist" signal from the :one queries. Callers map this to site.ErrNotFound.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// ---- instance settings (first-run admin password) ----

// GetAdminPasswordHash returns the DB-stored bcrypt hash of the admin password
// and whether it is set. A missing key yields ("", false, nil) — NOT an error —
// so callers can branch cleanly into the "no DB credential" first-run path. Any
// other store error is surfaced (found is false in that case).
func (s *Store) GetAdminPasswordHash(ctx context.Context) (hash string, found bool, err error) {
	v, err := s.GetInstanceSetting(ctx, SettingAdminPasswordHash)
	if err != nil {
		if IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("db: get admin password hash: %w", err)
	}
	return v, true, nil
}

// SetAdminPasswordHash persists (or overwrites) the admin-password bcrypt hash.
// The plaintext is hashed by the caller (auth layer); this layer never sees it.
func (s *Store) SetAdminPasswordHash(ctx context.Context, hash string) error {
	if err := s.SetInstanceSetting(ctx, gen.SetInstanceSettingParams{
		Key:   SettingAdminPasswordHash,
		Value: hash,
	}); err != nil {
		return fmt.Errorf("db: set admin password hash: %w", err)
	}
	return nil
}

// ---- instance settings (GitHub mirror config) ----

// GitHubConfig is the DB-stored GitHub mirror configuration. EnabledSet reports
// whether the github_mirror_enabled key exists at all so the effective-config
// layer can distinguish "admin explicitly disabled" (Enabled=false, EnabledSet=true)
// from "never configured -> fall back to env" (EnabledSet=false). The token is
// returned DECRYPTED (or "" with TokenSet=false when absent or undecryptable);
// callers must never echo Token/WebhookSecret over the API.
type GitHubConfig struct {
	Enabled       bool
	EnabledSet    bool
	Token         string
	TokenSet      bool
	Org           string
	WebhookSecret string
}

// SetGitHubConfigInput is the write payload for SetGitHubConfig. Pointer fields
// are OPTIONAL partial updates: a nil pointer leaves that setting untouched, a
// non-nil pointer overwrites it. Token is special (see SetGitHubConfig): an empty
// *Token keeps the existing stored token; ClearToken removes it.
type SetGitHubConfigInput struct {
	Enabled       *bool
	Org           *string
	WebhookSecret *string
	Token         *string
	ClearToken    bool
}

// GetGitHubConfig reads the DB-stored GitHub mirror config. Absent keys map to
// zero values (never an error) so the caller can layer env fallbacks. The token
// is decrypted via the secretbox; a missing box or a decryption failure (rotated
// KOTOJI_SECRET_KEY) yields TokenSet=false — treated as "not configured", never a
// crash (LOCKED policy). Returns enabled/token/org/webhookSecret/tokenSet folded
// into GitHubConfig for ergonomic callers.
func (s *Store) GetGitHubConfig(ctx context.Context) (GitHubConfig, error) {
	var cfg GitHubConfig

	if v, found, err := s.getSetting(ctx, SettingGitHubMirrorEnabled); err != nil {
		return GitHubConfig{}, err
	} else if found {
		cfg.EnabledSet = true
		cfg.Enabled = v == boolTrue
	}

	if v, found, err := s.getSetting(ctx, SettingGitHubOrg); err != nil {
		return GitHubConfig{}, err
	} else if found {
		cfg.Org = v
	}

	if v, found, err := s.getSetting(ctx, SettingGitHubWebhookSecret); err != nil {
		return GitHubConfig{}, err
	} else if found {
		cfg.WebhookSecret = v
	}

	// Token: stored as ciphertext; decrypt best-effort. A nil box or a decode/auth
	// failure leaves TokenSet=false so a rotated key degrades to "re-enter the PAT".
	if v, found, err := s.getSetting(ctx, SettingGitHubToken); err != nil {
		return GitHubConfig{}, err
	} else if found && v != "" && s.box != nil {
		if plain, ok := s.box.Open(v); ok {
			cfg.Token = plain
			cfg.TokenSet = true
		}
	}

	return cfg, nil
}

// SetGitHubConfig persists a partial GitHub mirror config update. Only the
// non-nil fields are written, so the admin GUI can PATCH a subset. The token is
// WRITE-ONLY semantics: a non-nil, non-empty Token is encrypted and stored; an
// empty Token (or a nil Token) LEAVES the existing stored token in place (so a
// "save" that does not re-type the PAT keeps it); ClearToken=true removes the
// stored token entirely. Encrypting a token without a configured secretbox is an
// error (refuse to store a plaintext credential).
func (s *Store) SetGitHubConfig(ctx context.Context, in SetGitHubConfigInput) error {
	return s.WithTx(ctx, func(q *gen.Queries) error {
		if in.Enabled != nil {
			val := boolFalse
			if *in.Enabled {
				val = boolTrue
			}
			if err := q.SetInstanceSetting(ctx, gen.SetInstanceSettingParams{Key: SettingGitHubMirrorEnabled, Value: val}); err != nil {
				return fmt.Errorf("db: set github_mirror_enabled: %w", err)
			}
		}
		if in.Org != nil {
			if err := q.SetInstanceSetting(ctx, gen.SetInstanceSettingParams{Key: SettingGitHubOrg, Value: *in.Org}); err != nil {
				return fmt.Errorf("db: set github_org: %w", err)
			}
		}
		if in.WebhookSecret != nil {
			if err := q.SetInstanceSetting(ctx, gen.SetInstanceSettingParams{Key: SettingGitHubWebhookSecret, Value: *in.WebhookSecret}); err != nil {
				return fmt.Errorf("db: set github_webhook_secret: %w", err)
			}
		}
		// Token handling (order: clear wins over set; empty set is a no-op).
		switch {
		case in.ClearToken:
			if err := q.DeleteInstanceSetting(ctx, SettingGitHubToken); err != nil {
				return fmt.Errorf("db: clear github_token: %w", err)
			}
		case in.Token != nil && *in.Token != "":
			if s.box == nil {
				// Refuse to persist a credential we cannot encrypt (no plaintext at rest).
				return errors.New("db: cannot store github token without a secret key configured")
			}
			ct, err := s.box.Seal(*in.Token)
			if err != nil {
				return fmt.Errorf("db: encrypt github_token: %w", err)
			}
			if err := q.SetInstanceSetting(ctx, gen.SetInstanceSettingParams{Key: SettingGitHubToken, Value: ct}); err != nil {
				return fmt.Errorf("db: set github_token: %w", err)
			}
		}
		// Token == nil OR empty (and not clearing): leave the stored token untouched.
		return nil
	})
}

// ---- instance settings (domain / URL config) ----

// DomainConfig is the DB-stored WordPress-style domain/URL config. Each field is
// reported alongside a *Set boolean so the effective-config layer can tell "admin
// explicitly set it" from "never configured -> fall back to env / derive". Both
// values are PLAIN strings (not secret); callers MAY echo them over the API.
type DomainConfig struct {
	BaseDomain        string
	BaseDomainSet     bool
	ControlBaseURL    string
	ControlBaseURLSet bool
}

// SetDomainConfigInput is the write payload for SetDomainConfig. Pointer fields
// are OPTIONAL partial updates: a nil pointer leaves that setting untouched, a
// non-nil pointer overwrites it (an empty string DELETES the key, reverting to
// the env/derived fallback). The caller validates the values before persisting.
type SetDomainConfigInput struct {
	BaseDomain     *string
	ControlBaseURL *string
}

// GetDomainConfig reads the DB-stored domain/URL config. Absent keys map to zero
// values with *Set=false (never an error) so the caller can layer env/derived
// fallbacks. A store read error other than no-rows is surfaced.
func (s *Store) GetDomainConfig(ctx context.Context) (DomainConfig, error) {
	var cfg DomainConfig

	if v, found, err := s.getSetting(ctx, SettingBaseDomain); err != nil {
		return DomainConfig{}, err
	} else if found {
		cfg.BaseDomain = v
		cfg.BaseDomainSet = true
	}

	if v, found, err := s.getSetting(ctx, SettingControlBaseURL); err != nil {
		return DomainConfig{}, err
	} else if found {
		cfg.ControlBaseURL = v
		cfg.ControlBaseURLSet = true
	}

	return cfg, nil
}

// SetDomainConfig persists a partial domain/URL config update in ONE transaction.
// Only the non-nil fields are written so the admin GUI can PATCH a subset. An
// empty-string pointer DELETES the key (reverts to the env/derived fallback); a
// non-empty pointer overwrites it. The caller validates the values first (this
// layer only persists). Values are plain (not encrypted).
func (s *Store) SetDomainConfig(ctx context.Context, in SetDomainConfigInput) error {
	return s.WithTx(ctx, func(q *gen.Queries) error {
		if in.BaseDomain != nil {
			if err := setOrDeleteSetting(ctx, q, SettingBaseDomain, *in.BaseDomain); err != nil {
				return fmt.Errorf("db: set base_domain: %w", err)
			}
		}
		if in.ControlBaseURL != nil {
			if err := setOrDeleteSetting(ctx, q, SettingControlBaseURL, *in.ControlBaseURL); err != nil {
				return fmt.Errorf("db: set control_base_url: %w", err)
			}
		}
		return nil
	})
}

// ---- instance settings (OIDC / Google auth config) ----

// OIDCConfig is the DB-stored OIDC provider configuration. Each field carries a
// *Set boolean so the effective-config layer (internal/oidccfg) can distinguish
// "admin explicitly set it" (so it overrides env when env is unset) from "never
// configured -> fall back to env". The client secret is returned DECRYPTED (or ""
// with ClientSecretSet=false when absent or undecryptable, e.g. a rotated
// KOTOJI_SECRET_KEY); the admin API NEVER echoes Secret over the wire — only a
// ClientSecretSet boolean. CSV fields are returned as the raw stored string (the
// effective layer splits + normalizes them) so this layer stays parse-free.
type OIDCConfig struct {
	Enabled           bool
	EnabledSet        bool
	Issuer            string
	IssuerSet         bool
	ClientID          string
	ClientIDSet       bool
	ClientSecret      string
	ClientSecretSet   bool
	RedirectURL       string
	RedirectURLSet    bool
	AllowedEmails     string // raw CSV
	AllowedEmailsSet  bool
	AllowedDomains    string // raw CSV
	AllowedDomainsSet bool
	AdminEmails       string // raw CSV
	AdminEmailsSet    bool
}

// SetOIDCConfigInput is the write payload for SetOIDCConfig. Pointer fields are
// OPTIONAL partial updates: a nil pointer leaves that setting untouched, a non-nil
// pointer overwrites it (an empty string DELETES the plain keys, reverting to the
// env/derived fallback). ClientSecret is SPECIAL (write-only, see SetOIDCConfig):
// a non-nil non-empty value is encrypted + stored; a nil/empty value KEEPS the
// existing stored secret; ClearClientSecret=true removes it. The caller validates
// the values before persisting.
type SetOIDCConfigInput struct {
	Enabled           *bool
	Issuer            *string
	ClientID          *string
	ClientSecret      *string
	ClearClientSecret bool
	RedirectURL       *string
	AllowedEmails     *string // raw CSV
	AllowedDomains    *string // raw CSV
	AdminEmails       *string // raw CSV
}

// GetOIDCConfig reads the DB-stored OIDC config. Absent keys map to zero values
// with *Set=false (never an error) so the caller can layer env/derived fallbacks.
// The client secret is decrypted best-effort via the secretbox; a nil box or a
// decode/auth failure (rotated KOTOJI_SECRET_KEY) leaves ClientSecretSet=false —
// treated as "not configured", never a crash (LOCKED policy, mirrors GetGitHubConfig).
func (s *Store) GetOIDCConfig(ctx context.Context) (OIDCConfig, error) {
	var cfg OIDCConfig

	if v, found, err := s.getSetting(ctx, SettingOIDCEnabled); err != nil {
		return OIDCConfig{}, err
	} else if found {
		cfg.EnabledSet = true
		cfg.Enabled = v == boolTrue
	}

	// Plain string fields: present => *Set true (the empty string is never stored
	// for these — SetOIDCConfig deletes the key on an empty pointer).
	for _, f := range []struct {
		key string
		dst *string
		set *bool
	}{
		{SettingOIDCIssuer, &cfg.Issuer, &cfg.IssuerSet},
		{SettingOIDCClientID, &cfg.ClientID, &cfg.ClientIDSet},
		{SettingOIDCRedirectURL, &cfg.RedirectURL, &cfg.RedirectURLSet},
		{SettingOIDCAllowedEmails, &cfg.AllowedEmails, &cfg.AllowedEmailsSet},
		{SettingOIDCAllowedDomains, &cfg.AllowedDomains, &cfg.AllowedDomainsSet},
		{SettingOIDCAdminEmails, &cfg.AdminEmails, &cfg.AdminEmailsSet},
	} {
		if v, found, err := s.getSetting(ctx, f.key); err != nil {
			return OIDCConfig{}, err
		} else if found {
			*f.dst = v
			*f.set = true
		}
	}

	// Client secret: stored as ciphertext; decrypt best-effort. A nil box or a
	// decode/auth failure leaves ClientSecretSet=false so a rotated key degrades to
	// "re-enter the secret" rather than crashing.
	if v, found, err := s.getSetting(ctx, SettingOIDCClientSecret); err != nil {
		return OIDCConfig{}, err
	} else if found && v != "" && s.box != nil {
		if plain, ok := s.box.Open(v); ok {
			cfg.ClientSecret = plain
			cfg.ClientSecretSet = true
		}
	}

	return cfg, nil
}

// SetOIDCConfig persists a partial OIDC config update in ONE transaction. Only the
// non-nil fields are written so the admin GUI can PATCH a subset. The plain string
// fields follow the "empty pointer reverts to fallback" semantics (an empty value
// DELETES the key); the client secret is WRITE-ONLY: a non-nil non-empty secret is
// encrypted + stored, a nil/empty secret LEAVES the stored one in place (so a save
// that does not re-type the secret keeps it), ClearClientSecret=true removes it.
// Encrypting a secret without a configured secretbox is an error (refuse to store a
// plaintext credential — mirrors SetGitHubConfig).
func (s *Store) SetOIDCConfig(ctx context.Context, in SetOIDCConfigInput) error {
	return s.WithTx(ctx, func(q *gen.Queries) error {
		if in.Enabled != nil {
			val := boolFalse
			if *in.Enabled {
				val = boolTrue
			}
			if err := q.SetInstanceSetting(ctx, gen.SetInstanceSettingParams{Key: SettingOIDCEnabled, Value: val}); err != nil {
				return fmt.Errorf("db: set oidc_enabled: %w", err)
			}
		}
		// Plain fields: empty value deletes the key (revert to env/derived).
		for _, f := range []struct {
			key string
			val *string
		}{
			{SettingOIDCIssuer, in.Issuer},
			{SettingOIDCClientID, in.ClientID},
			{SettingOIDCRedirectURL, in.RedirectURL},
			{SettingOIDCAllowedEmails, in.AllowedEmails},
			{SettingOIDCAllowedDomains, in.AllowedDomains},
			{SettingOIDCAdminEmails, in.AdminEmails},
		} {
			if f.val != nil {
				if err := setOrDeleteSetting(ctx, q, f.key, *f.val); err != nil {
					return fmt.Errorf("db: set %s: %w", f.key, err)
				}
			}
		}
		// Client secret handling (order: clear wins over set; empty set is a no-op).
		switch {
		case in.ClearClientSecret:
			if err := q.DeleteInstanceSetting(ctx, SettingOIDCClientSecret); err != nil {
				return fmt.Errorf("db: clear oidc_client_secret: %w", err)
			}
		case in.ClientSecret != nil && *in.ClientSecret != "":
			if s.box == nil {
				// Refuse to persist a credential we cannot encrypt (no plaintext at rest).
				return errors.New("db: cannot store oidc client secret without a secret key configured")
			}
			ct, err := s.box.Seal(*in.ClientSecret)
			if err != nil {
				return fmt.Errorf("db: encrypt oidc_client_secret: %w", err)
			}
			if err := q.SetInstanceSetting(ctx, gen.SetInstanceSettingParams{Key: SettingOIDCClientSecret, Value: ct}); err != nil {
				return fmt.Errorf("db: set oidc_client_secret: %w", err)
			}
		}
		// ClientSecret == nil OR empty (and not clearing): leave the stored secret.
		return nil
	})
}

// setOrDeleteSetting writes a non-empty value or DELETES the key when value is
// empty. Centralizes the "empty pointer reverts to fallback" semantics shared by
// the plain (non-secret) settings.
func setOrDeleteSetting(ctx context.Context, q *gen.Queries, key, value string) error {
	if value == "" {
		return q.DeleteInstanceSetting(ctx, key)
	}
	return q.SetInstanceSetting(ctx, gen.SetInstanceSettingParams{Key: key, Value: value})
}

// getSetting reads one instance_settings value, mapping the no-rows sentinel to
// (.., false, nil) so the GitHub-config readers treat an absent key as "unset"
// rather than an error.
func (s *Store) getSetting(ctx context.Context, key string) (string, bool, error) {
	v, err := s.GetInstanceSetting(ctx, key)
	if err != nil {
		if IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("db: get setting %q: %w", key, err)
	}
	return v, true, nil
}
