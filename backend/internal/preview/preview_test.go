package preview

import (
	"bytes"
	"testing"
)

func TestSecret_DeterministicAndKeyLength(t *testing.T) {
	a := Secret("pw", "oidc", "https://ctl.example", "hosting.example.com")
	b := Secret("pw", "oidc", "https://ctl.example", "hosting.example.com")
	if !bytes.Equal(a, b) {
		t.Fatal("Secret must be deterministic for identical inputs")
	}
	if len(a) != 32 {
		t.Fatalf("Secret length = %d want 32 (sha256)", len(a))
	}
}

func TestSecret_DiffersOnInput(t *testing.T) {
	base := Secret("pw", "oidc", "https://ctl.example", "hosting.example.com")
	cases := [][4]string{
		{"PW", "oidc", "https://ctl.example", "hosting.example.com"},
		{"pw", "OIDC", "https://ctl.example", "hosting.example.com"},
		{"pw", "oidc", "https://other.example", "hosting.example.com"},
		{"pw", "oidc", "https://ctl.example", "other.example.com"},
	}
	for i, c := range cases {
		if bytes.Equal(base, Secret(c[0], c[1], c[2], c[3])) {
			t.Fatalf("case %d: Secret must change when an input changes", i)
		}
	}
}
