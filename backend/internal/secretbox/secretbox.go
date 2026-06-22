// Package secretbox is the symmetric authenticated-encryption helper kotoji uses
// to store secrets (the GitHub mirror PAT) at rest in the database. It wraps
// AES-256-GCM behind a tiny Seal/Open seam so callers never touch the cipher
// directly and a misuse (nonce reuse, wrong key length) is impossible at the
// call site.
//
// Threat model: the ciphertext lives in instance_settings.value (Postgres). The
// key is held only in process memory (derived from config, see KeyFromSeed). An
// attacker with DB read access cannot recover the plaintext token without the
// key; a tampered ciphertext fails GCM authentication and Open returns ok=false.
//
// Failure policy (LOCKED): Open NEVER panics and NEVER returns an error. On any
// failure (wrong/rotated key, truncated/tampered ciphertext, malformed base64)
// it returns ("", false). Callers treat ok=false as "secret not configured" so a
// rotated KOTOJI_SECRET_KEY degrades gracefully (the admin re-enters the token)
// instead of crashing the instance.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

// KeySize is the AES-256 key length in bytes. The Box constructor enforces it so
// a short key can never silently weaken the cipher.
const KeySize = 32

// ciphertextPrefix domain-tags the stored value so a future migration to a
// different scheme (or a versioned key) can be detected. It is part of the
// base64-decoded blob layout: prefixByte || nonce || gcmSealed.
const versionByte byte = 1

// Box is a configured AES-256-GCM sealer/opener bound to one 32-byte key. It is
// safe for concurrent use (the underlying cipher.AEAD is stateless across calls;
// each Seal mints a fresh random nonce).
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a 32-byte key. A key of the wrong length is a programming
// error (the config layer guarantees 32 bytes via KeyFromSeed / a validated
// KOTOJI_SECRET_KEY), so it is reported as an error the composition root surfaces
// at boot rather than swallowed.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, errors.New("secretbox: key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal authenticates+encrypts plaintext and returns a base64 (std) string safe to
// store in a TEXT column. Layout (pre-base64): versionByte || nonce(12) ||
// ciphertext+tag. A fresh CSPRNG nonce is generated per call (GCM is catastrophic
// under nonce reuse — never derive the nonce from the plaintext or a counter).
func (b *Box) Seal(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	// Prepend the version byte to the output, then seal with the nonce. Seal
	// appends the ciphertext+tag to its first arg, so we hand it a slice that
	// already holds version||nonce and let it grow.
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, versionByte)
	out = append(out, nonce...)
	out = b.aead.Seal(out, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Open reverses Seal. It returns (plaintext, true) on success and ("", false) on
// ANY failure — bad base64, wrong version, truncated blob, or an authentication
// failure (wrong/rotated key or tampering). It never returns an error or panics
// (LOCKED failure policy): callers map ok=false to "secret not configured".
func (b *Box) Open(encoded string) (string, bool) {
	if encoded == "" {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	ns := b.aead.NonceSize()
	// Need at least version(1) + nonce(ns) + tag(Overhead) bytes to be valid.
	if len(raw) < 1+ns+b.aead.Overhead() {
		return "", false
	}
	if raw[0] != versionByte {
		return "", false
	}
	nonce := raw[1 : 1+ns]
	sealed := raw[1+ns:]
	plain, err := b.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		// Authentication failed: wrong key (rotated KOTOJI_SECRET_KEY) or tampering.
		return "", false
	}
	return string(plain), true
}
