package site

import (
	"fmt"
	"regexp"
	"strings"
)

// Handle / branch grammar constants (CANONICAL §5.1 / §5.2).
const (
	// HandleMinLen is the create-time minimum (friendlier, anti-squat).
	HandleMinLen = 3
	// HandleMaxLen is the DNS label limit; also keeps {handle}--{branch} parseable.
	HandleMaxLen = 63
	// HandleResolverMinLen lets the resolver accept already-created short handles.
	HandleResolverMinLen = 1
)

// handleRe: lowercase, start+end alnum, internal hyphens allowed. The no-"--"
// rule is a SEPARATE post-regex check (the regex permits internal hyphens).
// CANONICAL §5.1.
var handleRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// branchRe: same shape as the handle grammar, lowercased, no "--". CANONICAL §5.2.
var branchRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// doubleHyphen is the reserved preview separator; it must never appear inside a
// handle or branch (load-bearing for the {handle}--{branch} split, CANONICAL §5.3).
const doubleHyphen = "--"

// ReservedHandles is the locked baseline blocklist: the fallback when the
// reserved_handles DB table is empty/unreachable AND the seed source for that
// table (migration 0002_seed_reserved.sql). Admins may ADD to the DB table at
// runtime; they cannot remove these baseline entries. FROZEN by CANONICAL §5.1.
// A test (handle_test.go) asserts this slice equals the migration-0002 rows.
var ReservedHandles = []string{
	"draft", "preview", "published", "www", "api", "internal",
	"host", "admin", "app", "static", "assets", "mcp",
}

// reservedSet is the O(1) membership index over ReservedHandles, built once.
var reservedSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(ReservedHandles))
	for _, h := range ReservedHandles {
		m[h] = struct{}{}
	}
	return m
}()

// IsReservedHandle reports whether h collides with a baseline reserved word.
// Comparison is case-insensitive (handles are lowercased before store; the DB
// uses citext).
func IsReservedHandle(h Handle) bool {
	_, ok := reservedSet[strings.ToLower(string(h))]
	return ok
}

// ValidateHandle is the create-time / rename-time validator (primary gate; the
// DB CHECK is defense in depth). Rules (CANONICAL §5.1): ASCII lowercase grammar,
// length 3..63, no leading/trailing hyphen, no "--" substring, not reserved.
// Uniqueness is checked separately against the Store (the DB UNIQUE is the final
// guard). Returns a *ValidationError (Is ErrValidation) or ErrReservedHandle.
func ValidateHandle(h Handle) error {
	return validateHandleLen(h, HandleMinLen)
}

// ValidateHandleForResolver is the looser resolver-side validator: it accepts
// length 1..63 so already-created short handles still resolve (CANONICAL §5.1).
// It performs only structural validation; existence is checked elsewhere.
func ValidateHandleForResolver(h Handle) error {
	return validateHandleLen(h, HandleResolverMinLen)
}

// validateHandleLen is the shared core parameterized over the minimum length so
// the create gate and the resolver gate share one implementation.
func validateHandleLen(h Handle, minLen int) error {
	s := string(h)
	if s == "" {
		return &ValidationError{Field: "handle", Reason: "must not be empty"}
	}
	// Reject uppercase explicitly so the error is friendly rather than a bare
	// regex miss (handles are case-insensitive but stored lowercase).
	if s != strings.ToLower(s) {
		return &ValidationError{Field: "handle", Reason: "must be lowercase"}
	}
	if len(s) < minLen {
		return &ValidationError{Field: "handle", Reason: fmt.Sprintf("must be at least %d characters", minLen)}
	}
	if len(s) > HandleMaxLen {
		return &ValidationError{Field: "handle", Reason: fmt.Sprintf("must be at most %d characters", HandleMaxLen)}
	}
	// The "--" check precedes the regex match so its message is specific; the
	// regex itself permits internal hyphens including "--".
	if strings.Contains(s, doubleHyphen) {
		return &ValidationError{Field: "handle", Reason: `must not contain "--" (reserved preview separator)`}
	}
	if !handleRe.MatchString(s) {
		return &ValidationError{Field: "handle", Reason: "must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"}
	}
	// Reserved last so a reserved word that is also malformed reports the format
	// problem first (matches the strictest-relevant-rule expectation).
	if IsReservedHandle(h) {
		return ErrReservedHandle
	}
	return nil
}

// validateBranchName enforces the branch grammar for branch *creation* and for
// any branch name that may become a preview host label (CANONICAL §5.2). The two
// canonical logical branches "published"/"draft" are reserved words but are
// valid branch names the service manages itself; this validator is for NEW
// branches only and rejects collisions with the reserved logical names. The
// caller (CreateBranch) is responsible for that policy; this only checks shape.
func validateBranchName(name BranchName) error {
	s := string(name)
	if s == "" {
		return &ValidationError{Field: "branch", Reason: "must not be empty"}
	}
	if s != strings.ToLower(s) {
		return &ValidationError{Field: "branch", Reason: "must be lowercase"}
	}
	if len(s) > HandleMaxLen {
		return &ValidationError{Field: "branch", Reason: fmt.Sprintf("must be at most %d characters", HandleMaxLen)}
	}
	if strings.Contains(s, doubleHyphen) {
		return &ValidationError{Field: "branch", Reason: `must not contain "--"`}
	}
	if !branchRe.MatchString(s) {
		return &ValidationError{Field: "branch", Reason: "must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"}
	}
	return nil
}

// ValidateBranchName is the exported gate over the branch grammar (CANONICAL
// §5.2). It is used by the control plane's preview-grant endpoint to reject a
// non-host-safe branch before signing a grant for a preview URL that could never
// resolve. It is shape-only (the same check CreateBranch applies); it does NOT
// reject the reserved logical names — that policy belongs to the caller.
func ValidateBranchName(name BranchName) error {
	return validateBranchName(name)
}

// previewSubdomain returns the host label fragment for a branch under a handle:
// "{handle}" for published, "{handle}--{branch}" otherwise (Branch.PreviewSubdomain,
// CANONICAL §2).
func previewSubdomain(h Handle, branch BranchName) string {
	if branch == BranchPublished {
		return string(h)
	}
	return string(h) + doubleHyphen + string(branch)
}

// splitLabel implements the CANONICAL §5.3 "--" separator rule against a project
// label L (the part left of the base domain): split on the FIRST "--" into
// handle+branch; absent "--", branch defaults to "published". A malformed
// "a--b--c" yields handle=a, branch="b--c" which then fails branch validation
// upstream (fail-closed).
func splitLabel(label string) (Handle, BranchName) {
	if i := strings.Index(label, doubleHyphen); i >= 0 {
		return Handle(label[:i]), BranchName(label[i+len(doubleHyphen):])
	}
	return Handle(label), BranchPublished
}
