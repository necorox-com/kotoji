package preview

import (
	"bytes"
	"testing"
)

// explicitKey is a valid 32-byte (64 hex char) KOTOJI_SECRET_KEY for tests that
// exercise the explicit-key path of Secret.
const explicitKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func TestSecret_DeterministicAndKeyLength(t *testing.T) {
	a := Secret("", "pw", "oidc", "https://ctl.example", "hosting.example.com")
	b := Secret("", "pw", "oidc", "https://ctl.example", "hosting.example.com")
	if !bytes.Equal(a, b) {
		t.Fatal("Secret must be deterministic for identical inputs")
	}
	if len(a) != 32 {
		t.Fatalf("Secret length = %d want 32 (hmac-sha256)", len(a))
	}
}

func TestSecret_DiffersOnInput(t *testing.T) {
	base := Secret("", "pw", "oidc", "https://ctl.example", "hosting.example.com")
	cases := [][4]string{
		{"PW", "oidc", "https://ctl.example", "hosting.example.com"},
		{"pw", "OIDC", "https://ctl.example", "hosting.example.com"},
		{"pw", "oidc", "https://other.example", "hosting.example.com"},
		{"pw", "oidc", "https://ctl.example", "other.example.com"},
	}
	for i, c := range cases {
		if bytes.Equal(base, Secret("", c[0], c[1], c[2], c[3])) {
			t.Fatalf("case %d: Secret must change when an input changes", i)
		}
	}
}

// TestSecret_ConsumesExplicitKey proves the preview-grant key binds to the
// explicit KOTOJI_SECRET_KEY: two configs differing ONLY in the explicit key
// (all seed inputs identical) MUST yield different keys. Without consuming the
// explicit key the grant HMAC would collapse to a publicly-derivable constant
// in the standard DB-config production deployment (the H2 omission this fixes).
func TestSecret_ConsumesExplicitKey(t *testing.T) {
	derived := Secret("", "pw", "oidc", "https://ctl.example", "hosting.example.com")
	withKey := Secret(explicitKey, "pw", "oidc", "https://ctl.example", "hosting.example.com")
	if bytes.Equal(derived, withKey) {
		t.Fatal("Secret must consume the explicit KOTOJI_SECRET_KEY (key must change the result)")
	}

	// A second, DIFFERENT explicit key must also produce a different result so the
	// key is genuinely an input (not merely a present/absent toggle).
	otherKey := "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	withOther := Secret(otherKey, "pw", "oidc", "https://ctl.example", "hosting.example.com")
	if bytes.Equal(withKey, withOther) {
		t.Fatal("Secret must differ for two distinct explicit keys")
	}
}
