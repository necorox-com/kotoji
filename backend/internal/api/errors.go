package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/necorox-com/kotoji/backend/internal/site"
)

// Stable machine error codes (CANONICAL §3 / openapi ErrorEnvelope.code enum).
// Kept as constants so handlers never hardcode wire strings. The set mirrors the
// auth package's codes so the whole /api surface is drift-free.
const (
	codeUnauthenticated      = "unauthenticated"
	codeForbidden            = "forbidden"
	codeValidation           = "validation"
	codeConflict             = "conflict"
	codeNotFound             = "not_found"
	codeHandleTaken          = "handle_taken"
	codePublishConflict      = "publish_conflict"
	codeBranchExists         = "branch_exists"
	codeNothingToCommit      = "nothing_to_commit"
	codeTooLarge             = "too_large"
	codeUnsupportedMediaType = "unsupported_media_type"
	codeRateLimited          = "rate_limited"
	codeQuotaExceeded        = "quota_exceeded"
	codeInternal             = "internal"
)

// errorEnvelope is the uniform wire error body (CANONICAL §3 / openapi
// ErrorEnvelope + ConflictEnvelope + PublishConflictEnvelope). details is
// code-specific structured context (conflict SHAs, validation field, ...).
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// conflictDetails is the frozen optimistic-lock conflict detail (CANONICAL §8:
// expected/actual/changedPaths; plus branch for the ConflictEnvelope schema).
type conflictDetails struct {
	Branch       string   `json:"branch"`
	Expected     string   `json:"expected"`
	Actual       string   `json:"actual"`
	ChangedPaths []string `json:"changedPaths,omitempty"`
}

// publishConflictDetails carries the conflicting paths from a publish merge
// (openapi PublishConflictEnvelope.error.details).
type publishConflictDetails struct {
	Paths []string `json:"paths"`
}

// validationDetails carries the offending field/reason for a ValidationError
// (openapi ErrorEnvelope.details — free-form).
type validationDetails struct {
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// WriteRateLimited writes the canonical 429 JSON error envelope (code
// rate_limited, CANONICAL §3). It is exported so the composition root can hand it
// to the rate-limit middleware as the control-plane "denied" responder, keeping
// the wire error shape consistent with every other /api error.
func WriteRateLimited(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusTooManyRequests, codeRateLimited, safeMessageFor(codeRateLimited), nil)
}

// writeError emits a JSON error envelope with the given status + machine code.
// The message is human-safe (never leaks internals). details may be nil.
func writeError(w http.ResponseWriter, status int, code, message string, details any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// A fixed-shape struct cannot realistically fail to encode; the status line
	// is already committed so any error is unrecoverable and ignored.
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: code, Message: message, Details: details}})
}

// writeServiceError is the SINGLE place a site.Service (or Store) error becomes
// an HTTP response. It maps the frozen taxonomy (CANONICAL §3) to status+code+
// structured details using errors.As for the typed members, so the wire body
// matches the openapi envelopes exactly. The message is the safe human string.
func writeServiceError(w http.ResponseWriter, err error) {
	status, code := statusAndCode(err)

	// Structured details for the typed errors the frontend branches on.
	var conflict *site.ConflictError
	if errors.As(err, &conflict) {
		writeError(w, status, code, "the file or branch changed since you last read it", conflictDetails{
			Branch:       string(conflict.Branch),
			Expected:     conflict.Expected,
			Actual:       conflict.Actual,
			ChangedPaths: conflict.ChangedPaths,
		})
		return
	}
	var pub *site.PublishConflictError
	if errors.As(err, &pub) {
		writeError(w, status, code, "publish could not merge cleanly", publishConflictDetails{Paths: pub.Paths})
		return
	}
	var ve *site.ValidationError
	if errors.As(err, &ve) {
		writeError(w, status, code, "request failed validation", validationDetails{Field: ve.Field, Reason: ve.Reason})
		return
	}

	writeError(w, status, code, safeMessageFor(code), nil)
}

// statusAndCode maps a site error to its HTTP status and wire code. It is the
// API-layer mirror of site.statusFor (which is unexported); both follow
// CANONICAL §3 so they cannot drift (asserted by tests).
func statusAndCode(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, site.ErrNotFound):
		return http.StatusNotFound, codeNotFound
	case errors.Is(err, site.ErrForbidden):
		return http.StatusForbidden, codeForbidden
	case errors.Is(err, site.ErrHandleTaken):
		return http.StatusConflict, codeHandleTaken
	case errors.Is(err, site.ErrPublishConflict):
		return http.StatusConflict, codePublishConflict
	case errors.Is(err, site.ErrBranchExists):
		return http.StatusConflict, codeBranchExists
	case errors.Is(err, site.ErrNothingToCommit):
		return http.StatusConflict, codeNothingToCommit
	case errors.Is(err, site.ErrConflict):
		return http.StatusConflict, codeConflict
	case errors.Is(err, site.ErrZipTooLarge), errors.Is(err, site.ErrZipTooManyFiles):
		return http.StatusRequestEntityTooLarge, codeTooLarge
	case errors.Is(err, site.ErrQuotaExceeded):
		// quota_exceeded → 413 (CANONICAL §3); distinct wire code from too_large so
		// the client can tell "this single upload is too big" from "you are out of
		// per-site disk space".
		return http.StatusRequestEntityTooLarge, codeQuotaExceeded
	case errors.Is(err, site.ErrZipBadType):
		return http.StatusUnsupportedMediaType, codeUnsupportedMediaType
	case errors.Is(err, site.ErrZipSlip):
		return http.StatusBadRequest, codeValidation
	case errors.Is(err, site.ErrValidation), errors.Is(err, site.ErrReservedHandle):
		return http.StatusUnprocessableEntity, codeValidation
	default:
		// ErrGit + any unknown error => internal, never leaking the cause.
		return http.StatusInternalServerError, codeInternal
	}
}

// safeMessageFor returns a human-readable, non-leaky message per machine code.
func safeMessageFor(code string) string {
	switch code {
	case codeNotFound:
		return "resource not found"
	case codeForbidden:
		return "you do not have permission to do that"
	case codeUnauthenticated:
		return "authentication required"
	case codeHandleTaken:
		return "that handle is already taken"
	case codePublishConflict:
		return "publish could not merge cleanly"
	case codeBranchExists:
		return "that branch already exists"
	case codeNothingToCommit:
		return "there is nothing to commit"
	case codeConflict:
		return "the resource changed since you last read it"
	case codeTooLarge:
		return "the upload is too large"
	case codeUnsupportedMediaType:
		return "that file type is not allowed"
	case codeValidation:
		return "request failed validation"
	case codeRateLimited:
		return "too many requests"
	case codeQuotaExceeded:
		return "quota exceeded"
	default:
		return "something went wrong"
	}
}
