package site

import (
	"errors"
	"strings"
)

// Sentinels — the stable, switchable categories callers branch on with
// errors.Is. Every returned error wraps one of these so the HTTP/MCP layer maps
// to a status/code with a single errors.Is switch (statusFor below). FROZEN by
// CANONICAL §3.
var (
	ErrNotFound        = errors.New("site: not found")                // 404
	ErrValidation      = errors.New("site: validation failed")        // 422 (400 for malformed)
	ErrReservedHandle  = errors.New("site: reserved handle")          // 422
	ErrHandleTaken     = errors.New("site: handle already taken")     // 409
	ErrConflict        = errors.New("site: stale base sha")           // 409 (optimistic lock)
	ErrPublishConflict = errors.New("site: publish merge conflict")   // 409
	ErrBranchExists    = errors.New("site: branch already exists")    // 409
	ErrNothingToCommit = errors.New("site: nothing to commit")        // 409
	ErrForbidden       = errors.New("site: forbidden")                // 403
	// Zip family:
	ErrZipSlip         = errors.New("site: zip path traversal")       // 400
	ErrZipTooLarge     = errors.New("site: zip too large")            // 413
	ErrZipTooManyFiles = errors.New("site: zip too many files")       // 413
	ErrZipBadType      = errors.New("site: zip disallowed file type") // 415
	// git wrapper:
	ErrGit = errors.New("site: git operation failed") // 500
)

// ValidationError adds the offending field/reason. Is() ties it to ErrValidation
// so callers can branch on the category or pull the field via errors.As.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return "site: validation: " + e.Field + ": " + e.Reason
}
func (e *ValidationError) Is(t error) bool { return t == ErrValidation }

// ConflictError — the optimistic-lock conflict. errors.Is(err, ErrConflict) is
// true; errors.As gets the SHAs + changed paths. WIRE names: expected, actual,
// changedPaths (CANONICAL §8) — used identically by REST and MCP.
type ConflictError struct {
	Branch       BranchName
	Expected     string   // the BaseSHA the caller sent
	Actual       string   // the real current tip
	ChangedPaths []string // files that differ between Expected and Actual
}

func (e *ConflictError) Error() string {
	return "site: stale base sha on " + string(e.Branch) + ": expected " + e.Expected + " got " + e.Actual
}
func (e *ConflictError) Is(t error) bool { return t == ErrConflict }

// PublishConflictError carries the conflicting paths from a publish merge.
type PublishConflictError struct {
	Paths []string
}

func (e *PublishConflictError) Error() string { return "site: publish merge conflict" }
func (e *PublishConflictError) Is(t error) bool { return t == ErrPublishConflict }

// GitError is the raw os/exec failure. errors.Is(err, ErrGit) is true;
// errors.As gets the argv + exit code + stderr for diagnostics.
type GitError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *GitError) Error() string {
	return "site: git " + strings.Join(e.Args, " ") + ": " + e.Stderr
}
func (e *GitError) Is(t error) bool { return t == ErrGit }

// statusFor is the SINGLE place errors become HTTP statuses (CANONICAL §3).
// Context cancellation is surfaced as context.Canceled and handled by the
// request layer (mapped to 499/timeout), not here. It is unexported because the
// API layer (Phase 4) owns the wire mapping; it is exercised by tests so the
// taxonomy stays correct.
func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrForbidden):
		return 403
	case errors.Is(err, ErrConflict),
		errors.Is(err, ErrHandleTaken),
		errors.Is(err, ErrPublishConflict),
		errors.Is(err, ErrBranchExists),
		errors.Is(err, ErrNothingToCommit):
		return 409
	case errors.Is(err, ErrZipTooLarge),
		errors.Is(err, ErrZipTooManyFiles):
		return 413
	case errors.Is(err, ErrZipBadType):
		return 415
	case errors.Is(err, ErrZipSlip):
		return 400
	case errors.Is(err, ErrValidation),
		errors.Is(err, ErrReservedHandle):
		return 422
	default:
		return 500 // ErrGit + unknown
	}
}
