package secretbox

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// newKey returns a fresh random 32-byte key for tests.
func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := New(newKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []string{
		"",
		"ghp_exampletoken1234567890",
		"a token with spaces and symbols !@#$%^&*()",
		strings.Repeat("x", 4096),
		"日本語のトークン",
	}
	for _, pt := range cases {
		enc, err := box.Seal(pt)
		if err != nil {
			t.Fatalf("Seal(%q): %v", pt, err)
		}
		if pt != "" && enc == pt {
			t.Fatalf("ciphertext equals plaintext for %q", pt)
		}
		got, ok := box.Open(enc)
		if !ok {
			t.Fatalf("Open failed for round-trip of %q", pt)
		}
		if got != pt {
			t.Fatalf("round-trip mismatch: got %q want %q", got, pt)
		}
	}
}

func TestSealProducesDistinctCiphertexts(t *testing.T) {
	box, err := New(newKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Fresh nonce per Seal => identical plaintext yields different ciphertext.
	a, _ := box.Seal("same")
	b, _ := box.Seal("same")
	if a == b {
		t.Fatal("two Seals of the same plaintext produced identical ciphertext (nonce reuse?)")
	}
}

func TestOpenWithWrongKeyReturnsNotOk(t *testing.T) {
	box1, _ := New(newKey(t))
	box2, _ := New(newKey(t)) // a different (rotated) key

	enc, err := box1.Seal("secret-token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// LOCKED policy: a wrong/rotated key => ok=false, never an error or panic.
	if got, ok := box2.Open(enc); ok || got != "" {
		t.Fatalf("Open with wrong key = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestOpenRejectsTamperedAndMalformed(t *testing.T) {
	box, _ := New(newKey(t))
	enc, _ := box.Seal("token")

	// Tamper: flip a character in the base64 body.
	tampered := []byte(enc)
	// Mutate a byte in the middle to avoid only touching padding.
	tampered[len(tampered)/2] ^= 0x01
	if got, ok := box.Open(string(tampered)); ok || got != "" {
		t.Fatalf("Open(tampered) = (%q, %v), want (\"\", false)", got, ok)
	}

	for _, bad := range []string{
		"",               // empty
		"not base64 @@@", // bad base64
		base64.StdEncoding.EncodeToString([]byte("short")),                           // too short to hold version+nonce+tag
		base64.StdEncoding.EncodeToString(append([]byte{0x02}, make([]byte, 64)...)), // wrong version byte
	} {
		if got, ok := box.Open(bad); ok || got != "" {
			t.Fatalf("Open(%q) = (%q, %v), want (\"\", false)", bad, got, ok)
		}
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := New(make([]byte, n)); err == nil {
			t.Fatalf("New(%d-byte key) = nil error, want error", n)
		}
	}
}

func TestResolveKeyExplicitHex(t *testing.T) {
	raw := newKey(t)
	hexKey := hex.EncodeToString(raw)
	got := ResolveKey(hexKey, "seed", "oidc", "url", "domain")
	if len(got) != KeySize {
		t.Fatalf("key len = %d, want %d", len(got), KeySize)
	}
	if string(got) != string(raw) {
		t.Fatal("hex KOTOJI_SECRET_KEY was not used verbatim")
	}
}

func TestResolveKeyExplicitBase64(t *testing.T) {
	raw := newKey(t)
	b64 := base64.StdEncoding.EncodeToString(raw)
	got := ResolveKey(b64, "seed", "oidc", "url", "domain")
	if string(got) != string(raw) {
		t.Fatal("base64 KOTOJI_SECRET_KEY was not used verbatim")
	}
}

func TestResolveKeyDerivedIsStableAndSensitive(t *testing.T) {
	// Empty / too-short env => deterministic derived key from the seed inputs.
	a := ResolveKey("", "pwhash", "oidc", "https://ctl", "example.com")
	b := ResolveKey("short", "pwhash", "oidc", "https://ctl", "example.com")
	if len(a) != KeySize {
		t.Fatalf("derived key len = %d, want %d", len(a), KeySize)
	}
	if string(a) != string(b) {
		t.Fatal("derived key changed across calls with the same seed (must be stable)")
	}
	// A different seed input yields a different key.
	c := ResolveKey("", "DIFFERENT", "oidc", "https://ctl", "example.com")
	if string(a) == string(c) {
		t.Fatal("derived key did not change when a seed input changed")
	}
}

func TestResolveKeyDerivedDiffersFromExplicit(t *testing.T) {
	// Round-trip through a Box built from a derived key, then confirm a Box built
	// from a DIFFERENT (rotated) derivation cannot open it.
	k1 := ResolveKey("", "pw1", "o", "u", "d")
	k2 := ResolveKey("", "pw2", "o", "u", "d")
	b1, _ := New(k1)
	b2, _ := New(k2)
	enc, _ := b1.Seal("tok")
	if _, ok := b2.Open(enc); ok {
		t.Fatal("rotated derived key should not open the old ciphertext")
	}
}
