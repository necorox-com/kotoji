package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/necorox-com/kotoji/backend/internal/site"
)

// scope is a token capability. The chain read ⊂ write ⊂ publish is enforced at
// issuance, so a publish token also reads; the guard checks membership directly.
type scope string

const (
	scopeRead    scope = "read"
	scopeWrite   scope = "write"
	scopePublish scope = "publish"
)

// errorCode is the stable machine string returned in a tool-error body
// (CANONICAL §3 enum, mcp.md §6). Only the MCP-reachable subset is used here.
type errorCode string

const (
	codeUnauthenticated errorCode = "unauthenticated"
	codeForbidden       errorCode = "forbidden"
	codeValidation      errorCode = "validation"
	codeConflict        errorCode = "conflict"
	codeNotFound        errorCode = "not_found"
	codeHandleTaken     errorCode = "handle_taken"
	codePublishConflict errorCode = "publish_conflict"
	codeBranchExists    errorCode = "branch_exists"
	codeNothingToCommit errorCode = "nothing_to_commit"
	codeTooLarge        errorCode = "too_large"
	codeRateLimited     errorCode = "rate_limited"
	codeQuotaExceeded   errorCode = "quota_exceeded"
	codeInternal        errorCode = "internal"
)

// pathLimits mirror the upload-path bounds (mcp.md §4.3): 255 bytes per segment,
// 1024 total. These are structural caps applied BEFORE site.Service is touched.
const (
	maxPathSegmentBytes = 255
	maxPathTotalBytes   = 1024
)

// toolError is the structured business-error body carried inside a tool result's
// StructuredContent (and summarized in text). A Go error is reserved for
// transport/infra failures (mcp.md §6); business errors are IsError results so the
// model gets the detail (e.g. current_sha on conflict) to self-correct.
type toolError struct {
	Error toolErrorBody `json:"error"`
}

type toolErrorBody struct {
	Code      errorCode      `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	Retryable bool           `json:"retryable"`
}

// registry owns the dependencies every tool needs and the guard that decorates
// each handler with principal-recovery + a per-token rate gate before delegating.
// Per-site MEMBERSHIP-CAPPED authorization (intersection of token scopes and the
// user's membership role) is applied inside each handler via authorizeSite, since
// the target site now comes from the tool's `site` argument (authz.go).
type registry struct {
	svc     site.Service
	members membershipQuerier
	limits  Limits
	log     *slog.Logger
	cfg     Deps // carries published-URL composition inputs (base domain)
}

// toolFnFor is the typed handler signature a tool implements AFTER the guard has
// injected claims into ctx and verified scope. It mirrors the SDK's
// ToolHandlerFor but with the guard guarantees already established.
type toolFnFor[In, Out any] func(ctx context.Context, claims TokenInfo, in In) (*mcp.CallToolResult, Out, error)

// addTool wraps fn with the per-token guard (principal recovery + rate gate) and
// registers it via the SDK. The In type's jsonschema tags become the tool's input
// schema; Out becomes the structured output schema (auto-inferred).
//
// sc is the tool's DECLARED scope. It is no longer enforced as a GLOBAL gate here
// (a token's scopes are now capped per-site by membership): the actual scope check
// is intersection(token.scopes, membership-role scopes) inside each handler via
// authorizeSite. sc is retained for the rate class wiring and the test catalogue.
func addTool[In, Out any](
	s *mcp.Server, r *registry, name, desc string, sc scope, class toolClass,
	fn toolFnFor[In, Out],
) {
	_ = sc // declared scope; enforced per-site in the handler (membership-capped)
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, guard(r, class, fn))
}

// guard decorates a typed tool fn with the two TOKEN-level gates that do not need
// the target site: principal recovery + the per-token rate gate. The per-SITE
// authorization (resolve handle -> membership role -> effective scope) happens
// inside the handler via authorizeSite, because the site now comes from the tool's
// `site` argument (membership-capped model REPLACES the old site pin).
//
// It returns an SDK ToolHandlerFor so it can be registered OR invoked directly in
// unit tests with a synthetic request carrying Extra.TokenInfo. It is a free
// function (not a method) because Go methods cannot have their own type
// parameters; the registry is threaded as the first argument.
func guard[In, Out any](r *registry, class toolClass, fn toolFnFor[In, Out]) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out

		// 1. Recover the verified principal (set by RequireBearerToken). Behind
		// the bearer middleware this is always present; fail closed otherwise.
		claims, ok := principalFrom(req)
		if !ok {
			return toolErr(codeUnauthenticated, "invalid or missing token", nil), zero, nil
		}

		// 2. Per-token rate gate (mcp.md §10.3). Denied → rate_limited with a
		// retry hint; never a Go error.
		if okAllow, retryAfter := r.limits.Limiter.Allow(claims.TokenID, class); !okAllow {
			return toolErr(codeRateLimited, "rate limit exceeded; back off and retry", map[string]any{
				"retry_after": int(retryAfter.Seconds() + 0.999), // ceil to whole seconds
			}), zero, nil
		}

		// 3. Stash the principal in ctx so handlers/audit can read it; per-site
		// authorization is applied inside the handler (authorizeSite).
		ctx = withClaims(ctx, claims)
		return fn(ctx, claims, in)
	}
}

// principalFrom recovers the typed claims from the SDK request's Extra.TokenInfo.
func principalFrom(req *mcp.CallToolRequest) (TokenInfo, bool) {
	if req == nil {
		return TokenInfo{}, false
	}
	extra := req.GetExtra()
	if extra == nil {
		return TokenInfo{}, false
	}
	return claimsFromTokenInfo(extra.TokenInfo)
}

// hasScope reports whether the token's scope set grants required. Because the
// chain read ⊂ write ⊂ publish is enforced at issuance, exact membership is the
// correct check (a publish token literally carries read and write too).
func hasScope(have []string, required scope) bool {
	return slices.Contains(have, string(required))
}

// claimsCtxKey types the context value so it can't collide.
type claimsCtxKey struct{}

// withClaims stashes the principal in ctx so handlers/audit can read it without
// re-threading. The target site comes from each tool's `site` argument and is
// authorized per call (authz.go), not pinned on the token.
func withClaims(ctx context.Context, c TokenInfo) context.Context {
	return context.WithValue(ctx, claimsCtxKey{}, c)
}

// ---- tool-error envelope helpers ----

// toolErr builds an IsError CallToolResult whose StructuredContent is the
// canonical error body and whose text content is a compact human summary, so both
// code-driven and chat-driven clients can act on it.
func toolErr(code errorCode, msg string, details map[string]any) *mcp.CallToolResult {
	body := toolError{Error: toolErrorBody{
		Code:      code,
		Message:   msg,
		Details:   details,
		Retryable: retryableFor(code),
	}}
	return &mcp.CallToolResult{
		IsError:           true,
		StructuredContent: body,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(code) + ": " + msg}},
	}
}

// retryableFor flags the codes a client should retry: conflict (re-read+retry)
// and rate_limited (backoff). Others are not auto-retryable.
func retryableFor(code errorCode) bool {
	switch code {
	case codeConflict, codeRateLimited:
		return true
	default:
		return false
	}
}

// mapError translates a site.Service error into a tool result. Business errors
// become IsError results with a structured body (the model can self-correct);
// infrastructure failures (ErrGit/unknown) are returned as a Go error so the SDK
// surfaces a JSON-RPC protocol error and details stay in the server log, never
// leaked to the model (mcp.md §6, safety guarantee #12).
//
// It returns (result, goErr): exactly one is non-nil. The caller threads goErr as
// the handler's error return.
func (r *registry) mapError(err error, action string) (*mcp.CallToolResult, error) {
	switch {
	case err == nil:
		return nil, nil

	case errors.Is(err, site.ErrConflict):
		// Optimistic-lock conflict: surface current tip + changed paths so the
		// client can re-read and retry (mcp.md §5.4).
		details := map[string]any{}
		var ce *site.ConflictError
		if errors.As(err, &ce) {
			details["expected"] = ce.Expected
			details["actual"] = ce.Actual
			details["current_sha"] = ce.Actual // mcp.md §5.4 alias
			details["changed_paths"] = nonNilStrings(ce.ChangedPaths)
		}
		return toolErr(codeConflict, "branch moved since base_sha; re-read and retry", details), nil

	case errors.Is(err, site.ErrPublishConflict):
		details := map[string]any{}
		var pe *site.PublishConflictError
		if errors.As(err, &pe) {
			details["paths"] = nonNilStrings(pe.Paths)
		}
		return toolErr(codePublishConflict, "publish merge conflict", details), nil

	case errors.Is(err, site.ErrNotFound):
		return toolErr(codeNotFound, "not found", nil), nil

	case errors.Is(err, site.ErrHandleTaken):
		return toolErr(codeHandleTaken, "handle already taken", nil), nil

	case errors.Is(err, site.ErrBranchExists):
		return toolErr(codeBranchExists, "branch already exists", nil), nil

	case errors.Is(err, site.ErrNothingToCommit):
		return toolErr(codeNothingToCommit, "nothing to commit", nil), nil

	case errors.Is(err, site.ErrForbidden):
		return toolErr(codeForbidden, "forbidden", nil), nil

	case errors.Is(err, site.ErrReservedHandle):
		return toolErr(codeValidation, "handle is reserved", nil), nil

	case errors.Is(err, site.ErrValidation):
		// Surface the friendly field/reason if present, never internal detail.
		var ve *site.ValidationError
		if errors.As(err, &ve) {
			return toolErr(codeValidation, ve.Field+": "+ve.Reason, nil), nil
		}
		return toolErr(codeValidation, "validation failed", nil), nil

	case errors.Is(err, site.ErrZipTooLarge), errors.Is(err, site.ErrZipTooManyFiles):
		return toolErr(codeTooLarge, "upload too large", nil), nil

	case errors.Is(err, site.ErrQuotaExceeded):
		return toolErr(codeQuotaExceeded, "per-site disk quota exceeded", nil), nil

	default:
		// ErrGit / disk / unknown: protocol error, generic message, logged with
		// the action for triage. The detail never reaches the model.
		if r.log != nil {
			r.log.Error("mcp tool internal error", "action", action, "err", err)
		}
		return nil, fmt.Errorf("internal error during %s", action)
	}
}

// nonNilStrings normalizes a possibly-nil slice to a non-nil one so it serializes
// as [] rather than null (stable wire shape for clients).
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---- in-site path & extension confinement (mcp.md §4.3) ----

// validateContentPath validates an untrusted file path for read/write WITHIN the
// pinned site, BEFORE site.Service. It rejects absolute paths, ".." segments,
// the ".git/" prefix, NUL bytes and backslashes; normalizes to a clean relative
// POSIX path; and (when forWrite) enforces the MCP text-first extension allowlist.
// site.Service re-validates (defense in depth) — this is the friendly gate.
func validateContentPath(p string, forWrite bool) (string, *mcp.CallToolResult) {
	if p == "" {
		return "", toolErr(codeValidation, "path: must not be empty", nil)
	}
	if strings.ContainsRune(p, 0x00) {
		return "", toolErr(codeValidation, "path: must not contain NUL bytes", nil)
	}
	if strings.Contains(p, `\`) {
		return "", toolErr(codeValidation, "path: must use forward slashes", nil)
	}
	if strings.HasPrefix(p, "/") {
		return "", toolErr(codeValidation, "path: must be repo-relative (no leading slash)", nil)
	}
	if len(p) > maxPathTotalBytes {
		return "", toolErr(codeValidation, "path: too long", nil)
	}
	// filepath.IsLocal rejects absolute paths and any escape via "..".
	if !filepath.IsLocal(p) {
		return "", toolErr(codeValidation, "path: must not escape the repo root", nil)
	}
	clean := path.Clean(p)
	if clean == "." || clean == "" {
		return "", toolErr(codeValidation, "path: must reference a file, not the root", nil)
	}
	for _, seg := range strings.Split(clean, "/") {
		if len(seg) > maxPathSegmentBytes {
			return "", toolErr(codeValidation, "path: segment too long", nil)
		}
		switch seg {
		case "..":
			return "", toolErr(codeValidation, `path: must not contain ".." segments`, nil)
		case gitDir:
			return "", toolErr(codeValidation, `path: must not reference the ".git" directory`, nil)
		}
	}
	if forWrite && !extAllowedForMCPWrite(clean) {
		return "", toolErr(codeValidation, "path: file extension is not allowed for MCP write (text/static only)", nil)
	}
	return clean, nil
}

// gitDir is the repo metadata dir that must never be reachable via a content tool.
const gitDir = ".git"

// extAllowedForMCPWrite reports whether the path's extension is in the served MIME
// allowlist AND not a large-media type (mcp.md §4.3 / CANONICAL §5.6). It reuses
// site.MIMEByExt (the single source of truth) and excludes the same media set the
// site package excludes for MCP writes.
func extAllowedForMCPWrite(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	if _, ok := site.MIMEByExt[ext]; !ok {
		return false
	}
	_, denied := mcpWriteDeniedExt[ext]
	return !denied
}

// mcpWriteDeniedExt is the large-media exclusion set for MCP writes, kept in sync
// with internal/site (CANONICAL §5.6 / consistency-report #P1-7). MCP is
// text-first; binaries are upload-only.
var mcpWriteDeniedExt = map[string]struct{}{
	".mp4":  {},
	".webm": {},
	".mp3":  {},
	".wav":  {},
	".pdf":  {},
}
