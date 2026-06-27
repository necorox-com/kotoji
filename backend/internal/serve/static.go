package serve

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// CacheConfig controls caching + path-mode body transforms (routing-and-serving.md §6.3/§6.4).
//
// The two "Disable*" fields are opt-OUTS so the zero value yields the spec's
// default-ON behavior (ETag on, base-href injection on) without a tri-state.
type CacheConfig struct {
	// AssetMaxAge is the max-age for non-HTML assets when NOT fingerprinted.
	// <=0 => "no-cache" (store-but-revalidate-every-time): the commit-derived
	// ETag makes a re-publish visible IMMEDIATELY via cheap 304s, so a returning
	// visitor never sees stale CSS/JS. >0 => "public, max-age=N" (opt-in to a
	// time-window cache when the operator accepts up-to-N-seconds staleness).
	AssetMaxAge time.Duration
	// DisableETag turns OFF the commit-derived strong ETag + If-None-Match
	// handling. Default false => ETag ON (routing-and-serving.md §6.3 UseETag default true).
	DisableETag bool
	// ImmutableHint adds ", immutable" + a long max-age when the operator knows
	// assets are content-hashed. Default false (no build step => filenames reused).
	ImmutableHint bool
	// DisableBaseHref turns OFF path-mode <base> injection. Default false =>
	// injection ON in path mode (irrelevant in Host mode), §6.4 InjectBaseHref default true.
	DisableBaseHref bool
}

// etagEnabled / baseHrefEnabled translate the opt-out fields to the hot-path flags.
func (c CacheConfig) etagEnabled() bool     { return !c.DisableETag }
func (c CacheConfig) baseHrefEnabled() bool { return !c.DisableBaseHref }

const (
	immutableAssetMaxAge    = 365 * 24 * time.Hour
	assetNoCacheControl     = "no-cache" // AssetMaxAge<=0 default: store but revalidate every time (ETag-driven 304s)
	htmlCacheControl        = "no-cache, must-revalidate"
	previewCacheControl     = "private, no-store"
	allowMethods            = "GET, HEAD, OPTIONS"
	indexFile               = "index.html"
	siteNotFoundFile        = "404.html"
	contentTypeOctetStream  = "application/octet-stream"
	contentTypeTextHTMLUTF8 = "text/html; charset=utf-8"
)

// normalize fills CacheConfig defaults. AssetMaxAge is intentionally NOT defaulted
// here: a non-positive value is the meaningful "no-cache" (revalidate-every-time)
// default that applyCacheHeaders emits, so a re-publish propagates immediately. The
// function is retained as the single construction-time normalization seam.
func (c CacheConfig) normalize() CacheConfig {
	return c
}

// HandlerConfig bundles construction-time settings for the static handler.
type HandlerConfig struct {
	Security SecurityHeaderConfig
	Cache    CacheConfig
	// PreviewUnauthStatus is the status returned when a preview fails authz.
	// Default 404 (no branch-name leak, fail-closed). Set 401 for debugging.
	PreviewUnauthStatus int
	// Now is the injected clock for tests. Defaults to time.Now.
	Now func() time.Time
}

const defaultPreviewUnauthStatus = http.StatusNotFound

// Handler is the data-plane HTTP entry point: it resolves a request to a Target,
// authorizes previews, then serves a single file from the materialized tree with
// the full security/cache header set. It is the http.Handler returned by NewHandler.
type Handler struct {
	resolver resolve.Resolver
	trees    TreeProvider
	authz    PreviewAuthz
	sec      SecurityHeaderConfig
	cache    CacheConfig
	unauth   int
	now      func() time.Time
	// controlHandler, when set, receives IsControl requests (same-binary mode).
	// When nil, control-plane requests get a 404 (pure data-plane mode).
	controlHandler http.Handler
}

var _ http.Handler = (*Handler)(nil)

// Deps is the dependency-injection bundle for NewHandler.
type Deps struct {
	Resolver resolve.Resolver // REQUIRED
	Trees    TreeProvider     // REQUIRED
	Authz    PreviewAuthz     // REQUIRED for previews; nil => DenyPreviewAuthz (fail-closed)
	// Control is the optional control-plane handler for same-binary mode (§3 note).
	Control http.Handler
	Config  HandlerConfig
}

// NewHandler wires the data-plane handler. Authz defaults to DenyPreviewAuthz
// (fail-closed) if nil. Security/Cache configs are normalized once here so the hot
// path never re-derives strings.
func NewHandler(d Deps) *Handler {
	cfg := d.Config
	if cfg.PreviewUnauthStatus == 0 {
		cfg.PreviewUnauthStatus = defaultPreviewUnauthStatus
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	authz := d.Authz
	if authz == nil {
		authz = DenyPreviewAuthz{}
	}
	sec := cfg.Security
	// A fully zero-value Security config => use the locked default. If ANY field is
	// set (including the single-domain isolation knobs FrameAncestors* /
	// CrossOriginResourcePolicy, which an operator threads in to allow the dashboard
	// preview iframe), go through normalize() so that operator value is honored rather
	// than discarded.
	if sec.CSP == "" && sec.ConnectSrc == "" && sec.ReferrerPolicy == "" && sec.PermissionsPolicy == "" &&
		sec.FrameAncestors == "" && sec.FrameAncestorsControlOrigin == "" && sec.CrossOriginResourcePolicy == "" {
		sec = DefaultSecurityHeaderConfig()
	} else {
		sec = sec.normalize()
	}
	return &Handler{
		resolver:       d.Resolver,
		trees:          d.Trees,
		authz:          authz,
		sec:            sec,
		cache:          cfg.Cache.normalize(),
		unauth:         cfg.PreviewUnauthStatus,
		now:            cfg.Now,
		controlHandler: d.Control,
	}
}

// ServeHTTP resolves, authorizes, and serves one request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, err := h.resolver.Resolve(r)
	if err != nil {
		h.writeResolveError(w, r, err)
		return
	}

	if target.IsControl {
		if h.controlHandler != nil {
			h.controlHandler.ServeHTTP(w, r)
			return
		}
		// Pure data-plane mode: control-host requests are not ours.
		h.writeError(w, r, http.StatusNotFound, false)
		return
	}

	// Method gate (read-only content). OPTIONS/405 short-circuit before any tree IO.
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// continue
	case http.MethodOptions:
		h.sec.applySecurityHeaders(w, false)
		w.Header().Set("Allow", allowMethods)
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		h.sec.applySecurityHeaders(w, false)
		w.Header().Set("Allow", allowMethods)
		h.writeError(w, r, http.StatusMethodNotAllowed, false)
		return
	}

	h.serveProject(w, r, target)
}

// serveProject handles a resolved project Target end to end.
func (h *Handler) serveProject(w http.ResponseWriter, r *http.Request, target resolve.Target) {
	tree, err := h.trees.Tree(r.Context(), target)
	if err != nil {
		h.handleTreeError(w, r, target, err)
		return
	}

	// Preview authz gate: published is PUBLIC and never gated (§8.2). Previews must
	// pass PreviewAuthz; failure -> PreviewUnauthStatus (404 default, fail-closed).
	if target.IsPreview {
		action, aerr := h.authz.Authorize(r.Context(), target, r, tree.SiteID)
		if aerr != nil {
			// Do NOT leak the branch: emit the unauth status with no auth headers.
			h.writeError(w, r, h.unauth, target.IsPreview)
			return
		}
		if action.SetCookie != nil {
			http.SetCookie(w, action.SetCookie)
		}
		if action.RedirectTo != "" {
			h.sec.applySecurityHeaders(w, false)
			w.Header().Set("Cache-Control", previewCacheControl)
			http.Redirect(w, r, action.RedirectTo, http.StatusFound)
			return
		}
	}

	h.serveFile(w, r, target, tree)
}

// serveFile performs path cleaning, directory/index resolution, and writes the
// file with the proper headers.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, target resolve.Target, tree TreeHandle) {
	// 1. Strip the path-mode prefix and clean the request path.
	reqPath := r.URL.Path
	if target.PathPrefix != "" {
		// PathPrefix is the escaped form; r.URL.Path is decoded. Strip a decoded
		// prefix equivalent by trimming the same number of leading segments.
		reqPath = stripDecodedPrefix(reqPath, target.PathPrefix)
	}
	cleaned, ok := cleanRequestPath(reqPath)
	if !ok {
		h.writeNotFound(w, r, target, tree)
		return
	}

	// 2. Resolve to a concrete file (handling dir -> index + trailing-slash 301).
	name, info, redirect, found := h.resolveTarget(tree.FS, cleaned)
	if redirect != "" {
		h.redirectTrailingSlash(w, r, target, redirect)
		return
	}
	if !found {
		h.writeNotFound(w, r, target, tree)
		return
	}

	h.writeContent(w, r, target, tree, name, info, http.StatusOK)
}

// resolveTarget maps a cleaned in-site path to a servable file. It returns:
//   - name: the file path within the FS to open,
//   - info: its fs.FileInfo,
//   - redirect: a non-empty IN-SITE path to 301 to (dir without trailing slash),
//   - found: whether a file was resolved.
//
// Directory listings are NEVER produced (routing-and-serving.md §5.2).
func (h *Handler) resolveTarget(fsys fs.FS, cleaned string) (name string, info fs.FileInfo, redirect string, found bool) {
	// cleaned is like "/", "/foo", "/foo/". Convert to an fs path (no leading "/").
	inSite := strings.TrimPrefix(cleaned, "/")
	hadTrailingSlash := strings.HasSuffix(cleaned, "/") && cleaned != "/"

	if inSite == "" || cleaned == "/" {
		// Root -> index.html (if present).
		if fi, err := statFile(fsys, indexFile); err == nil && !fi.IsDir() {
			return indexFile, fi, "", true
		}
		return "", nil, "", false
	}

	trimmed := strings.TrimSuffix(inSite, "/")
	fi, err := fs.Stat(fsys, trimmed)
	if err != nil {
		return "", nil, "", false
	}

	if fi.IsDir() {
		// A directory: serve {dir}/index.html, redirecting to the trailing-slash
		// canonical form first if the request lacked it.
		if !hadTrailingSlash {
			return "", nil, "/" + trimmed + "/", false
		}
		idx := trimmed + "/" + indexFile
		if ifi, ierr := statFile(fsys, idx); ierr == nil && !ifi.IsDir() {
			return idx, ifi, "", true
		}
		return "", nil, "", false // dir without index -> 404 (no listing)
	}

	// A regular file. A trailing slash on a file is a 404 (§5.2).
	if hadTrailingSlash {
		return "", nil, "", false
	}
	return trimmed, fi, "", true
}

// statFile stats a path and returns an error if it does not exist or is a dir
// caller-checked separately.
func statFile(fsys fs.FS, name string) (fs.FileInfo, error) {
	return fs.Stat(fsys, name)
}

// writeContent serializes one resolved file with MIME, caching, security headers,
// optional base-href injection, ETag/Last-Modified, and range/precondition support.
func (h *Handler) writeContent(w http.ResponseWriter, r *http.Request, target resolve.Target, tree TreeHandle, name string, info fs.FileInfo, status int) {
	ct := contentTypeForName(name)
	isSVG := strings.EqualFold(path.Ext(name), ".svg")
	isHTML := strings.HasPrefix(ct, "text/html")
	isOctet := ct == "" // not in the allowlist -> fail-closed octet-stream + attachment

	// Security headers FIRST (they apply to every response incl. 404 bodies).
	h.sec.applySecurityHeaders(w, isSVG)

	if isOctet {
		w.Header().Set("Content-Type", contentTypeOctetStream)
		w.Header().Set("Content-Disposition", "attachment")
	} else {
		w.Header().Set("Content-Type", ct)
	}

	// Read body. We read fully because (a) we may transform HTML (base-href) and
	// (b) http.ServeContent needs an io.ReadSeeker; the materialized files are
	// local-disk small static assets (Q5 assumption).
	body, rerr := readFile(tree.FS, name)
	if rerr != nil {
		// A file that vanished mid-request -> 404 (no half-tree leak).
		h.writeNotFound(w, r, target, tree)
		return
	}

	transformed := false
	if isHTML {
		body, transformed = maybeInjectBaseHref(body, target, ServeOptions{InjectBaseHref: h.cache.baseHrefEnabled()})
	}

	// Caching headers.
	h.applyCacheHeaders(w, target, tree, isHTML, transformed)

	modTime := tree.CommitTime
	// ETag/precondition handling. We skip ETag for previews and for transformed
	// path-mode HTML (the bytes are request-specific; §6.3/§6.4).
	etag := ""
	if h.cache.etagEnabled() && !target.IsPreview && !transformed {
		etag = makeETag(tree.CommitSHA, tree.CacheVersion, int64(len(body)))
		w.Header().Set("ETag", etag)
		if matchETag(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	// http.ServeContent handles HEAD, Range, If-Modified-Since, and Content-Length
	// using our deterministic modTime + the in-memory body. We pass an empty name
	// so it does not re-sniff/override our Content-Type.
	if status != http.StatusOK {
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}
	http.ServeContent(w, r, "", modTime, bytes.NewReader(body))
}

// applyCacheHeaders sets Cache-Control + Last-Modified per §6.3.
func (h *Handler) applyCacheHeaders(w http.ResponseWriter, target resolve.Target, tree TreeHandle, isHTML, transformed bool) {
	hdr := w.Header()
	if !tree.CommitTime.IsZero() {
		hdr.Set("Last-Modified", tree.CommitTime.UTC().Format(http.TimeFormat))
	}
	switch {
	case target.IsPreview:
		// Previews are auth-gated and must not be cached by shared caches (§6.3).
		hdr.Set("Cache-Control", previewCacheControl)
	case transformed:
		// A path-specific base-href rewrite must not be cached (§6.4.6).
		hdr.Set("Cache-Control", "no-store")
	case isHTML:
		// Revalidate every time so a publish is seen immediately (cheap 304 via ETag).
		hdr.Set("Cache-Control", htmlCacheControl)
	default:
		// Non-HTML assets. The DEFAULT (AssetMaxAge<=0) is "no-cache": the client
		// MAY store the asset but MUST revalidate on every use. Combined with the
		// commit+cache_version-derived ETag, a re-publish (or a cache purge) is seen
		// IMMEDIATELY — the revalidation is a cheap 304 until the content changes,
		// instead of the old public,max-age=3600 that let returning visitors serve
		// stale CSS/JS for up to an hour. An operator who accepts a staleness window
		// sets AssetMaxAge>0 (KOTOJI_ASSET_MAX_AGE) to opt into "public, max-age=N",
		// and ImmutableHint promotes content-hashed assets to a year of immutability.
		var cc string
		switch {
		case h.cache.AssetMaxAge <= 0:
			cc = assetNoCacheControl
		case h.cache.ImmutableHint:
			cc = fmt.Sprintf("public, max-age=%d, immutable", int(immutableAssetMaxAge.Seconds()))
		default:
			cc = fmt.Sprintf("public, max-age=%d", int(h.cache.AssetMaxAge.Seconds()))
		}
		hdr.Set("Cache-Control", cc)
	}
}

// redirectTrailingSlash emits a 301 to the canonical trailing-slash form,
// preserving the query string and re-prepending the path-mode prefix (§5.2).
func (h *Handler) redirectTrailingSlash(w http.ResponseWriter, r *http.Request, target resolve.Target, inSitePath string) {
	loc := inSitePath
	if target.PathPrefix != "" {
		loc = target.PathPrefix + inSitePath
	}
	if r.URL.RawQuery != "" {
		loc += "?" + r.URL.RawQuery
	}
	h.sec.applySecurityHeaders(w, false)
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusMovedPermanently)
}

// writeNotFound serves the per-site /404.html if present, else the built-in 404,
// both with status 404 and the full security headers (§5.4).
func (h *Handler) writeNotFound(w http.ResponseWriter, r *http.Request, target resolve.Target, tree TreeHandle) {
	if fi, err := statFile(tree.FS, siteNotFoundFile); err == nil && !fi.IsDir() {
		// Per-site override. Serve with 404 status + html content type.
		h.writeContent(w, r, target, tree, siteNotFoundFile, fi, http.StatusNotFound)
		return
	}
	h.writeError(w, r, http.StatusNotFound, target.IsPreview)
}

// handleTreeError maps TreeProvider errors to 404 (branded) or 301 (rename), §7.3.
func (h *Handler) handleTreeError(w http.ResponseWriter, r *http.Request, target resolve.Target, err error) {
	var redir *RedirectError
	if errors.As(err, &redir) {
		h.redirectRenamedHandle(w, r, target, redir.NewHandle)
		return
	}
	// ErrSiteNotFound / ErrBranchNotFound (and any other) -> 404. For previews we
	// also 404 to avoid confirming branch existence.
	h.writeError(w, r, http.StatusNotFound, target.IsPreview)
}

// redirectRenamedHandle 301s an old handle to the current one, preserving
// branch+path+query in both Host and path mode (§9).
func (h *Handler) redirectRenamedHandle(w http.ResponseWriter, r *http.Request, target resolve.Target, newHandle string) {
	var loc string
	if target.Source == resolve.SourcePath {
		// Path mode: /host/{new}[--branch]/{remainder}.
		newLabel := newHandle
		if target.IsPreview {
			newLabel = newHandle + "--" + target.Branch
		}
		remainder := stripDecodedPrefix(r.URL.Path, target.PathPrefix)
		loc = "/host/" + newLabel + remainder
	} else {
		// Host mode: rebuild the host with the new handle label.
		newLabel := newHandle
		if target.IsPreview {
			newLabel = newHandle + "--" + target.Branch
		}
		base := strings.TrimPrefix(hostSuffixForRedirect(r), ".")
		scheme := requestScheme(r)
		loc = scheme + "://" + newLabel + "." + base + r.URL.Path
	}
	if r.URL.RawQuery != "" {
		loc += "?" + r.URL.RawQuery
	}
	h.sec.applySecurityHeaders(w, false)
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusMovedPermanently)
}

// writeResolveError maps a *resolve.ResolveError to its HTTP status with the full
// security headers and a minimal branded body.
func (h *Handler) writeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	var re *resolve.ResolveError
	if errors.As(err, &re) {
		status = re.Status
	}
	h.writeError(w, r, status, false)
}

// writeError writes a branded, security-headered error body with the given status.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, isPreview bool) {
	h.sec.applySecurityHeaders(w, false)
	w.Header().Set("Content-Type", contentTypeTextHTMLUTF8)
	if isPreview {
		w.Header().Set("Cache-Control", previewCacheControl)
	} else {
		w.Header().Set("Cache-Control", htmlCacheControl)
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, brandedErrorBody(status))
}

// ---- pure helpers ----

// cleanRequestPath defends in depth (§5.1): prepend "/", path.Clean, then assert
// no ".." segment and no NUL byte and no ".git" segment remain. Returns the
// cleaned rooted path and ok=false on any rejection (fail-closed).
func cleanRequestPath(reqPath string) (string, bool) {
	if strings.ContainsRune(reqPath, 0) {
		return "", false // NUL byte
	}
	// Preserve a single trailing slash (significant for dir/index resolution) since
	// path.Clean strips it.
	trailing := strings.HasSuffix(reqPath, "/")
	cleaned := path.Clean("/" + reqPath)
	if cleaned != "/" && trailing {
		cleaned += "/"
	}
	// After Clean of a rooted path, ".." cannot escape; assert it anyway.
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." || seg == ".git" {
			return "", false
		}
	}
	return cleaned, true
}

// stripDecodedPrefix removes a path-mode prefix from a decoded request path. The
// stored prefix may be escaped (e.g. /host/site--feature%2Fx); we strip the same
// number of leading "/"-segments rather than a literal string compare.
func stripDecodedPrefix(decodedPath, prefix string) string {
	// Count segments in the prefix (prefix is "/host/{label}").
	prefSegs := strings.Count(strings.Trim(prefix, "/"), "/") + 1 // "host" + "label"
	p := strings.TrimPrefix(decodedPath, "/")
	segs := strings.SplitN(p, "/", prefSegs+1)
	if len(segs) <= prefSegs {
		return "/" // nothing after the prefix
	}
	return "/" + segs[prefSegs]
}

// contentTypeForName returns the allowlisted Content-Type for a path, or "" when
// the extension is not allowlisted (the handler then serves octet-stream +
// attachment). Backed by the single source of truth site.MIMEByExt.
func contentTypeForName(name string) string {
	return site.MIMEByExt[strings.ToLower(path.Ext(name))]
}

// readFile reads an entire file from the FS.
func readFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// makeETag builds the strong, commit-derived ETag:
// "<commitSHA12>-<cacheVersionHex>-<sizeHex>". The per-site cacheVersion is folded
// in so an operator "Clear cache" (which bumps sites.cache_version) changes EVERY
// asset ETag for that site, forcing clients to refetch fresh on revalidation —
// without requiring a new commit. cacheVersion 0 (never purged) keeps the ETag
// stable across restarts for a given commit+size.
func makeETag(commitSHA string, cacheVersion, size int64) string {
	sha := commitSHA
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if sha == "" {
		sha = "dev"
	}
	return fmt.Sprintf("%q", sha+"-"+strconv.FormatInt(cacheVersion, 16)+"-"+strconv.FormatInt(size, 16))
}

// matchETag reports whether an If-None-Match header matches etag (supporting the
// "*" wildcard and a comma list).
func matchETag(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, cand := range strings.Split(ifNoneMatch, ",") {
		cand = strings.TrimSpace(cand)
		cand = strings.TrimPrefix(cand, "W/") // weak comparison is fine for 304
		if cand == etag {
			return true
		}
	}
	return false
}

// requestScheme infers the scheme for an absolute redirect Location.
func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		if i := strings.IndexByte(proto, ','); i >= 0 {
			proto = proto[:i]
		}
		return strings.TrimSpace(proto)
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// hostSuffixForRedirect derives the ".{base}" portion of the current host so a
// rename redirect can swap only the leading label. It uses the request Host
// (effective host parsing already happened in the resolver; for the Location we
// rebuild from the raw host's tail after the first label).
func hostSuffixForRedirect(r *http.Request) string {
	host := r.Host
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		if i := strings.IndexByte(xfh, ','); i >= 0 {
			xfh = xfh[:i]
		}
		host = strings.TrimSpace(xfh)
	}
	// Strip the first label (the old handle's subdomain). Keep any :port.
	if i := strings.IndexByte(host, '.'); i >= 0 {
		return host[i:] // includes leading "."
	}
	return "." + host
}

// brandedErrorBody returns a minimal branded HTML body for an error status.
func brandedErrorBody(status int) string {
	title := http.StatusText(status)
	if title == "" {
		title = "Error"
	}
	return "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\">" +
		"<title>" + escapeHTML(title) + "</title></head><body>" +
		"<h1>" + escapeHTML(strconv.Itoa(status)+" "+title) + "</h1>" +
		"<p>kotoji</p></body></html>"
}

// escapeHTML escapes the minimal set for the branded error body.
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
