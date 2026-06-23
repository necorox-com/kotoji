package auth

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAccessPolicy_Decide is the table test for the pure access-control decision:
// allow iff email ∈ ALLOWED_EMAILS (case-insensitive) OR emailDomain ∈
// ALLOWED_DOMAINS (also honoring the Google `hd` claim); both empty ⇒ deny
// (fail-closed). The admin decision is independent (email ∈ ADMIN_EMAILS).
func TestAccessPolicy_Decide(t *testing.T) {
	cases := []struct {
		name      string
		emails    []string
		domains   []string
		admins    []string
		email     string
		hd        string
		wantAllow bool
		wantAdmin bool
	}{
		{
			name:      "fail-closed: both lists empty denies",
			email:     "anyone@corp.com",
			wantAllow: false,
		},
		{
			name:      "email allowlist exact match allows",
			emails:    []string{"alice@corp.com"},
			email:     "alice@corp.com",
			wantAllow: true,
		},
		{
			name:      "email allowlist is case-insensitive",
			emails:    []string{"Alice@Corp.com"},
			email:     "alice@corp.com",
			wantAllow: true,
		},
		{
			name:      "email not on list, no domain gate -> deny",
			emails:    []string{"alice@corp.com"},
			email:     "bob@corp.com",
			wantAllow: false,
		},
		{
			name:      "domain allowlist matches email domain part",
			domains:   []string{"corp.com"},
			email:     "anyone@corp.com",
			wantAllow: true,
		},
		{
			name:      "domain allowlist matches via verified hd claim",
			domains:   []string{"corp.com"},
			email:     "anyone@personal.example", // email domain differs...
			hd:        "corp.com",                // ...but hd matches
			wantAllow: true,
		},
		{
			name:      "domain mismatch (email + hd) -> deny",
			domains:   []string{"corp.com"},
			email:     "intruder@evil.com",
			hd:        "evil.com",
			wantAllow: false,
		},
		{
			name:      "multi-domain: second domain allows",
			domains:   []string{"corp.com", "partner.io"},
			email:     "p@partner.io",
			wantAllow: true,
		},
		{
			name:      "email OR domain: email on neither but domain ok",
			emails:    []string{"vip@other.com"},
			domains:   []string{"corp.com"},
			email:     "rando@corp.com",
			wantAllow: true,
		},
		{
			name:      "email OR domain: domain mismatch but email listed",
			emails:    []string{"vip@other.com"},
			domains:   []string{"corp.com"},
			email:     "vip@other.com",
			wantAllow: true,
		},
		{
			name:      "admin email promoted when allowed",
			domains:   []string{"corp.com"},
			admins:    []string{"boss@corp.com"},
			email:     "boss@corp.com",
			wantAllow: true,
			wantAdmin: true,
		},
		{
			name:      "admin email case-insensitive",
			domains:   []string{"corp.com"},
			admins:    []string{"Boss@Corp.com"},
			email:     "boss@corp.com",
			wantAllow: true,
			wantAdmin: true,
		},
		{
			name:      "non-admin allowed user is not promoted",
			domains:   []string{"corp.com"},
			admins:    []string{"boss@corp.com"},
			email:     "worker@corp.com",
			wantAllow: true,
			wantAdmin: false,
		},
		{
			name:      "admin listing alone does not allow a blocked email",
			admins:    []string{"boss@corp.com"},
			email:     "boss@corp.com",
			wantAllow: false, // no allowlist/domain gate => fail-closed
			wantAdmin: false,
		},
		{
			name:      "malformed email (no @) cannot match a domain",
			domains:   []string{"corp.com"},
			email:     "not-an-email",
			wantAllow: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newAccessPolicy(tc.emails, tc.domains, tc.admins)
			dec := p.decide(tc.email, tc.hd)
			require.Equal(t, tc.wantAllow, dec.Allow, "Allow")
			require.Equal(t, tc.wantAdmin, dec.Admin, "Admin")
		})
	}
}
