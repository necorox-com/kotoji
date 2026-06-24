package domaincfg

import (
	"errors"
	"net/url"
	"strings"
)

// maxHostnameLen / maxLabelLen bound a DNS hostname (RFC 1035): 253 total, 63 per
// label. Centralized so the validator carries no magic numbers.
const (
	maxHostnameLen = 253
	maxLabelLen    = 63
)

// ErrInvalidBaseDomain / ErrInvalidControlBaseURL are the sentinel validation
// errors the admin handler maps to a 422. They carry a human-safe reason via the
// wrapped error so the handler can surface "why" without leaking internals.
var (
	ErrInvalidBaseDomain     = errors.New("invalid base domain")
	ErrInvalidControlBaseURL = errors.New("invalid control base URL")
)

// ValidateBaseDomain checks that s is a syntactically valid DNS hostname: a dotted
// set of labels, each 1..63 chars of [a-z0-9-] not starting/ending with a hyphen,
// total <= 253, lowercased. It does NOT allow a scheme, port, path, or wildcard.
// An empty string is REJECTED here (the caller treats "" as a delete, not a set).
//
// Returns a wrapped ErrInvalidBaseDomain (with a reason) so the admin handler can
// 422 with a clear field message.
func ValidateBaseDomain(s string) error {
	h := strings.TrimSpace(s)
	if h == "" {
		return reasonErr(ErrInvalidBaseDomain, "must not be empty")
	}
	if h != strings.ToLower(h) {
		return reasonErr(ErrInvalidBaseDomain, "must be lowercase")
	}
	if len(h) > maxHostnameLen {
		return reasonErr(ErrInvalidBaseDomain, "too long (max 253 chars)")
	}
	// A trailing dot (FQDN root) is not accepted; it would break the suffix match.
	if strings.HasSuffix(h, ".") {
		return reasonErr(ErrInvalidBaseDomain, "must not end with a dot")
	}
	if strings.Contains(h, "://") || strings.ContainsAny(h, " /:?#@") {
		return reasonErr(ErrInvalidBaseDomain, "must be a bare hostname (no scheme, port, or path)")
	}
	for _, label := range strings.Split(h, ".") {
		if err := validateLabel(label); err != nil {
			return reasonErr(ErrInvalidBaseDomain, err.Error())
		}
	}
	return nil
}

// validateLabel checks one DNS label: 1..63 chars of [a-z0-9-], no leading or
// trailing hyphen. (The string is already lowercased by the caller.)
func validateLabel(label string) error {
	if label == "" {
		return errors.New("empty label (consecutive dots)")
	}
	if len(label) > maxLabelLen {
		return errors.New("label too long (max 63 chars)")
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return errors.New("label must not start or end with a hyphen")
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isDigit && c != '-' {
			return errors.New("label has invalid characters (allowed: a-z 0-9 -)")
		}
	}
	return nil
}

// ValidateControlBaseURL checks that s is a valid ABSOLUTE http(s) URL with a host
// and no fragment. A trailing slash is tolerated (normalized away by the caller).
// It does NOT require a path. An empty string is REJECTED here (the caller treats
// "" as a delete, not a set).
//
// Returns a wrapped ErrInvalidControlBaseURL (with a reason).
func ValidateControlBaseURL(s string) error {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return reasonErr(ErrInvalidControlBaseURL, "must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return reasonErr(ErrInvalidControlBaseURL, "not a valid URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return reasonErr(ErrInvalidControlBaseURL, "must use the http or https scheme")
	}
	if u.Host == "" || u.Hostname() == "" {
		return reasonErr(ErrInvalidControlBaseURL, "must include a host")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return reasonErr(ErrInvalidControlBaseURL, "must not contain a fragment")
	}
	// Reject a path beyond the root: the control base URL is an origin, callers
	// append paths (e.g. /auth/callback) to it.
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		return reasonErr(ErrInvalidControlBaseURL, "must not include a path")
	}
	return nil
}

// NormalizeControlBaseURL trims trailing slashes from a validated control base
// URL so the OIDC redirect (base + "/auth/callback") never doubles a slash. The
// caller validates first; this only normalizes.
func NormalizeControlBaseURL(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// reasonErr wraps a sentinel with a human-safe reason joined via %w-style errors.
// The admin handler reads errors.Is(err, sentinel) for the status and the reason
// string for the field message.
func reasonErr(sentinel error, reason string) error {
	return &validationError{sentinel: sentinel, reason: reason}
}

// validationError wraps a sentinel + a human reason. Errors.Is matches the
// sentinel; Reason() exposes the message for the API field detail.
type validationError struct {
	sentinel error
	reason   string
}

func (e *validationError) Error() string { return e.sentinel.Error() + ": " + e.reason }
func (e *validationError) Unwrap() error { return e.sentinel }

// Reason returns the human-safe reason for the API validation detail.
func (e *validationError) Reason() string { return e.reason }

// Reason extracts the human reason from a validation error produced by this
// package, or "" if err is not one. The admin handler uses it for the field
// message without depending on the concrete type.
func Reason(err error) string {
	var ve *validationError
	if errors.As(err, &ve) {
		return ve.reason
	}
	return ""
}
