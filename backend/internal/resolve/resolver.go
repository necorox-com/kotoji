// Package resolve maps an incoming HTTP request to a routing Target on the
// kotoji data plane. It is the single, swappable abstraction the spec requires:
// Host-based and path-based resolution both live behind the Resolver interface so
// the reverse proxy / URL scheme can change without touching the static handler.
//
// This package is intentionally DB-free and pure: it performs only STRUCTURAL
// validation (grammar + reserved words + the "--" separator rule). Existence
// (does this handle map to a real site?) is checked later by the serve layer
// against site.Service, so the resolver stays unit-testable without a DB.
//
// routing-and-serving.md §1-§4 is the law for this file; it honors CANONICAL §5
// (handle/branch regex, the "--" separator, {handle}--published -> 400, the
// reserved-word list, and the host-label length budget).
package resolve

import (
	"net/http"
	"regexp"
	"strings"
)

// Target is the fully-resolved routing decision for one request. Produced by
// Resolver.Resolve; consumed by the static handler. A Target is never partially
// valid: Resolve returns a *ResolveError on any structural problem instead.
type Target struct {
	Handle     string // validated handle, lowercased. e.g. "expense-calc"
	Branch     string // git branch to serve. "published" for the bare handle.
	IsPreview  bool   // true if a branch was explicitly requested (-- suffix / path branch)
	IsControl  bool   // true if this is the control-plane host (not a project)
	Source     Source // how the target was derived (host vs path); for logging/metrics
	PathPrefix string // path-mode only: the URL prefix to strip ("/host/{handle}[--{branch}]")
}

// Source records how a Target was derived, for logging/metrics.
type Source uint8

const (
	// SourceUnknown is the zero value (never returned for a successful resolve).
	SourceUnknown Source = iota
	// SourceHost means the target was resolved from the Host header (subdomain).
	SourceHost
	// SourcePath means the target was resolved from /host/{handle}[--{branch}]/...
	SourcePath
)

// String renders the Source for logs.
func (s Source) String() string {
	switch s {
	case SourceHost:
		return "host"
	case SourcePath:
		return "path"
	default:
		return "unknown"
	}
}

// Resolver maps an *http.Request to a Target. Host-based and path-based
// resolution live behind it so the proxy / URL scheme can change without
// touching the static handler.
type Resolver interface {
	// Resolve never returns a partially-valid Target. On any structural problem
	// it returns a *ResolveError carrying an HTTP status to emit.
	Resolve(r *http.Request) (Target, error)
}

// ResolveError carries the HTTP status to emit for a structural resolution
// failure (routing-and-serving.md §1).
type ResolveError struct {
	Status int    // 400 (malformed) | 404 (not a known host shape) | 421 (misdirected)
	Code   string // stable machine code, e.g. "bad_handle", "bad_branch", "not_project_host"
	Msg    string
}

func (e *ResolveError) Error() string { return e.Code + ": " + e.Msg }

// Stable machine codes for ResolveError.Code.
const (
	CodeBadHandle       = "bad_handle"
	CodeBadBranch       = "bad_branch"
	CodeNotProjectHost  = "not_project_host"
	CodeMisdirected     = "misdirected_request"
	CodeLabelTooLong    = "label_too_long"
	CodePathFallbackOff = "path_fallback_disabled"
	CodeEmptyHost       = "empty_host"
)

// ---- Structural validation surfaces (no DB) ----

// HandleValidator does structural-only handle validation. site.ValidateHandleForResolver
// satisfies it via the adapter in NewResolver's default; a fake satisfies it in tests.
type HandleValidator interface {
	// ValidateHandle returns nil if h is a structurally valid handle for the
	// resolver (length 1..63, grammar, no "--", not reserved). It does NOT check
	// existence.
	ValidateHandle(h string) error
}

// BranchValidator does structural-only branch validation.
type BranchValidator interface {
	// ValidateBranch returns nil if b is a structurally valid branch name.
	ValidateBranch(b string) error
}

// Config configures the DefaultResolver. routing-and-serving.md §1.
type Config struct {
	// BaseDomain is "hosting.example.com" | "localhost". REQUIRED. Lowercased on
	// construction. Everything to the left of it is the project/control label.
	BaseDomain string
	// ControlLabel is the label that means "control plane" when present as a
	// subdomain of BaseDomain. Default "" => bare BaseDomain is control.
	ControlLabel string
	// EnablePathFallback accepts /host/{handle}/... in addition to Host. Default
	// true (set explicitly via NewResolver options or the field).
	EnablePathFallback bool
	// TrustForwardedHost reads X-Forwarded-Host instead of Host. MUST be false if
	// the data plane is ever exposed directly to the internet (X-Forwarded-Host is
	// then attacker-controlled). Default true because the documented topology has a
	// reverse proxy in front.
	TrustForwardedHost bool
}

// builtinValidator implements HandleValidator + BranchValidator with the exact
// grammar from CANONICAL §5 so the resolver has zero DB/site dependency by
// default. Callers MAY inject site.ValidateHandleForResolver-backed validators
// instead via NewResolverWith; both share identical grammar.
type builtinValidator struct{}

// labelRe is the shared grammar for handles and (host-safe) branches: lowercase,
// start+end alphanumeric, internal hyphens allowed. The no-"--" rule is a
// separate post-regex check (CANONICAL §5.1/§5.2).
var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// reservedHandles mirrors site.ReservedHandles / CANONICAL §5.1. Kept local so
// the resolver does not need to import site for its pure path. A test asserts the
// two lists stay in sync.
var reservedHandles = map[string]struct{}{
	"draft": {}, "preview": {}, "published": {}, "www": {}, "api": {},
	"internal": {}, "host": {}, "admin": {}, "app": {}, "static": {},
	"assets": {}, "mcp": {},
}

const (
	handleMinLen = 1  // resolver accepts 1..63 so short already-created handles resolve
	labelMaxLen  = 63 // single DNS-label limit (also the {handle}--{branch} budget)
)

func (builtinValidator) ValidateHandle(h string) error {
	if h == "" {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadHandle, Msg: "empty handle"}
	}
	if h != strings.ToLower(h) {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadHandle, Msg: "handle must be lowercase"}
	}
	if len(h) < handleMinLen || len(h) > labelMaxLen {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadHandle, Msg: "handle length out of range"}
	}
	if strings.Contains(h, "--") {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadHandle, Msg: `handle must not contain "--"`}
	}
	if !labelRe.MatchString(h) {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadHandle, Msg: "handle has invalid characters"}
	}
	if _, ok := reservedHandles[h]; ok {
		// A reserved word is not a valid handle. Per the §11 table (draft.host ->
		// 404), reserved labels resolve to "not a project host", not a 400.
		return &ResolveError{Status: http.StatusNotFound, Code: CodeNotProjectHost, Msg: "reserved handle"}
	}
	return nil
}

func (builtinValidator) ValidateBranch(b string) error {
	if b == "" {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadBranch, Msg: "empty branch"}
	}
	if b != strings.ToLower(b) {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadBranch, Msg: "branch must be lowercase"}
	}
	if len(b) > labelMaxLen {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadBranch, Msg: "branch too long"}
	}
	if strings.Contains(b, "--") {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadBranch, Msg: `branch must not contain "--"`}
	}
	if !labelRe.MatchString(b) {
		return &ResolveError{Status: http.StatusBadRequest, Code: CodeBadBranch, Msg: "branch has invalid characters"}
	}
	return nil
}

// DefaultResolver is the concrete Resolver constructed from Config.
type DefaultResolver struct {
	cfg      Config
	handles  HandleValidator
	branches BranchValidator
	// baseSuffix is "." + BaseDomain, precomputed for the suffix check.
	baseSuffix string
}

var _ Resolver = (*DefaultResolver)(nil)

// NewResolver constructs a DefaultResolver with the built-in CANONICAL-grammar
// validators. BaseDomain is lowercased. EnablePathFallback / TrustForwardedHost
// are taken from cfg verbatim (callers should set them explicitly; the
// composition root applies the documented defaults).
func NewResolver(cfg Config) *DefaultResolver {
	return NewResolverWith(cfg, builtinValidator{}, builtinValidator{})
}

// NewResolverWith constructs a DefaultResolver with injected validators (e.g.
// site-package-backed). Useful for keeping a single source of truth for the
// grammar across packages.
func NewResolverWith(cfg Config, handles HandleValidator, branches BranchValidator) *DefaultResolver {
	cfg.BaseDomain = strings.ToLower(strings.TrimSpace(cfg.BaseDomain))
	cfg.ControlLabel = strings.ToLower(strings.TrimSpace(cfg.ControlLabel))
	if handles == nil {
		handles = builtinValidator{}
	}
	if branches == nil {
		branches = builtinValidator{}
	}
	return &DefaultResolver{
		cfg:        cfg,
		handles:    handles,
		branches:   branches,
		baseSuffix: "." + cfg.BaseDomain,
	}
}

// Resolve implements Resolver. It tries Host-mode classification first; if the
// path is under /host/ AND path fallback is enabled, path mode wins (the editor
// live-preview iframe relies on path mode under the control origin). The
// precedence is: a /host/ path on the control host resolves as a project via
// path mode; any other path on the control host is IsControl.
func (d *DefaultResolver) Resolve(r *http.Request) (Target, error) {
	host := lowerStripPort(d.effectiveHost(r))
	if host == "" {
		return Target{}, &ResolveError{Status: http.StatusBadRequest, Code: CodeEmptyHost, Msg: "no host on request"}
	}

	// Classify the host: control, project, or foreign.
	class, label, herr := d.classifyHost(host)
	if herr != nil {
		return Target{}, herr
	}

	// Use the ESCAPED path so a percent-encoded branch (e.g. feature%2Fx, the
	// non-host-safe escape hatch) survives intact for path-mode parsing; the
	// label is decoded by resolvePath after the first "/" boundary is found.
	escPath := requestEscapedPath(r)

	switch class {
	case hostControl:
		// On the control host, a /host/{label}/... path is a project served via
		// path mode (path fallback). Everything else is the control plane.
		if d.cfg.EnablePathFallback && isHostPath(escPath) {
			return d.resolvePath(escPath)
		}
		return Target{IsControl: true, Source: SourceHost}, nil
	case hostProject:
		return d.resolveProjectLabel(label, SourceHost, "")
	default:
		// Foreign host. Path fallback is still allowed (proxy-independent reach):
		// if the request is a /host/ path, honor it; otherwise it is misdirected.
		if d.cfg.EnablePathFallback && isHostPath(escPath) {
			return d.resolvePath(escPath)
		}
		return Target{}, &ResolveError{
			Status: http.StatusMisdirectedRequest,
			Code:   CodeMisdirected,
			Msg:    "host not under base domain",
		}
	}
}

// requestEscapedPath returns the URL-escaped request path, preferring URL.RawPath
// (set only when the escaped form differs from the decoded form, e.g. it contains
// %2F). When RawPath is empty the escaped form equals URL.Path, so we use it
// directly. url.URL.EscapedPath() implements exactly this preference.
func requestEscapedPath(r *http.Request) string {
	if r.URL == nil {
		return ""
	}
	return r.URL.EscapedPath()
}

type hostClass uint8

const (
	hostForeign hostClass = iota
	hostControl
	hostProject
)

// classifyHost implements routing-and-serving.md §3: distinguish control-plane
// from project hosts. host is already lowercased + port-stripped.
func (d *DefaultResolver) classifyHost(host string) (hostClass, string, error) {
	if host == d.cfg.BaseDomain {
		if d.cfg.ControlLabel == "" {
			return hostControl, "", nil
		}
		// A control label is required (e.g. app.base); bare base is then a
		// misconfig the proxy should have redirected.
		return hostForeign, "", &ResolveError{
			Status: http.StatusNotFound,
			Code:   CodeNotProjectHost,
			Msg:    "bare base domain is not a project (control label required)",
		}
	}
	if !strings.HasSuffix(host, d.baseSuffix) {
		return hostForeign, "", nil // caller decides 421 vs path fallback
	}
	label := host[:len(host)-len(d.baseSuffix)]
	if strings.Contains(label, ".") {
		// Multi-label (a.b.base) is never a project; we don't serve nested subdomains.
		return hostForeign, "", &ResolveError{
			Status: http.StatusNotFound,
			Code:   CodeNotProjectHost,
			Msg:    "nested subdomain is not a project host",
		}
	}
	if d.cfg.ControlLabel != "" && label == d.cfg.ControlLabel {
		return hostControl, "", nil
	}
	return hostProject, label, nil
}

// resolveProjectLabel splits {handle}[--{branch}] from a label and validates it.
// pathPrefix is "" for Host mode and "/host/{label}" for path mode.
func (d *DefaultResolver) resolveProjectLabel(label string, src Source, pathPrefix string) (Target, error) {
	handle, branch, isPreview := splitLabel(label)

	if err := d.handles.ValidateHandle(handle); err != nil {
		return Target{}, asResolveError(err, CodeBadHandle)
	}

	if isPreview {
		// {handle}--published is not addressable via "--": published is reached via
		// the bare handle host only (CANONICAL §5.2). Fail with 400 bad_branch.
		if branch == "published" {
			return Target{}, &ResolveError{
				Status: http.StatusBadRequest,
				Code:   CodeBadBranch,
				Msg:    "published is not addressable via --",
			}
		}
		if err := d.branches.ValidateBranch(branch); err != nil {
			return Target{}, asResolveError(err, CodeBadBranch)
		}
		// Host-label budget: {handle}--{branch} must fit one DNS label (<= 63).
		// (For Host mode this is implied by the 63-char host label; for path mode
		// we enforce it explicitly for parity and for non-host-safe escape hatches.)
		if len(handle)+len("--")+len(branch) > labelMaxLen {
			return Target{}, &ResolveError{
				Status: http.StatusBadRequest,
				Code:   CodeLabelTooLong,
				Msg:    "handle--branch exceeds 63-char DNS label budget",
			}
		}
	}

	return Target{
		Handle:     handle,
		Branch:     branch,
		IsPreview:  isPreview,
		IsControl:  false,
		Source:     src,
		PathPrefix: pathPrefix,
	}, nil
}

// resolvePath handles the /host/{label}/{filepath...} fallback grammar
// (routing-and-serving.md §4).
func (d *DefaultResolver) resolvePath(urlPath string) (Target, error) {
	if !d.cfg.EnablePathFallback {
		return Target{}, &ResolveError{Status: http.StatusNotFound, Code: CodePathFallbackOff, Msg: "path fallback disabled"}
	}
	// Strip the leading "/host/" and take the first segment as the label.
	rest := strings.TrimPrefix(urlPath, "/host/")
	// urlPath == "/host" or "/host/" => no label.
	if rest == "" || rest == urlPath {
		return Target{}, &ResolveError{Status: http.StatusNotFound, Code: CodeNotProjectHost, Msg: "no project label in path"}
	}
	rawLabel := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rawLabel = rest[:i]
	}
	if rawLabel == "" {
		return Target{}, &ResolveError{Status: http.StatusNotFound, Code: CodeNotProjectHost, Msg: "empty project label in path"}
	}
	// Percent-decode the label (non-host-safe branches arrive percent-encoded,
	// e.g. feature%2Fx). Decoding errors fall through to grammar rejection.
	decoded := percentDecodeLabel(rawLabel)
	// Lowercase the label for parity with Host mode (CANONICAL §2.4). The file
	// path remainder stays case-sensitive and is handled by the static handler.
	label := strings.ToLower(decoded)
	pathPrefix := "/host/" + rawLabel
	return d.resolveProjectLabel(label, SourcePath, pathPrefix)
}

// effectiveHost picks the authoritative host string (routing-and-serving.md §3.1):
// X-Forwarded-Host (first token) when trusted, else Host, else URL.Host.
func (d *DefaultResolver) effectiveHost(r *http.Request) string {
	if d.cfg.TrustForwardedHost {
		if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
			return firstHostToken(xfh)
		}
	}
	if r.Host != "" {
		return r.Host
	}
	return r.URL.Host
}

// ---- pure helpers ----

// splitLabel implements the CANONICAL §5.3 "--" separator rule: split on the
// FIRST "--" into handle+branch; absent "--", branch defaults to "published" and
// isPreview is false. A malformed "a--b--c" yields handle=a, branch="b--c" which
// then fails branch validation upstream (fail-closed).
func splitLabel(label string) (handle, branch string, isPreview bool) {
	if i := strings.Index(label, "--"); i >= 0 {
		return label[:i], label[i+len("--"):], true
	}
	return label, "published", false
}

// isHostPath reports whether a URL path is under the /host/ fallback prefix.
func isHostPath(p string) bool {
	return p == "/host" || strings.HasPrefix(p, "/host/")
}

// lowerStripPort lowercases a host and strips a trailing :port. IPv6 literals
// keep their brackets; the port (after the last ']') is stripped.
func lowerStripPort(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "[") {
		// IPv6 literal: strip the port that follows the closing bracket, if any.
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return host[:end+1]
		}
		return host
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// firstHostToken returns the first comma-separated token of an X-Forwarded-Host
// value, trimmed of spaces. A multi-proxy chain may comma-join values; NPM sets a
// single value. We take the FIRST (the original client-facing host).
func firstHostToken(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// percentDecodeLabel does a minimal percent-decode for the path-mode label. We
// avoid net/url.PathUnescape's strictness (it errors on a stray %), instead
// best-effort decoding well-formed %XX and leaving anything else verbatim so the
// grammar validator makes the final call. Backslashes are normalized to slashes
// nowhere — branches forbid both — so any decoded "/" makes the branch fail
// validation (fail-closed), which is the desired behavior for non-host-safe refs
// reaching a host-only grammar.
func percentDecodeLabel(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := fromHex(s[i+1])
			lo, ok2 := fromHex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// asResolveError ensures the returned error is a *ResolveError; if a validator
// returned a non-ResolveError it is wrapped with a sane default status/code.
func asResolveError(err error, defaultCode string) error {
	if err == nil {
		return nil
	}
	if re, ok := err.(*ResolveError); ok {
		return re
	}
	return &ResolveError{Status: http.StatusBadRequest, Code: defaultCode, Msg: err.Error()}
}
