package domaincfg

import (
	"errors"
	"testing"
)

func TestValidateBaseDomain(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"simple fqdn", "hosting.example.com", true},
		{"single label", "localhost", true},
		{"with hyphen", "my-host.example.com", true},
		{"digits", "host1.example2.com", true},
		{"long but valid", "a.b.c.d.e.example.com", true},
		{"empty rejected", "", false},
		{"uppercase rejected", "Hosting.example.com", false},
		{"scheme rejected", "https://hosting.example.com", false},
		{"port rejected", "hosting.example.com:8080", false},
		{"path rejected", "hosting.example.com/x", false},
		{"trailing dot rejected", "hosting.example.com.", false},
		{"leading hyphen label rejected", "-bad.example.com", false},
		{"trailing hyphen label rejected", "bad-.example.com", false},
		{"consecutive dots rejected", "bad..example.com", false},
		{"space rejected", "bad host.com", false},
		{"underscore rejected", "bad_host.com", false},
		{"wildcard rejected", "*.example.com", false},
		{"label over 63 rejected", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateBaseDomain(c.in)
			if c.ok && err != nil {
				t.Fatalf("ValidateBaseDomain(%q) = %v, want nil", c.in, err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("ValidateBaseDomain(%q) = nil, want error", c.in)
				}
				if !errors.Is(err, ErrInvalidBaseDomain) {
					t.Fatalf("error %v does not wrap ErrInvalidBaseDomain", err)
				}
				if Reason(err) == "" {
					t.Fatalf("expected a human reason for %q", c.in)
				}
			}
		})
	}
}

func TestValidateControlBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"https origin", "https://hosting.example.com", true},
		{"http origin", "http://hosting.example.com", true},
		{"with port", "https://hosting.example.com:8443", true},
		{"trailing slash tolerated", "https://hosting.example.com/", true},
		{"localhost dev", "http://hosting.localhost:8080", true},
		{"empty rejected", "", false},
		{"no scheme rejected", "hosting.example.com", false},
		{"ftp scheme rejected", "ftp://hosting.example.com", false},
		{"no host rejected", "https://", false},
		{"path rejected", "https://hosting.example.com/app", false},
		{"fragment rejected", "https://hosting.example.com/#x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateControlBaseURL(c.in)
			if c.ok && err != nil {
				t.Fatalf("ValidateControlBaseURL(%q) = %v, want nil", c.in, err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("ValidateControlBaseURL(%q) = nil, want error", c.in)
				}
				if !errors.Is(err, ErrInvalidControlBaseURL) {
					t.Fatalf("error %v does not wrap ErrInvalidControlBaseURL", err)
				}
			}
		})
	}
}

func TestNormalizeControlBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://h.example.com/":    "https://h.example.com",
		"https://h.example.com//":   "https://h.example.com",
		"https://h.example.com":     "https://h.example.com",
		"  https://h.example.com/ ": "https://h.example.com",
	}
	for in, want := range cases {
		if got := NormalizeControlBaseURL(in); got != want {
			t.Fatalf("NormalizeControlBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
