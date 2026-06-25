// Package resolve maps an incoming HTTP request to a routing Target on the
// kotoji data plane. It is the single, swappable abstraction the spec requires:
// host-based resolution lives behind the Resolver interface so the reverse proxy /
// URL scheme can change without touching the static handler.
//
// Serving is SUBDOMAIN-ONLY (owner decision, v1): a project is reached EXCLUSIVELY
// via its {handle}[--{branch}].<base> host. The legacy /host/{handle}/... path
// fallback was REMOVED (M1) so untrusted project content can never be served
// same-origin with the control plane's dashboard/API on the control host.
//
// This package is intentionally DB-free and pure: it performs only STRUCTURAL
// validation (grammar + reserved words + the "--" separator rule). Existence
// (does this handle map to a real site?) is checked later by the serve layer
// against site.Service, so the resolver stays unit-testable without a DB.
//
// routing-and-serving.md §1-§3 is the law for this file; it honors CANONICAL §5
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
	Source     Source // how the target was derived; always SourceHost now (subdomain-only)
	PathPrefix string // RETAINED for the serve layer's API; always "" (path mode removed, M1)
}

// Source records how a Target was derived, for logging/metrics.
type Source uint8

const (
	// SourceUnknown is the zero value (never returned for a successful resolve).
	SourceUnknown Source = iota
	// SourceHost means the target was resolved from the Host header (subdomain).
	// This is the ONLY source the resolver produces now (subdomain-only serving).
	SourceHost
	// SourcePath formerly meant the target was resolved from the /host/{handle}/...
	// path fallback. Path-mode serving was REMOVED (M1: subdomain-only) so the
	// resolver no longer produces it; the constant is retained for the serve layer's
	// path-prefix rendering API, which is now unreachable from this resolver.
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
	CodeBadHandle      = "bad_handle"
	CodeBadBranch      = "bad_branch"
	CodeNotProjectHost = "not_project_host"
	CodeMisdirected    = "misdirected_request"
	CodeLabelTooLong   = "label_too_long"
	CodeEmptyHost      = "empty_host"
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
	// EnablePathFallback is DEPRECATED and now a NO-OP. Serving is subdomain-only
	// (owner decision, v1): the /host/{handle}/... path fallback was REMOVED so
	// untrusted project content can never be served same-origin with the control
	// plane (M1). The field is retained so existing construction sites compile;
	// its value is ignored.
	EnablePathFallback bool
	// TrustForwardedHost reads X-Forwarded-Host instead of Host. MUST be false if
	// the data plane is ever exposed directly to the internet (X-Forwarded-Host is
	// then attacker-controlled). Default true because the documented topology has a
	// reverse proxy in front.
	TrustForwardedHost bool
	// BaseDomainFunc, when non-nil, supplies the EFFECTIVE base domain per request
	// (WordPress-style runtime config: env > DB > derived). It is consulted ONLY on
	// the env-EMPTY path; when KOTOJI_BASE_DOMAIN is set the composition root leaves
	// this nil and the STATIC BaseDomain field is used (today's behavior, no DB read
	// per request). The returned value is lowercased + trimmed by the resolver; an
	// empty return falls back to the static BaseDomain so a fresh, unconfigured
	// instance still classifies the control host.
	BaseDomainFunc func(r *http.Request) string `json:"-"`
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
// validators. BaseDomain is lowercased. TrustForwardedHost is taken from cfg
// verbatim (the composition root applies the documented default). EnablePathFallback
// is a deprecated no-op (serving is subdomain-only; path mode removed, M1).
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

// Resolve implements Resolver. Serving is SUBDOMAIN-ONLY (owner decision, v1):
// a project is reached EXCLUSIVELY via its {handle}[--{branch}].<base> host. The
// control host serves ONLY the control plane; every other host is classified as a
// project or foreign. Path-mode (/host/{handle}/...) serving is REMOVED so that
// untrusted project content can NEVER be served same-origin with the dashboard/API
// on the control origin (it previously could via the path fallback). Because this
// resolver is the SINGLE source of truth for the data plane, the CombinedRouter,
// AND the on-demand TLS DecisionFunc, the change applies consistently to all three.
func (d *DefaultResolver) Resolve(r *http.Request) (Target, error) {
	host := lowerStripPort(d.effectiveHost(r))
	if host == "" {
		return Target{}, &ResolveError{Status: http.StatusBadRequest, Code: CodeEmptyHost, Msg: "no host on request"}
	}

	// Resolve the EFFECTIVE base domain for this request. On the static (env-set)
	// fast path BaseDomainFunc is nil and this is the precomputed startup value
	// (zero DB reads). On the dynamic path it reflects the DB/derived value.
	base, suffix := d.effectiveBase(r)

	// Classify the host: control, project, or foreign.
	class, label, herr := d.classifyHost(host, base, suffix)
	if herr != nil {
		return Target{}, herr
	}

	switch class {
	case hostControl:
		// The control host serves ONLY the control plane. A /host/{label}/... path
		// here is NOT a project (subdomain-only serving): it falls through to the
		// control plane, which 404s the unknown route. This is the M1 fix — the old
		// path fallback served anonymous project content same-origin with the API.
		return Target{IsControl: true, Source: SourceHost}, nil
	case hostProject:
		return d.resolveProjectLabel(label, SourceHost, "")
	default:
		// Foreign host: not under the base domain. With subdomain-only serving there
		// is no path fallback to honor, so the request is misdirected.
		return Target{}, &ResolveError{
			Status: http.StatusMisdirectedRequest,
			Code:   CodeMisdirected,
			Msg:    "host not under base domain",
		}
	}
}

type hostClass uint8

const (
	hostForeign hostClass = iota
	hostControl
	hostProject
)

// effectiveBase returns the effective base domain (lowercased/trimmed) and its
// "." + base suffix for THIS request. When BaseDomainFunc is set (dynamic path)
// it is consulted and falls back to the static field on an empty return; when nil
// (env-set fast path) it returns the precomputed startup values with no work.
func (d *DefaultResolver) effectiveBase(r *http.Request) (base, suffix string) {
	if d.cfg.BaseDomainFunc == nil {
		return d.cfg.BaseDomain, d.baseSuffix
	}
	base = strings.ToLower(strings.TrimSpace(d.cfg.BaseDomainFunc(r)))
	if base == "" {
		// Unconfigured: fall back to the static field (may itself be empty).
		return d.cfg.BaseDomain, d.baseSuffix
	}
	return base, "." + base
}

// classifyHost implements routing-and-serving.md §3: distinguish control-plane
// from project hosts. host is already lowercased + port-stripped. base + suffix
// are the EFFECTIVE base domain for this request (env > DB > derived).
func (d *DefaultResolver) classifyHost(host, base, suffix string) (hostClass, string, error) {
	if host == base {
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
	if !strings.HasSuffix(host, suffix) {
		return hostForeign, "", nil // caller decides 421 vs path fallback
	}
	label := host[:len(host)-len(suffix)]
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
// pathPrefix is always "" now (subdomain-only serving; path mode removed, M1). The
// parameter is retained so the signature stays stable for the single Host-mode call
// site and any future re-introduction behind a non-control context.
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
