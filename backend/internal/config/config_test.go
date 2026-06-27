package config

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// devBase is a minimal env that should always load cleanly in development.
func devBase() map[string]string {
	return map[string]string{
		"KOTOJI_ENV":       "development",
		"KOTOJI_AUTH_MODE": "none",
	}
}

func TestLoad_DevelopmentDefaults(t *testing.T) {
	cfg, err := LoadFromMap(devBase())
	require.NoError(t, err)

	assert.Equal(t, EnvDevelopment, cfg.Env)
	assert.Equal(t, RunModeAll, cfg.Mode)
	assert.Equal(t, defaultControlAddr, cfg.ControlAddr)
	assert.Equal(t, defaultServeAddr, cfg.ServeAddr)
	assert.Equal(t, defaultBaseDomain, cfg.BaseDomain)
	assert.Equal(t, "hosting.localhost", cfg.ControlHost, "control host derived from base URL")
	assert.Equal(t, defaultSessionTTL, cfg.SessionTTL)
	assert.False(t, cfg.CookieSecure, "dev defaults to insecure cookies over http")
	assert.True(t, cfg.ServesControl())
	assert.True(t, cfg.ServesData())
	// Derived OIDC redirect comes from the control base URL.
	assert.Equal(t, "http://hosting.localhost:8080/auth/callback", cfg.OIDC.RedirectURL)
	// Zip allowlist parsed into a slice.
	assert.Contains(t, cfg.Zip.AllowedExt, ".html")
	assert.Contains(t, cfg.Zip.AllowedExt, ".wasm")
	// AssetMaxAge defaults to 0 => the data plane emits "no-cache" (immediate
	// re-publish/cache-purge propagation).
	assert.Equal(t, 0, cfg.AssetMaxAge)
}

func TestLoad_AssetMaxAge(t *testing.T) {
	env := devBase()
	env["KOTOJI_ASSET_MAX_AGE"] = "3600"
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Equal(t, 3600, cfg.AssetMaxAge, "positive value opts into public,max-age=N")
}

func TestLoad_DevAllowsNoAuthAndNoDB(t *testing.T) {
	// The headline P0 promise: control mode boots with zero external services in dev.
	env := devBase()
	env["KOTOJI_RUN_MODE"] = "control"
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Equal(t, AuthModeNone, cfg.AuthMode)
	assert.Empty(t, cfg.DatabaseURL)
}

func TestLoad_InvalidEnums(t *testing.T) {
	cases := map[string]map[string]string{
		"bad env":       {"KOTOJI_ENV": "staging"},
		"bad run mode":  {"KOTOJI_ENV": "development", "KOTOJI_RUN_MODE": "both"},
		"bad auth mode": {"KOTOJI_ENV": "development", "KOTOJI_AUTH_MODE": "magic"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFromMap(env)
			require.Error(t, err)
		})
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	env := devBase()
	env["KOTOJI_SESSION_TTL"] = "not-a-duration"
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KOTOJI_SESSION_TTL")
}

func TestLoad_ProductionRequiresEssentials(t *testing.T) {
	// Bare production env: every hard-required field is missing.
	env := map[string]string{
		"KOTOJI_ENV":       "production",
		"KOTOJI_AUTH_MODE": "password",
	}
	_, err := LoadFromMap(env)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "KOTOJI_DATABASE_URL")
	assert.Contains(t, msg, "KOTOJI_BASE_DOMAIN")
	assert.Contains(t, msg, "KOTOJI_CONTROL_BASE_URL")
	// H2: the at-rest encryption key is now a production essential too.
	assert.Contains(t, msg, "KOTOJI_SECRET_KEY")
}

// TestLoad_PasswordModeAllowsEmptyEnvPassword is the first-run setup contract:
// AUTH_MODE=password with NO env password is valid — the instance boots into the
// "setup required" state and the admin sets the password via the GUI (DB hash).
func TestLoad_PasswordModeAllowsEmptyEnvPassword(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "password"
	delete(env, "KOTOJI_AUTH_ADMIN_PASSWORD")
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Equal(t, AuthModePassword, cfg.AuthMode)
	assert.Empty(t, cfg.AdminPassword)
}

// TestLoad_PasswordModeRejectsShortEnvPassword: when an env password IS provided
// it must still meet the shared minimum length.
func TestLoad_PasswordModeRejectsShortEnvPassword(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "password"
	env["KOTOJI_AUTH_ADMIN_PASSWORD"] = "short"
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KOTOJI_AUTH_ADMIN_PASSWORD")
}

func TestLoad_ProductionRejectsNoAuth(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "none"
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AUTH_MODE=none is not allowed in production")
}

// TestLoad_ProductionRejectsInsecureCookies is the M2 fail-closed guard: in
// production, KOTOJI_COOKIE_SECURE=false is refused at Load because the __Host-
// cookies' Secure attribute is the sole cross-subdomain isolation control.
func TestLoad_ProductionRejectsInsecureCookies(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "password"
	env["KOTOJI_COOKIE_SECURE"] = "false"
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KOTOJI_COOKIE_SECURE=false is not allowed in production")
}

// TestLoad_ProductionAllowsSecureCookies: the same prod env with secure cookies
// (the production default) loads cleanly — the guard only trips on an explicit
// downgrade.
func TestLoad_ProductionAllowsSecureCookies(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "password"
	// KOTOJI_COOKIE_SECURE unset => defaults true in production.
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.True(t, cfg.CookieSecure)

	// Explicit true is also accepted.
	env["KOTOJI_COOKIE_SECURE"] = "true"
	cfg, err = LoadFromMap(env)
	require.NoError(t, err)
	assert.True(t, cfg.CookieSecure)
}

// TestLoad_DevelopmentAllowsInsecureCookies: development over plain http may keep
// KOTOJI_COOKIE_SECURE=false; the invariant is production-only.
func TestLoad_DevelopmentAllowsInsecureCookies(t *testing.T) {
	env := devBase()
	env["KOTOJI_COOKIE_SECURE"] = "false"
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.False(t, cfg.CookieSecure)
}

func TestLoad_ProductionOIDCRequiresCreds(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "oidc"
	// No client id/secret/allowlist provided.
	_, err := LoadFromMap(env)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "KOTOJI_AUTH_OIDC_CLIENT_ID")
	assert.Contains(t, msg, "KOTOJI_AUTH_OIDC_CLIENT_SECRET")
	// Fail-closed: with neither allowlist configured, oidc refuses to boot.
	assert.Contains(t, msg, "KOTOJI_OIDC_ALLOWED_EMAILS")
	assert.Contains(t, msg, "KOTOJI_OIDC_ALLOWED_DOMAINS")
}

func TestLoad_ProductionOIDCHappyPath(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "oidc"
	env["KOTOJI_AUTH_OIDC_CLIENT_ID"] = "client-123"
	env["KOTOJI_AUTH_OIDC_CLIENT_SECRET"] = "secret-xyz"
	env["KOTOJI_AUTH_GOOGLE_HD"] = "necorox.com"

	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.True(t, cfg.IsProduction())
	assert.True(t, cfg.CookieSecure, "production defaults to secure cookies")
	assert.Equal(t, "kotoji.example.com", cfg.ControlHost)
	assert.Equal(t, "kotoji.example.com", cfg.SessionCookieDomain, "session cookie defaults to host-only control host")
}

func TestLoad_GitHubMirrorRequiresCreds(t *testing.T) {
	env := devBase()
	env["KOTOJI_GITHUB_MIRROR_ENABLED"] = "true"
	_, err := LoadFromMap(env)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "KOTOJI_GITHUB_APP_TOKEN")
	assert.Contains(t, msg, "KOTOJI_GITHUB_WEBHOOK_SECRET")
}

func TestLoad_AllowedEmailsParsed(t *testing.T) {
	env := devBase()
	env["KOTOJI_AUTH_ALLOWED_EMAILS"] = " a@x.com , b@y.com ,"
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Equal(t, []string{"a@x.com", "b@y.com"}, cfg.OIDC.AllowedEmails)
}

func TestLoad_CustomDurationAndAddrs(t *testing.T) {
	env := devBase()
	env["KOTOJI_SESSION_TTL"] = "48h"
	env["KOTOJI_CONTROL_ADDR"] = ":9090"
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Equal(t, 48*time.Hour, cfg.SessionTTL)
	assert.Equal(t, ":9090", cfg.ControlAddr)
}

// TestLoad_ZipMaxEntryBytes is the M7 guard: the per-entry uncompressed cap is now
// operator-configurable via KOTOJI_ZIP_MAX_ENTRY_BYTES, defaulting to the previous
// silent 50MiB value when unset.
func TestLoad_ZipMaxEntryBytes(t *testing.T) {
	t.Run("default is 50MiB when unset", func(t *testing.T) {
		cfg, err := LoadFromMap(devBase())
		require.NoError(t, err)
		assert.Equal(t, int64(52428800), cfg.Zip.MaxEntryBytes, "default per-entry cap is 50MiB")
	})

	t.Run("env override is parsed", func(t *testing.T) {
		env := devBase()
		env["KOTOJI_ZIP_MAX_ENTRY_BYTES"] = "104857600" // 100MiB
		cfg, err := LoadFromMap(env)
		require.NoError(t, err)
		assert.Equal(t, int64(104857600), cfg.Zip.MaxEntryBytes)
	})

	t.Run("invalid value falls back to default", func(t *testing.T) {
		env := devBase()
		env["KOTOJI_ZIP_MAX_ENTRY_BYTES"] = "not-a-number"
		cfg, err := LoadFromMap(env)
		require.NoError(t, err)
		assert.Equal(t, int64(52428800), cfg.Zip.MaxEntryBytes)
	})
}

// productionEnv returns a production env with the non-auth essentials filled in,
// so individual tests can isolate auth-specific failures. It includes a valid
// KOTOJI_SECRET_KEY (H2: production hard-requires an explicit at-rest key) so the
// helper alone yields a bootable prod config.
func productionEnv() map[string]string {
	return map[string]string{
		"KOTOJI_ENV":              "production",
		"KOTOJI_DATABASE_URL":     "postgres://u:p@db:5432/kotoji?sslmode=require",
		"KOTOJI_BASE_DOMAIN":      "hosting.example.com",
		"KOTOJI_CONTROL_BASE_URL": "https://kotoji.example.com",
		// 64 hex chars = 32 bytes (a valid explicit key). Test-only fixed value.
		"KOTOJI_SECRET_KEY": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}

func TestLoad_DeriveHostInvalidURL(t *testing.T) {
	env := devBase()
	env["KOTOJI_CONTROL_BASE_URL"] = "://missing-scheme"
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "KOTOJI_CONTROL_BASE_URL"))
}

// TestParseAuthModes covers the pure set parser: legacy single values, comma
// lists, normalization, dedup, the exclusive "none", and invalid entries.
func TestParseAuthModes(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantModes []AuthMode
		wantPrim  AuthMode
		wantErr   bool
	}{
		{name: "legacy single oidc", raw: "oidc", wantModes: []AuthMode{AuthModeOIDC}, wantPrim: AuthModeOIDC},
		{name: "legacy single password", raw: "password", wantModes: []AuthMode{AuthModePassword}, wantPrim: AuthModePassword},
		{name: "legacy single none", raw: "none", wantModes: []AuthMode{AuthModeNone}, wantPrim: AuthModeNone},
		{name: "set oidc+password", raw: "oidc,password", wantModes: []AuthMode{AuthModeOIDC, AuthModePassword}, wantPrim: AuthModeOIDC},
		{name: "set reorders to normalized order", raw: "password,oidc", wantModes: []AuthMode{AuthModeOIDC, AuthModePassword}, wantPrim: AuthModeOIDC},
		{name: "set dedups + trims + uppercases", raw: " OIDC , oidc , Password ", wantModes: []AuthMode{AuthModeOIDC, AuthModePassword}, wantPrim: AuthModeOIDC},
		{name: "none exclusive with oidc -> error", raw: "none,oidc", wantErr: true},
		{name: "invalid entry -> error", raw: "oidc,magic", wantErr: true},
		{name: "empty -> error", raw: "  ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			modes, prim, err := parseAuthModes(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantModes, modes)
			assert.Equal(t, tc.wantPrim, prim)
		})
	}
}

// TestLoad_AuthModeSet_OIDCAndPassword is the headline break-glass config: oidc +
// password enabled together, validated independently, with the legacy authMode
// representative resolving to oidc (the primary human path).
func TestLoad_AuthModeSet_OIDCAndPassword(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "oidc,password"
	env["KOTOJI_AUTH_OIDC_CLIENT_ID"] = "client-123"
	env["KOTOJI_AUTH_OIDC_CLIENT_SECRET"] = "secret-xyz"
	env["KOTOJI_OIDC_ALLOWED_DOMAINS"] = "necorox.com"
	env["KOTOJI_AUTH_ADMIN_PASSWORD"] = "break-glass-pw-1234"

	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.True(t, cfg.OIDCEnabled())
	assert.True(t, cfg.PasswordEnabled())
	assert.False(t, cfg.NoAuthEnabled())
	assert.Equal(t, AuthModeOIDC, cfg.AuthMode, "legacy representative is oidc when both are enabled")
	assert.Equal(t, []string{"oidc", "password"}, cfg.AuthModeStrings())
}

// TestLoad_AuthModeSet_OIDCWithoutAllowlistFailsClosed: oidc enabled (even
// alongside password) with empty allowlists is rejected at boot.
func TestLoad_AuthModeSet_OIDCWithoutAllowlistFailsClosed(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "oidc,password"
	env["KOTOJI_AUTH_OIDC_CLIENT_ID"] = "client-123"
	env["KOTOJI_AUTH_OIDC_CLIENT_SECRET"] = "secret-xyz"
	env["KOTOJI_AUTH_ADMIN_PASSWORD"] = "break-glass-pw-1234"
	// No ALLOWED_EMAILS / ALLOWED_DOMAINS -> fail-closed.
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fail-closed")
}

// TestLoad_OIDCAllowlists_Parsed checks the new env vars parse + normalize and
// that the legacy GOOGLE_HD folds into AllowedDomains.
func TestLoad_OIDCAllowlists_Parsed(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "oidc"
	env["KOTOJI_AUTH_OIDC_CLIENT_ID"] = "client-123"
	env["KOTOJI_AUTH_OIDC_CLIENT_SECRET"] = "secret-xyz"
	env["KOTOJI_OIDC_ALLOWED_EMAILS"] = " Alice@Corp.com , bob@corp.com "
	env["KOTOJI_OIDC_ALLOWED_DOMAINS"] = " Partner.io ,necorox.com"
	env["KOTOJI_AUTH_GOOGLE_HD"] = "Legacy.com" // folds into AllowedDomains
	env["KOTOJI_OIDC_ADMIN_EMAILS"] = "Alice@Corp.com"

	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Equal(t, []string{"alice@corp.com", "bob@corp.com"}, cfg.OIDC.AllowedEmails)
	assert.Equal(t, []string{"partner.io", "necorox.com", "legacy.com"}, cfg.OIDC.AllowedDomains)
	assert.Equal(t, []string{"alice@corp.com"}, cfg.OIDC.AdminEmails)
}

// TestLoad_NoneExclusive: "none,password" is rejected by Load (exclusive).
func TestLoad_NoneExclusive(t *testing.T) {
	env := devBase()
	env["KOTOJI_AUTH_MODE"] = "none,password"
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exclusive")
}

// --- kotoji-native on-demand TLS (KOTOJI_TLS_MODE) ---

// TestLoad_TLSDefaultsOff pins the headline invariant: with no TLS env, mode is
// OFF and ServesTLS is false — today's plain-HTTP behavior is untouched.
func TestLoad_TLSDefaultsOff(t *testing.T) {
	cfg, err := LoadFromMap(devBase())
	require.NoError(t, err)
	assert.Equal(t, TLSModeOff, cfg.TLSMode)
	assert.Equal(t, TLSCAProd, cfg.TLSCA)
	assert.False(t, cfg.ServesTLS(), "off mode must not boot the TLS engine")
	assert.Equal(t, ":443", cfg.TLSAddr)
	assert.Equal(t, ":80", cfg.TLSHTTPAddr)
	// Cert storage is derived under the data dir.
	assert.Equal(t, "/data/certmagic", cfg.CertMagicStorageDir)
}

// TestLoad_TLSAutoRequiresRunModeAll: auto mode is rejected unless RUN_MODE=all,
// since the single :443 listener fronts BOTH planes via Host routing.
func TestLoad_TLSAutoRequiresRunModeAll(t *testing.T) {
	for _, mode := range []string{"control", "serve"} {
		env := devBase()
		env["KOTOJI_TLS_MODE"] = "auto"
		env["KOTOJI_RUN_MODE"] = mode
		_, err := LoadFromMap(env)
		require.Error(t, err, "auto + run_mode=%s must fail", mode)
		assert.Contains(t, err.Error(), "KOTOJI_TLS_MODE=auto requires KOTOJI_RUN_MODE=all")
	}
}

// TestLoad_TLSAutoAllValid: auto + all loads cleanly and ServesTLS is true.
func TestLoad_TLSAutoAllValid(t *testing.T) {
	env := devBase()
	env["KOTOJI_TLS_MODE"] = "auto" // RUN_MODE defaults to all
	env["KOTOJI_ACME_EMAIL"] = "ops@example.com"
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Equal(t, TLSModeAuto, cfg.TLSMode)
	assert.True(t, cfg.ServesTLS())
	assert.Equal(t, "ops@example.com", cfg.ACMEEmail)
}

// TestLoad_TLSStagingToggle: KOTOJI_TLS_CA=staging selects the staging directory.
func TestLoad_TLSStagingToggle(t *testing.T) {
	env := devBase()
	env["KOTOJI_TLS_MODE"] = "auto"
	env["KOTOJI_TLS_CA"] = "staging"
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.True(t, cfg.TLSStaging())
	assert.Equal(t, TLSCAStaging, cfg.TLSCA)
}

// TestLoad_TLSInvalidEnums: bad TLS_MODE / TLS_CA values are rejected at load.
func TestLoad_TLSInvalidEnums(t *testing.T) {
	cases := map[string]map[string]string{
		"bad tls mode": {"KOTOJI_TLS_MODE": "on"},
		"bad tls ca":   {"KOTOJI_TLS_MODE": "auto", "KOTOJI_TLS_CA": "letsencrypt"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			env := devBase()
			for k, v := range extra {
				env[k] = v
			}
			_, err := LoadFromMap(env)
			require.Error(t, err)
		})
	}
}

// TestLoad_CertMagicStorageFollowsDataDir: the cert store tracks KOTOJI_DATA_DIR.
func TestLoad_CertMagicStorageFollowsDataDir(t *testing.T) {
	env := devBase()
	env["KOTOJI_DATA_DIR"] = "/srv/kotoji"
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Equal(t, "/srv/kotoji/certmagic", cfg.CertMagicStorageDir)
}

// --- H2: production REQUIRES an explicit high-entropy KOTOJI_SECRET_KEY ---

// TestLoad_ProductionRequiresSecretKey: a prod control-plane env WITHOUT
// KOTOJI_SECRET_KEY (so the at-rest key would be derived from public inputs) is
// rejected fail-closed.
func TestLoad_ProductionRequiresSecretKey(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "password"
	delete(env, "KOTOJI_SECRET_KEY")
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KOTOJI_SECRET_KEY")
}

// TestLoad_ProductionRejectsShortDerivableSecretKey: a value that does NOT decode
// to >= 32 bytes is treated as "no explicit key" (it would fall back to the derived
// key) and is rejected in production.
func TestLoad_ProductionRejectsShortDerivableSecretKey(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "password"
	env["KOTOJI_SECRET_KEY"] = "tooshort" // < 32 decoded bytes
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KOTOJI_SECRET_KEY")
}

// TestLoad_ProductionAcceptsExplicitSecretKey: hex and base64 keys that decode to
// >= 32 bytes satisfy the requirement and boot cleanly.
func TestLoad_ProductionAcceptsExplicitSecretKey(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	cases := map[string]string{
		"hex":    hex.EncodeToString(raw),
		"base64": base64.StdEncoding.EncodeToString(raw),
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			env := productionEnv()
			env["KOTOJI_AUTH_MODE"] = "password"
			env["KOTOJI_SECRET_KEY"] = key
			cfg, err := LoadFromMap(env)
			require.NoError(t, err)
			assert.Equal(t, key, cfg.SecretKey)
		})
	}
}

// TestLoad_DevelopmentAllowsDerivedSecretKey: in DEV the derived fallback is fine —
// no KOTOJI_SECRET_KEY is required (zero-config local stack).
func TestLoad_DevelopmentAllowsDerivedSecretKey(t *testing.T) {
	env := devBase()
	_, ok := env["KOTOJI_SECRET_KEY"]
	require.False(t, ok, "dev base must not set a secret key")
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.Empty(t, cfg.SecretKey, "dev derives the key; no explicit key needed")
}

// TestLoad_ProductionServeModeSkipsSecretKey: a data-plane-only (serve) prod
// instance does not run the control plane, so it never stores secrets and does not
// require the key. (It still requires the other prod essentials.)
func TestLoad_ProductionServeModeSkipsSecretKey(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_RUN_MODE"] = "serve"
	delete(env, "KOTOJI_SECRET_KEY")
	cfg, err := LoadFromMap(env)
	require.NoError(t, err)
	assert.False(t, cfg.ServesControl())
}

// TestExplicitSecretKey is the pure predicate the H2 check gates on.
func TestExplicitSecretKey(t *testing.T) {
	raw := make([]byte, 32)
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty", in: "", want: false},
		{name: "blank", in: "   ", want: false},
		{name: "short hex", in: "abcd", want: false},
		{name: "valid hex 32B", in: hex.EncodeToString(raw), want: true},
		{name: "valid base64 32B", in: base64.StdEncoding.EncodeToString(raw), want: true},
		{name: "valid raw base64 32B", in: base64.RawStdEncoding.EncodeToString(raw), want: true},
		{name: "undecodable", in: "!!! not a key !!!", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, explicitSecretKey(tc.in))
		})
	}
}
