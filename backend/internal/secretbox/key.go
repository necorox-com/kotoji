package secretbox

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// keySeedPrefix domain-separates the at-rest secret key from any other HMAC/key
// derivation that reuses the same config inputs (e.g. internal/preview's
// preview-grant key). Without distinct prefixes two derivations over identical
// seeds would collide.
const keySeedPrefix = "kotoji-secretbox|"

// ResolveKey produces the 32-byte AES-256 key for the Box. Resolution order
// (LOCKED): an explicit KOTOJI_SECRET_KEY (env) wins when it decodes (hex OR
// base64) to >= 32 bytes — its FIRST 32 bytes are used; otherwise the key is
// derived deterministically via sha256 over a stable server seed (the same
// admin-password-hash | oidc-secret | control-base-url | base-domain inputs the
// preview-grant key binds to). The derived path means an env-only deployment
// with no KOTOJI_SECRET_KEY still gets a stable key across restarts, so tokens
// sealed before a restart remain decryptable.
//
// All inputs are explicit strings (not a config.Config) so this package stays
// dependency-free and importable anywhere without an import cycle.
func ResolveKey(secretKeyEnv, adminPwSeed, oidcSecret, controlBaseURL, baseDomain string) []byte {
	if k, ok := decodeExplicitKey(secretKeyEnv); ok {
		return k
	}
	seed := keySeedPrefix + adminPwSeed + "|" + oidcSecret + "|" + controlBaseURL + "|" + baseDomain
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

// ExplicitKeyProvided reports whether KOTOJI_SECRET_KEY decodes to a usable
// (>= 32-byte) explicit key — i.e. ResolveKey would return the operator-supplied
// key rather than the derived fallback. It is the single predicate both the
// config validator (H2 fail-closed: production REQUIRES an explicit key) and the
// composition root use, so "what counts as a real key" is defined in exactly one
// place. A blank/short/undecodable value returns false (derived path).
func ExplicitKeyProvided(secretKeyEnv string) bool {
	_, ok := decodeExplicitKey(secretKeyEnv)
	return ok
}

// decodeExplicitKey decodes an operator-supplied KOTOJI_SECRET_KEY. It accepts
// hex or standard/raw base64; the chosen encoding must yield AT LEAST 32 bytes
// (only the first 32 are used). A shorter/blank/undecodable value returns
// ok=false so ResolveKey falls back to the derived key. Trying hex first then
// base64 is unambiguous because a valid 64-char hex string is also valid base64
// but decodes to 32 bytes either way; we prefer the hex interpretation.
func decodeExplicitKey(v string) ([]byte, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	// hex first: a 64-char hex string is the canonical 32-byte key form.
	if b, err := hex.DecodeString(v); err == nil && len(b) >= KeySize {
		return b[:KeySize], true
	}
	// base64 (std then raw/url) fallback.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil && len(b) >= KeySize {
			return b[:KeySize], true
		}
	}
	return nil, false
}
