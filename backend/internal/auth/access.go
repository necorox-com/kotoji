package auth

import (
	"errors"
	"strings"
)

// ErrNotAllowed is returned when a verified OIDC identity is not permitted by the
// allowlist/domain gate (or when both lists are empty — fail-closed). The handler
// maps it to a 403 without leaking which gate failed.
var ErrNotAllowed = errors.New("auth: oidc sign-in not allowed")

// ErrEmailNotVerified is returned when the IdP did not assert email_verified=true.
// Decision #4: an unverified email is never run through the allowlist/domain gate.
var ErrEmailNotVerified = errors.New("auth: oidc email not verified")

// accessDecision is the result of the access-control policy for one identity.
type accessDecision struct {
	// Allow reports whether the verified email may sign in (email allowlist OR
	// domain allowlist; fail-closed when both lists are empty).
	Allow bool
	// Admin reports whether the email is in the admin-email allowlist (decision
	// #3). It is only meaningful when Allow is true.
	Admin bool
}

// accessPolicy is the pure, table-testable OIDC access-control decision: a
// verified email is allowed iff it is in the email allowlist OR its domain is in
// the domain allowlist (the domain match also honors the Google `hd` claim). With
// BOTH lists empty every sign-in is DENIED (fail-closed, decision #2). Separately
// it resolves the is_admin promotion from the admin-email allowlist (decision #3).
//
// All inputs are lowercased + trimmed + deduped at construction so decide() is a
// set-membership check with no per-call allocation.
type accessPolicy struct {
	emails  map[string]struct{}
	domains map[string]struct{}
	admins  map[string]struct{}
}

// newAccessPolicy builds the policy from the (already config-normalized, but we
// re-normalize defensively so the provider's unit tests can pass raw lists) email
// allowlist, domain allowlist, and admin-email allowlist.
func newAccessPolicy(allowedEmails, allowedDomains, adminEmails []string) accessPolicy {
	return accessPolicy{
		emails:  toLowerSet(allowedEmails),
		domains: toLowerSet(allowedDomains),
		admins:  toLowerSet(adminEmails),
	}
}

// decide applies the policy to a (verified) email and its `hd` claim. email is
// expected already-lowercased; hd may be "" (no Workspace claim). The caller MUST
// have already checked email_verified — decide does not see that flag.
func (p accessPolicy) decide(email, hd string) accessDecision {
	email = strings.ToLower(strings.TrimSpace(email))
	hd = strings.ToLower(strings.TrimSpace(hd))

	// FAIL-CLOSED: with neither gate configured, deny everything. config.validate
	// already rejects this at boot for oidc, so this is defense in depth (and the
	// invariant the unit tests pin).
	if len(p.emails) == 0 && len(p.domains) == 0 {
		return accessDecision{Allow: false}
	}

	allow := false
	// Email allowlist: case-insensitive exact match.
	if len(p.emails) > 0 {
		if _, ok := p.emails[email]; ok {
			allow = true
		}
	}
	// Domain allowlist: match the verified email's domain part AND, when present,
	// the Google `hd` claim. Either matching the configured set permits sign-in.
	if !allow && len(p.domains) > 0 {
		if _, ok := p.domains[emailDomain(email)]; ok {
			allow = true
		} else if hd != "" {
			if _, ok := p.domains[hd]; ok {
				allow = true
			}
		}
	}
	if !allow {
		return accessDecision{Allow: false}
	}
	// Admin promotion is independent of HOW the user was allowed: it is purely
	// "is this exact email in the admin allowlist".
	_, admin := p.admins[email]
	return accessDecision{Allow: true, Admin: admin}
}

// toLowerSet lowercases + trims every entry into a set, dropping empties/dups.
func toLowerSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, v := range in {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

// emailDomain returns the lowercased domain part of an email, "" if malformed.
func emailDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}
