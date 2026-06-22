package config

import (
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

func TestLoad_ProductionOIDCRequiresCreds(t *testing.T) {
	env := productionEnv()
	env["KOTOJI_AUTH_MODE"] = "oidc"
	// No client id/secret/allowlist provided.
	_, err := LoadFromMap(env)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "KOTOJI_AUTH_OIDC_CLIENT_ID")
	assert.Contains(t, msg, "KOTOJI_AUTH_OIDC_CLIENT_SECRET")
	assert.Contains(t, msg, "KOTOJI_AUTH_GOOGLE_HD")
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

// productionEnv returns a production env with the non-auth essentials filled in,
// so individual tests can isolate auth-specific failures.
func productionEnv() map[string]string {
	return map[string]string{
		"KOTOJI_ENV":              "production",
		"KOTOJI_DATABASE_URL":     "postgres://u:p@db:5432/kotoji?sslmode=require",
		"KOTOJI_BASE_DOMAIN":      "hosting.example.com",
		"KOTOJI_CONTROL_BASE_URL": "https://kotoji.example.com",
	}
}

func TestLoad_DeriveHostInvalidURL(t *testing.T) {
	env := devBase()
	env["KOTOJI_CONTROL_BASE_URL"] = "://missing-scheme"
	_, err := LoadFromMap(env)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "KOTOJI_CONTROL_BASE_URL"))
}
