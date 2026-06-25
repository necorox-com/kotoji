package auth

import (
	"bytes"
	"testing"

	"github.com/necorox-com/kotoji/backend/internal/config"
)

// signKeyExplicit is a valid 32-byte (64 hex char) KOTOJI_SECRET_KEY used to
// exercise the explicit-key path of deriveSignKey.
const signKeyExplicit = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// baseSignKeyConfig is a config with stable seed inputs (admin password / OIDC
// secret / control + base domains) but no explicit key, so each test can vary
// ONLY the field under test.
func baseSignKeyConfig() config.Config {
	cfg := config.Config{
		AdminPassword:  "admin-pw",
		ControlBaseURL: "https://ctl.example",
		BaseDomain:     "hosting.example.com",
	}
	cfg.OIDC.ClientSecret = "oidc-secret"
	return cfg
}

// TestDeriveSignKey_ConsumesExplicitKey is the CONFIRMED-FINDING regression test:
// two configs differing ONLY in KOTOJI_SECRET_KEY MUST produce DIFFERENT login-
// state sign keys. Before the fix deriveSignKey ignored cfg.SecretKey entirely, so
// in the standard DB-config production deployment (admin password / client secret
// in the DB, empty in env) the key collapsed to a value reproducible from the
// public control URL — a forgeable login-state HMAC. Consuming the explicit key
// (via secretbox.ResolveKey) makes the HMAC as strong as the at-rest key.
func TestDeriveSignKey_ConsumesExplicitKey(t *testing.T) {
	withoutKey := baseSignKeyConfig()

	withKey := baseSignKeyConfig()
	withKey.SecretKey = signKeyExplicit

	k1 := deriveSignKey(withoutKey)
	k2 := deriveSignKey(withKey)
	if bytes.Equal(k1, k2) {
		t.Fatal("deriveSignKey must change when KOTOJI_SECRET_KEY is set (explicit key not consumed)")
	}

	// A second, DIFFERENT explicit key must also change the result so the key is a
	// genuine input rather than a present/absent toggle.
	withOther := baseSignKeyConfig()
	withOther.SecretKey = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	k3 := deriveSignKey(withOther)
	if bytes.Equal(k2, k3) {
		t.Fatal("deriveSignKey must differ for two distinct explicit keys")
	}
}

// TestDeriveSignKey_DeterministicWithExplicitKey verifies that, given an explicit
// key, the derivation is stable across calls (so process restarts with the same
// config keep in-flight login-state cookies valid — the property that makes the
// key roll acceptable).
func TestDeriveSignKey_DeterministicWithExplicitKey(t *testing.T) {
	cfg := baseSignKeyConfig()
	cfg.SecretKey = signKeyExplicit

	a := deriveSignKey(cfg)
	b := deriveSignKey(cfg)
	if !bytes.Equal(a, b) {
		t.Fatal("deriveSignKey must be deterministic for identical config (with explicit key)")
	}
	if len(a) != 32 {
		t.Fatalf("deriveSignKey length = %d want 32 (hmac-sha256)", len(a))
	}
}

// TestDeriveSignKey_DeterministicFromSeed verifies the derived (no explicit key)
// path is still stable across calls whenever seed material is present, so an
// env-only / DB-config instance keeps cookies valid across restarts.
func TestDeriveSignKey_DeterministicFromSeed(t *testing.T) {
	cfg := baseSignKeyConfig()
	a := deriveSignKey(cfg)
	b := deriveSignKey(cfg)
	if !bytes.Equal(a, b) {
		t.Fatal("deriveSignKey must be deterministic from a stable seed (no explicit key)")
	}
}

// TestDeriveSignKey_RandomWhenNoKeyMaterial verifies the dev fallback: with NO
// explicit key AND an empty seed (no admin password, OIDC secret, control URL, or
// base domain) the key is per-process random rather than a publicly-derivable
// constant. Two calls must therefore differ.
func TestDeriveSignKey_RandomWhenNoKeyMaterial(t *testing.T) {
	cfg := config.Config{} // all seed fields empty, no explicit key
	a := deriveSignKey(cfg)
	b := deriveSignKey(cfg)
	if bytes.Equal(a, b) {
		t.Fatal("deriveSignKey must be per-process random when there is no key material")
	}
}
