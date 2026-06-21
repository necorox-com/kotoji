# Contract: Host/Path Resolution + Data-Plane Serving

> Status: **locked design, implementation-ready.**
> Scope: how an incoming HTTP request on the **data plane** is mapped to a
> `{handle, branch, isPreview}` target, how files for that target are served,
> what security/caching headers are applied, and how preview access is
> authorized. This is the single authoritative spec for the Go data plane and
> for the reverse proxy in front of it.

Related contracts (separate files, referenced here):

- `docs/contracts/site-service.md` — the `SiteService` interface that produces the servable file tree. (TODO; this doc only depends on the read-side surface defined in [§7](#7-where-files-come-from-published-tree-decision).)
- `docs/contracts/identifiers.md` — handle/branch validation rules (this doc restates the regex it relies on but the validation contract is authoritative).
- `docs/contracts/api.md` — the control-plane REST API (out of scope here).

---

## 0. Terminology & the two host classes

The data plane sees exactly two classes of host:

| Class | Meaning | Example (prod) | Example (dev) |
|---|---|---|---|
| **Control-plane host** | the app/dashboard/API/MCP host. NOT a project. | `hosting.example.com` | `kotoji.localhost:8080` |
| **Project host** | a `{handle}` (published) or `{handle}--{branch}` (preview) project. | `expense-calc.hosting.example.com` | `expense-calc.localhost:8080` |

A single deployment is configured with **one** base domain (`KOTOJI_BASE_DOMAIN`,
e.g. `hosting.example.com` / `localhost`). Everything to the *left* of that base
domain is the **label** that the resolver parses. The control-plane host is the
base domain itself with no project label (or with a configured control label,
see [§3](#3-distinguishing-control-plane-from-project-hosts)).

`isPreview` is `true` iff a branch was explicitly selected via the `--`
separator (Host) or branch suffix (path). The published branch is **never** a
preview even though it is "a branch": published is the production target.

---

## 1. Resolver: data model

```go
package resolve

// Target is the fully-resolved routing decision for one request.
// Produced by Resolver.Resolve; consumed by the static handler.
type Target struct {
	Handle     string // validated handle, lowercased. e.g. "expense-calc"
	Branch     string // git branch to serve. "" until canonicalized; see Source.
	IsPreview  bool   // true if a branch was explicitly requested (-- suffix / path branch)
	IsControl  bool   // true if this is the control-plane host (not a project)
	Source     Source // how the target was derived (host vs path); for logging/metrics
	PathPrefix string // for path-mode only: the URL prefix to strip ("/host/{handle}[--{branch}]")
}

type Source uint8

const (
	SourceUnknown Source = iota
	SourceHost           // resolved from the Host header (subdomain)
	SourcePath           // resolved from /host/{handle}[--{branch}]/... fallback
)

// Resolver maps an *http.Request to a Target. It is the swappable abstraction
// the spec requires: Host-based and path-based resolution live behind it so the
// proxy / URL scheme can change without touching the static handler.
type Resolver interface {
	// Resolve never returns a partially-valid Target. On any structural problem
	// it returns a *ResolveError carrying an HTTP status to emit.
	Resolve(r *http.Request) (Target, error)
}

type ResolveError struct {
	Status int    // 400 (malformed) | 404 (not a known host shape) | 421 (misdirected)
	Code   string // stable machine code, e.g. "bad_handle", "bad_branch", "not_project_host"
	Msg    string
}

func (e *ResolveError) Error() string { return e.Code + ": " + e.Msg }
```

The concrete implementation is constructed from config:

```go
type Config struct {
	BaseDomain   string // "hosting.example.com" | "localhost". REQUIRED. lowercased.
	ControlLabel string // label that means "control plane" when present as a subdomain
	                     // of BaseDomain. default "" => bare BaseDomain is control.
	                     // (set e.g. "app" if you want app.hosting.example.com.)
	EnablePathFallback bool // accept /host/{handle}/... in addition to Host. default true.
	TrustForwardedHost bool // read X-Forwarded-Host instead of Host. default true behind NPM.
}

func NewResolver(cfg Config, handles HandleValidator) *DefaultResolver
```

`HandleValidator` and `BranchValidator` are the small read-only interfaces from
the identifiers contract; the resolver only does *structural* validation
(grammar + reserved words). Existence (does this handle map to a real site?) is
checked later by the static handler against the DB/SiteService, so the resolver
stays pure and unit-testable without a DB.

---

## 2. Subdomain grammar & the `--` separator

### 2.1 Handle grammar (authoritative restatement)

Handles are DNS labels, lowercase only. This is the regex the resolver applies
to a candidate handle after it has been split out of the host/path:

```
handle  := ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$
```

- Charset `[a-z0-9-]`, must start and end alphanumeric (no leading/trailing `-`).
- Length **1–63** (single DNS-label limit). The control-plane may impose a
  tighter *minimum* (e.g. 3) at create time, but the resolver accepts 1–63 so
  that already-created short handles still resolve.
- **No consecutive hyphens are allowed inside a handle** — i.e. `--` can never
  appear inside a handle. This is what makes `--` an unambiguous branch
  separator (see 2.3). Enforced as a post-regex check: reject if the handle
  contains `--`.
- Reserved words (blocked at create time, also rejected by the resolver as
  handles): `draft`, `preview`, `published`, `www`, `api`, `internal`, `host`,
  `admin`, `app`, `static`, `assets`, `mcp`. (Authoritative list lives in the
  identifiers contract; keep in sync.)

### 2.2 Branch grammar

Branches are git refs but constrained for URL/DNS safety when they appear in a
host. The data plane only ever *reads* a branch name from a URL, so it applies:

```
branch  := ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$   // same shape as handle, lowercased
```

Plus:

- Total host label budget: `{handle}--{branch}` must still be ≤ 63 chars
  **as a single DNS label** (it is one label). So `len(handle)+2+len(branch) ≤ 63`.
  The resolver rejects with `421`/`400` if exceeded; the control plane must
  prevent creating preview URLs that overflow (it can fall back to path mode or
  a short branch alias).
- `published` as an explicit branch in a preview URL is **rejected** at the
  resolver (`bad_branch`): `{handle}--published` is not a thing; published is
  reached via the bare `{handle}` host. This avoids two URLs for one target.
- Real git branches may contain `/` (e.g. `feature-*` is fine, but
  `feature/x` is not host-safe). Branches intended to have preview URLs MUST be
  host-safe. Non-host-safe branches are still reachable via **path mode** with
  URL-encoding (see [§4](#4-path-fallback-grammar)) and via the editor/preview
  proxy, but get no clean subdomain. The control plane SHOULD name AI/user
  branches host-safe by convention (`feature-<user>-<slug>`).

### 2.3 The `--` separator rule (exact)

Given a project label `L` (the part to the left of `BaseDomain`):

1. If `L` contains the substring `--`, **split on the *first* `--`**:
   `handle = L[:i]`, `branch = L[i+2:]` where `i` is the index of the first `--`.
2. Else `handle = L`, `branch = ""` (published).

Because handles cannot contain `--` (2.1) and branches cannot contain `--`
either (branch grammar forbids consecutive hyphens), there is **at most one**
`--` in a well-formed label, so "first `--`" == "the `--`". If a malformed label
contains more than one `--` (e.g. `a--b--c`), the split yields `handle=a`,
`branch=b--c`; `branch` then fails branch validation → `400 bad_branch`. This is
the desired fail-closed behavior.

> Why `--` and not `.` or `-preview-`: a second dot (`{handle}.preview.host`)
> creates a 2-label wildcard that a single `*.host` cert cannot cover. A single
> embedded `--` keeps everything one DNS label under one wildcard cert. (Locked
> decision; restated for implementers.)

### 2.4 Case handling

- Host headers are **case-insensitive** per RFC; the resolver lowercases the
  entire host before parsing. `Expense-Calc.HOSTING.Example.com` →
  `expense-calc.hosting.example.com`.
- After lowercasing, handles/branches must match the lowercase grammar exactly.
  Because we lowercased first, a mixed-case URL still resolves (forgiving in),
  but canonical/generated URLs are always lowercase (strict out).
- Path mode: the `{handle}`/`{branch}` path segments are **also lowercased**
  before validation, for parity. (The rest of the path — actual file paths — is
  case-sensitive, see [§5](#5-static-serving-behavior).)

---

## 3. Distinguishing control plane from project hosts

Algorithm (Host mode), after lowercasing host and stripping `:port`:

```
host = lower(strip_port(effective_host(r)))   // see §3.1 for effective_host

if host == BaseDomain:
    -> Target{IsControl:true} when ControlLabel == ""   (bare base = control)
    -> ResolveError 404 not_project_host when ControlLabel != ""
       (you required app.base; bare base is then a misconfig/redirect)

if !hasSuffix(host, "." + BaseDomain):
    -> ResolveError 421 misdirected_request   // host not under our domain at all

label = host[: len(host) - len("."+BaseDomain)]   // the single left label
if label contains "." :
    -> ResolveError 404 not_project_host  // multi-label, e.g. a.b.base — unsupported

if ControlLabel != "" && label == ControlLabel:
    -> Target{IsControl:true}

// else: label is a project label -> parse handle/branch (§2.3) and validate
```

Notes:

- **Only one label** to the left of `BaseDomain` is ever a project. `a.b.host`
  is rejected (`404 not_project_host`). We never serve nested subdomains.
- `localhost` as `BaseDomain`: `kotoji.localhost` is control (control label
  `kotoji` by convention in dev) **or** bare `localhost` is control if
  `ControlLabel==""`. Recommended dev config sets `ControlLabel="kotoji"` so the
  dashboard lives at `kotoji.localhost:8080` and any `{handle}.localhost:8080`
  is a project. Document both; default the compose file to `ControlLabel=kotoji`.
- The control plane and data plane may be the **same binary** (run-mode
  `serve`) — see [§8](#8-data-plane-deployment-same-binary-vs-sibling). When same
  binary, `IsControl=true` requests are handed to the control-plane mux
  (dashboard/API/auth/MCP); `IsControl=false` go to the static handler.

### 3.1 effective_host (proxy trust)

```go
func (r *DefaultResolver) effectiveHost(req *http.Request) string {
	if r.cfg.TrustForwardedHost {
		if xfh := req.Header.Get("X-Forwarded-Host"); xfh != "" {
			// take the FIRST value if comma-joined; NPM sets a single value.
			return firstHostToken(xfh)
		}
	}
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
}
```

- Behind NPM, `Host` is preserved by default but `X-Forwarded-Host` is the
  belt-and-suspenders source. `TrustForwardedHost` MUST be `false` if the data
  plane is ever exposed directly to the internet (then `X-Forwarded-Host` is
  attacker-controlled). Default `true` because the documented topology always
  has NPM in front. Surface this in config docs.
- Port is stripped for host classification but the original port is kept for
  generating absolute redirect URLs.

---

## 4. Path fallback grammar

Enabled by `EnablePathFallback` (default `true`). Provides a proxy-independent
way to reach any target, behind the **same resolver**. This is what makes the
proxy swappable per the locked decision.

Grammar:

```
/host/{label}/{filepath...}
{label} := {handle}            (published)
         | {handle}--{branch}  (preview)
```

- The literal first segment is `host` (reserved word, never a handle — that is
  why `host` is in the reserved list).
- `{label}` is parsed with the **exact same** `--` split + validation as §2.3.
  Lowercased first.
- `PathPrefix` = `/host/{label}` is recorded on the Target so the static
  handler strips it before file lookup. The remainder (`/{filepath...}`) is the
  in-site path, identical to what Host mode would see as `r.URL.Path`.
- For non-host-safe branches (containing `/`), path mode accepts a
  **percent-encoded** branch (`feature%2Fx`) — but the recommended convention is
  host-safe branch names, so this is an escape hatch, not the main road.
- Control plane via path mode: `/host` with no label, or any path **not** under
  `/host/`, is `IsControl`-style routing only when the binary is in same-process
  mode; in pure data-plane mode a non-`/host/` path is `404`.

Path mode is primarily for: (a) environments where wildcard DNS/certs are
impossible, (b) the **editor live-preview** inside the dashboard, which can iframe
`/_preview/host/{handle}--{branch}/index.html` on the control-plane origin
without needing per-branch DNS. (The control plane MAY mount the data-plane
static handler under an internal `/_preview/` prefix for this; same resolver,
`SourcePath`.)

---

## 5. Static serving behavior (Go net/http)

The static handler receives a **validated, existing** `Target` plus an
`fs.FS` (or equivalent) that is the servable root for `{handle,branch}` (see
[§7](#7-where-files-come-from-published-tree-decision)). Signature:

```go
// StaticHandler serves files for a single resolved Target. It is constructed
// per-request from the resolved Target + the tree provider; the tree provider
// is the read side of SiteService.
type StaticHandler struct {
	trees TreeProvider          // resolves Target -> fs.FS (cached, immutable per commit)
	sec   SecurityHeaderConfig  // §6
	cache CacheConfig           // §6.3
	now   func() time.Time      // injected clock for tests
}

func (h *StaticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

### 5.1 Path cleaning (defense in depth)

Even though serving is read-only, clean every path:

1. Strip `PathPrefix` (path mode only).
2. `p := path.Clean("/" + reqPath)` — collapses `..`, `.`, `//`. Because we
   prepend `/` and Clean a rooted path, `..` can never escape root. Reject (404)
   if the cleaned path still contains a `..` segment (it can't after Clean of a
   rooted path, but assert it — fail closed).
3. Reject any path containing a NUL byte or a path segment that is `.git`
   (the repo metadata must never be served even if it somehow lands in the
   tree). Serve from a tree that **excludes** `.git` by construction (§7), so
   this is belt-and-suspenders.

### 5.2 Directory → index, trailing slash

| Request path | Tree state | Result |
|---|---|---|
| `/` | `index.html` exists | serve `index.html` (200) |
| `/` | no `index.html` | `404` site index page (§5.4). No directory listing, ever. |
| `/foo` | `foo` is a file | serve it (200) |
| `/foo` | `foo` is a directory, `foo/index.html` exists | **301 → `/foo/`** (canonical trailing slash), then index |
| `/foo/` | `foo/index.html` exists | serve `foo/index.html` (200) |
| `/foo/` | `foo` dir, no index | `404` |
| `/foo/` | `foo` is a file (not dir) | `404` (trailing slash on a file) |
| `/foo.html` | exists | serve (200) |
| `/foo` | `foo` missing but `foo.html` exists | **NO implicit `.html`** by default; `404`. (Optional `PrettyURLs` config can enable `/foo`→`foo.html`; off by default to keep behavior predictable.) |

- **Directory listing is never produced** (unlike `http.FileServer`). We do not
  use `http.FileServer` directly because it lists directories and leaks
  `Last-Modified` from FS mtimes; we implement the lookup ourselves over the
  tree. (We MAY reuse `http.ServeContent` for range/precondition handling on a
  resolved file, feeding it a deterministic ModTime = commit time and our own
  content-type — see 5.3/6.3.)
- Trailing-slash redirect uses **301** and preserves query string. Redirect
  `Location` is path-relative (`/foo/`) so it is correct under both Host and path
  mode (in path mode, prepend `PathPrefix`).

### 5.3 Content types / MIME

Deterministic, allowlist-driven MIME — **do not sniff arbitrary bytes for
type** (sniffing + serving user HTML is an XSS amplifier). Algorithm:

1. Take the file extension (lowercased).
2. Look it up in the kotoji MIME table (below). If found, use it verbatim.
3. If **not** in the table, the file should not exist (upload allowlist blocks
   it), but fail closed: serve as `application/octet-stream` **with
   `Content-Disposition: attachment`** and `X-Content-Type-Options: nosniff`, so
   unknown content can never execute in-origin.
4. Always send charset for text types: `text/html; charset=utf-8`, etc.
5. Always send `X-Content-Type-Options: nosniff` (§6) so the browser honors our
   declared type and never sniffs.

kotoji MIME table (the serve-side allowlist; mirrors the upload-side extension
allowlist):

| Ext | Content-Type |
|---|---|
| `.html`, `.htm` | `text/html; charset=utf-8` |
| `.css` | `text/css; charset=utf-8` |
| `.js`, `.mjs` | `text/javascript; charset=utf-8` |
| `.json` | `application/json; charset=utf-8` |
| `.map` | `application/json; charset=utf-8` |
| `.svg` | `image/svg+xml` (served with CSP that neutralizes inline script in SVG; see §6.1 note) |
| `.png` | `image/png` |
| `.jpg`, `.jpeg` | `image/jpeg` |
| `.gif` | `image/gif` |
| `.webp` | `image/webp` |
| `.avif` | `image/avif` |
| `.ico` | `image/x-icon` |
| `.woff` | `font/woff` |
| `.woff2` | `font/woff2` |
| `.ttf` | `font/ttf` |
| `.otf` | `font/otf` |
| `.txt` | `text/plain; charset=utf-8` |
| `.xml` | `application/xml; charset=utf-8` |
| `.webmanifest`, `.manifest` | `application/manifest+json` |
| `.pdf` | `application/pdf` |
| `.wasm` | `application/wasm` |
| `.mp4` | `video/mp4` |
| `.webm` | `video/webm` |
| `.mp3` | `audio/mpeg` |
| `.wav` | `audio/wav` |
| `.csv` | `text/csv; charset=utf-8` |

This table is the **single source of truth** consumed by BOTH the upload
extension allowlist and the serve-side type resolver (export it as one Go map
`var MIMEByExt map[string]string`). Keeping them one table prevents an
extension being uploadable but unserveable, or vice versa.

### 5.4 404 page

- A built-in, branded, static `404.html` is served with status `404` when no
  file resolves.
- **Per-site override**: if the tree contains `/404.html` at its root, serve
  that instead (lets non-engineers customize), still with status `404`. (This is
  the only "special" filename the serve layer honors besides `index.html`.)
- The 404 body still gets the full security headers (§6) — it is served content.

### 5.5 Methods

- Allowed: `GET`, `HEAD`. `HEAD` returns headers with no body (via
  `http.ServeContent`).
- `OPTIONS`: respond `204` with `Allow: GET, HEAD, OPTIONS`. No CORS by default
  on served content (it's static same-origin); CORS is a control-plane/API
  concern, not data plane.
- Anything else (`POST`/`PUT`/`DELETE`/...): `405 Method Not Allowed` with
  `Allow: GET, HEAD, OPTIONS`. (Hosted content is read-only; in-page `fetch` to
  third-party APIs is the user's app's business and goes direct from the
  browser, not through us.)

---

## 6. Headers on served content

### 6.1 Security headers (applied to EVERY data-plane response, including 301/404)

```
Content-Security-Policy: default-src 'self';
    script-src 'self' 'unsafe-inline' 'unsafe-eval';
    style-src 'self' 'unsafe-inline';
    img-src 'self' data: blob:;
    font-src 'self' data:;
    connect-src *;
    media-src 'self' data: blob:;
    object-src 'none';
    base-uri 'self';
    form-action 'self';
    frame-ancestors 'none';
    sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads allow-same-origin;
X-Content-Type-Options: nosniff
Referrer-Policy: strict-origin-when-cross-origin
Permissions-Policy: geolocation=(), microphone=(), camera=(), payment=(), usb=(), interest-cohort=()
X-Frame-Options: DENY        # legacy mirror of frame-ancestors 'none'
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Resource-Policy: same-origin
```

Rationale & the hard tradeoffs (these are deliberate, document them so nobody
"fixes" them later):

- **The CSP is permissive *toward the hosted app's own code* but strict toward
  cross-origin.** AI-generated static tools routinely use inline `<script>`,
  inline event handlers, inline styles, and sometimes `eval`/`new Function`
  (templating libs). A strict nonce/hash CSP would break the bulk of pasted-in
  tools and there is **no build step** to inject nonces. So `script-src` allows
  `'self' 'unsafe-inline' 'unsafe-eval'`. The safety comes from **origin
  isolation**, not from blocking the app's own JS:
  - Each project is its own origin (`{handle}.host` / `{handle}--{branch}.host`),
    so per-subdomain cookie/storage isolation (locked requirement) means one
    tool's JS cannot read another tool's storage/cookies.
  - `frame-ancestors 'none'` + `X-Frame-Options: DENY` stop clickjacking and
    stop project A iframing project B to phish.
  - `object-src 'none'`, `base-uri 'self'`, `form-action 'self'` close the
    classic injection escalations.
  - `connect-src *` is intentional: "in-page fetch OK" is a locked feature; tools
    call third-party APIs from the browser. We do not restrict outbound fetch
    targets. (A future API-proxy layer could tighten this per-project.)
- **`sandbox` directive**: applying `sandbox` to a top-level document is a strong
  default, but it strips same-origin/storage/forms unless re-granted. We
  **re-grant** `allow-scripts allow-forms allow-popups allow-modals
  allow-downloads allow-same-origin`. NOTE: `allow-scripts allow-same-origin`
  together effectively removes sandbox's isolation guarantee — so the *real*
  isolation is the per-subdomain origin, and `sandbox` here is mostly a
  hardening signal. **Make the `sandbox` directive configurable
  (`SecurityHeaderConfig.TopLevelSandbox bool`, default `false` in v1)** because
  it can subtly break some apps (downloads, popups) and the per-origin model is
  the actual control. Recommend shipping with sandbox **off** by default and
  documenting it as an opt-in hardening toggle. (See open question Q3.)
- **SVG**: `.svg` can carry inline `<script>`. Because SVGs are served `image/svg+xml`
  and referenced via `<img>`/`background`, the browser does not execute their
  script in those contexts; but if a user links an SVG directly as a top-level
  document, its script runs under this CSP. The per-origin model contains it.
  Optionally serve `.svg` with `Content-Security-Policy: script-src 'none'` via a
  per-file override — recommended (see Q4).

`SecurityHeaderConfig` lets self-hosters tune without code changes:

```go
type SecurityHeaderConfig struct {
	CSP             string // full policy string; default = the policy above (built from parts)
	ConnectSrc      string // default "*"; tighten to allowlist if desired
	TopLevelSandbox bool   // default false; if true, add the sandbox directive
	ReferrerPolicy  string // default "strict-origin-when-cross-origin"
	PermissionsPolicy string
	ExtraHeaders    map[string]string // escape hatch
}
```

### 6.2 What the data plane MUST strip / never set

- Never reflect a `Set-Cookie` from served content (there isn't one — read-only),
  and never proxy cookies between origins.
- Strip any `Server` banner detail; set `Server: kotoji`.
- Do not set `Access-Control-Allow-*` on data-plane responses (no CORS).

### 6.3 Caching headers

Content is content-addressed by commit, which lets us cache aggressively but
stay correct on publish/rollback:

```go
type CacheConfig struct {
	HTMLMaxAge   time.Duration // default 0 (no-cache, must-revalidate) for *.html
	AssetMaxAge  time.Duration // default 1h for non-HTML, when NOT fingerprinted
	UseETag      bool          // default true: ETag = commitSHA + ":" + filePathHash
	ImmutableHint bool         // default false; see note
}
```

Rules:

- **ETag** (always, when `UseETag`): `ETag: "<commitSHA12>-<sizeHex>"`. The
  served tree is pinned to a specific commit, so a file's bytes are immutable
  for that commit; ETag is strong. Honor `If-None-Match` → `304`.
- **HTML** (`text/html`): `Cache-Control: no-cache, must-revalidate` (revalidate
  every time so a publish is seen immediately) but **with ETag**, so
  revalidation is a cheap `304` when unchanged. Avoid `no-store` (kills `304`).
- **Other assets**: `Cache-Control: public, max-age=<AssetMaxAge>`. Because we
  cannot assume content-hashed filenames (no build step), do **not** send
  `immutable` by default. If a self-hoster knows their assets are fingerprinted,
  `ImmutableHint=true` adds `, immutable` and bumps max-age.
- **Last-Modified**: set to the **commit timestamp** of the served commit, not
  the FS mtime (FS mtime is meaningless after a checkout). Deterministic →
  stable conditional requests and reproducible tests.
- **Vary**: `Vary: Accept-Encoding` when compression is enabled (gzip/br on
  text types; optional, behind config). Never `Vary: Cookie` (no per-cookie
  variance on public content).
- On **preview** targets (`IsPreview`), force `Cache-Control: private, no-store`
  regardless of file type — previews are auth-gated (§7.x→ actually §below) and
  must not be cached by shared caches. Keep ETag off for previews to avoid any
  shared-cache retention.

---

## 6.4 base-href injection fallback (root-absolute asset paths)

**Problem.** A tool generated assuming it lives at the **origin root** uses
root-absolute paths like `<link href="/style.css">`, `<script src="/app.js">`,
`<img src="/logo.png">`. Under **Host mode** each project IS its own origin
root, so `/style.css` resolves correctly — **no injection needed**. The problem
only exists under **path mode** (`/host/{handle}/...`), where `/style.css`
escapes the project prefix and 404s (or worse, hits the control plane).

**Fallback (path mode only, opt-in per response):**

When `Source == SourcePath` AND the served document is `text/html` AND the
`<head>` does **not** already contain a `<base>` tag, inject:

```html
<base href="/host/{handle}[--{branch}]/">
```

immediately after `<head>` (or after `<meta charset>` if present, to not change
charset detection). This makes root-relative-looking but actually relative
resolution work: with `<base href="/host/expense-calc/">`, a `<a href="page2/">`
resolves under the prefix.

**Hard limits — document loudly:**

1. `<base href>` only affects **relative** URLs. It does **NOT** rewrite
   **root-absolute** URLs (`/style.css` stays `/style.css` and still escapes the
   prefix). So `<base>` injection does **not** fully fix root-absolute assets in
   path mode. It only fixes relative URLs and anchor resolution.
2. It does **NOT** affect URLs constructed in **JavaScript** (`fetch('/api')`,
   `new URL('/x', location)`), inline styles `url(/bg.png)`, or CSS files'
   `url(/...)`. Those still break under path mode.
3. Therefore: **Host mode is the supported, correct mode for root-absolute
   tools. Path mode is a degraded fallback.** The control plane should steer
   users to Host mode (subdomains) for anything non-trivial, and only use path
   mode for the in-dashboard preview iframe of simple tools or where DNS is
   impossible.
4. We deliberately **do not** do full HTML/CSS/JS URL rewriting (a
   "reverse-proxy that rewrites every absolute URL"): it is fragile, can't touch
   JS-constructed URLs, costs CPU on every request, and corrupts content. The
   `<base>` injection is the only rewrite we do, and only for relative URLs.
5. Injection is gated by `CacheConfig`/`SecurityHeaderConfig` so it can be
   disabled (`InjectBaseHref bool`, default `true` in path mode, irrelevant in
   Host mode). It must be skipped if a `<base>` already exists (never inject two).
6. Injection mutates the body → the `ETag`/`Content-Length` are recomputed on the
   transformed bytes for that path-mode response, and such responses are
   `Cache-Control: no-store` to avoid caching a path-specific rewrite.

Implementation note: do a **minimal, streaming** head-region scan (only inspect
up to the first `<head ...>`; if not found within the first N KB, give up and
serve unmodified). Do not parse the whole document with an HTML parser on the
hot path; a bounded byte scan for the `<head>` open tag is enough and cheap.

```go
type ServeOptions struct {
	InjectBaseHref bool // effective only when Source==SourcePath && content is html
}
```

---

## 7. Where files come from (published-tree decision)

The data plane MUST NOT run `git` per request, and MUST survive the control
plane being down (locked: "配信は published ツリーを読むだけ … 操作プレーンが落ちても公開ツールは生存").

### 7.1 TreeProvider interface (read side of SiteService)

```go
// TreeProvider gives the data plane an immutable, ready-to-serve file tree for a
// resolved Target, without invoking git on the request path.
type TreeProvider interface {
	// Tree returns an fs.FS rooted at the site's web root for the resolved commit,
	// plus the commit SHA and commit time used (for ETag / Last-Modified), plus
	// whether the handle/branch exists at all.
	// Returns ErrSiteNotFound / ErrBranchNotFound for 404 mapping.
	Tree(ctx context.Context, t Target) (TreeHandle, error)
}

type TreeHandle struct {
	FS         fs.FS     // rooted at web root; EXCLUDES .git by construction
	CommitSHA  string    // 40-hex; "" only allowed in dev/empty-site
	CommitTime time.Time // commit timestamp -> Last-Modified
	Exists     bool
}
```

### 7.2 Recommended materialization strategy

**Worktree-per-served-branch on publish/save, served from disk; never read .git on request.**

- For **published**: on `publish`, SiteService checks out the new `published`
  commit into a stable served directory `/data/sites/{uuid}/served/published/`
  (atomic swap: build into a temp dir, then `rename()` over the live symlink, so
  readers never see a half-written tree). The data plane serves this directory
  via `os.DirFS`. This is the resilient path: even if the whole control plane
  process is dead, the directory is still on disk and the data-plane serve mode
  can read it.
- For **preview branches**: materialize on demand into
  `/data/sites/{uuid}/served/branch/{branch}/` (or use `git archive | extract`
  into a temp dir, cached with a TTL keyed by branch HEAD SHA). Previews are
  auth-gated and lower-traffic, so on-demand + short cache is fine.
- The served directory is the **web root**. If a site has a configured
  subdirectory web root (future: `dist/`), the checkout/export honors it; v1
  serves the repo root. `.git` is never inside `served/` (we check out files
  only), satisfying "never serve `.git`".
- `CommitTime`/`CommitSHA` come from a tiny sidecar `served/published/.kotoji-meta`
  (JSON: `{commit, committedAt}`) written atomically with the tree, so the data
  plane gets them without touching `.git`.

Why not serve straight from a git object DB (go-git on each request)? It couples
serving to git availability and integrity, costs CPU, and breaks the "plane
isolation" guarantee. The materialized directory is the contract; go-git is fine
for the control plane's read ops (diff/log), not for the hot serve path.

### 7.3 Existence & 404 vs misdirected

- Resolver does structural validation only. The static handler calls
  `TreeProvider.Tree`. Mapping:
  - `ErrSiteNotFound` (handle has no site) → `404` (branded "no such site" page).
  - `ErrBranchNotFound` (site exists, branch missing) → `404` ("no such
    preview").
  - Renamed handle (old → new): the control plane stores `handle_redirects`;
    `Tree` (or a thin resolver hook) returns a redirect target → data plane
    emits `301` to the new host/path (see [§9](#9-handle-rename-redirects)).

---

## 8. Preview/draft access authorization

**Locked posture (recommended & adopted): `published` is PUBLIC; every preview
(any non-published branch, including `draft`) requires authentication or a
scoped preview token.** Hosted production tools are meant to be shared by URL
(often with non-logged-in colleagues), but unreviewed drafts/AI branches must
not leak.

### 8.1 Enforcement in the Go data plane

The static handler runs a `PreviewAuthz` check **before** serving when
`Target.IsPreview` is true:

```go
type PreviewAuthz interface {
	// Authorize returns nil if the request may view this preview target.
	// Returns ErrUnauthorized (no/invalid credential) or ErrForbidden
	// (valid identity but not permitted for this site).
	Authorize(ctx context.Context, t Target, r *http.Request) error
}
```

Two credential paths (either suffices):

1. **Session cookie** — the same server-side session the control plane issues
   (opaque session id cookie). BUT cookies are per-origin: a session set on
   `hosting.example.com` is **not** sent to `expense-calc--draft.hosting.example.com`.
   So we cannot rely on the dashboard cookie reaching a preview subdomain.
   Resolution:
   - Set the session cookie with `Domain=.hosting.example.com` (the base domain)
     so it is sent to all `*.hosting.example.com` subdomains. **Cost:** this
     shares the cookie across every project subdomain too, which **breaks the
     per-subdomain cookie isolation requirement for the *kotoji session*.**
     Mitigate: the kotoji **session** cookie is `Domain=.hosting.example.com`,
     `HttpOnly`, `Secure`, `SameSite=Lax`, and is **only** read by the data
     plane for authz — it is never exposed to hosted JS (HttpOnly). Hosted
     apps' *own* cookies remain per-origin (they can't set/read a domain-wide
     cookie because hosted JS can't set `Domain=.hosting.example.com` for an
     origin it doesn't control... actually it CAN set a cookie on its parent
     domain — see Q1, this is a real gap). **This is the central tension; see
     Open Question Q1.**
   - Preferred, isolation-safe path: **preview tokens** (path 2), so we do NOT
     need a domain-wide cookie at all.

2. **Scoped preview token** — a per-site (optionally per-branch) bearer token,
   matching the MCP "token scoped per-project" model. Accepted as:
   - `Authorization: Bearer <token>` (for programmatic/MCP-driven preview), or
   - a **signed, short-TTL preview cookie** set by a control-plane endpoint:
     the dashboard's "Preview" button calls
     `POST /api/sites/{id}/preview-grant?branch=draft`, the control plane
     returns a 302 to the preview origin with a one-time `?kpt=<signed>` query
     param; the data plane validates `kpt` (HMAC, contains `{uuid, branch, exp}`),
     then sets a **host-only**, `HttpOnly`, `Secure`, `SameSite=Lax` cookie
     `kotoji_preview` scoped to exactly that preview origin (no `Domain`
     attribute → host-only → preserves isolation), and 302s to strip `kpt` from
     the URL. Subsequent requests carry the host-only cookie. This keeps
     **per-origin isolation intact** and needs no domain-wide cookie.

**Recommendation: implement path 2 (signed preview-grant → host-only cookie) as
the primary mechanism; treat domain-wide session cookie (path 1) as an optional
convenience mode disabled by default.** This satisfies both "previews require
auth" and "per-subdomain isolation".

### 8.2 Failure modes

- No/invalid credential on a preview: **404, not 401/403.** Returning 404 (same
  as a non-existent branch) avoids confirming that a given `{handle}--{branch}`
  exists to an unauthenticated visitor (don't leak branch names). Internally log
  as unauthorized. (Config `PreviewUnauthStatus`: `404` default, `401` for
  debugging.)
- The `kotoji_preview` cookie is validated for `{uuid, branch}` match against
  the resolved Target; a cookie for site A's draft is rejected on site B
  (`ErrForbidden` → 404).
- Published is never run through `PreviewAuthz`.

### 8.3 Modes interaction

- `dev/no-auth` mode: `PreviewAuthz.Authorize` always returns nil → previews
  are open locally. Documented as dev-only.
- `admin-password` mode: a single shared password gates previews via the
  preview-grant flow (control plane checks the password, then issues the signed
  grant). Same data-plane code path.
- OIDC mode: preview-grant requires a valid session + site membership.

---

## 9. Handle rename redirects

- Renaming `old` → `new` keeps the UUID/path. The control plane writes a
  `handle_redirects(old_handle, new_handle, site_uuid, created_at)` row.
- Data plane: when resolving `old` (Host or path), `TreeProvider`/resolver hook
  finds the redirect and the static handler emits **`301 Moved Permanently`** to
  the equivalent URL with `new`, preserving branch + path + query:
  - Host mode: `301` to `new[--branch].BaseDomain` + same path.
  - Path mode: `301` to `/host/new[--branch]/` + same remainder.
- Redirects are followed only **one hop** (resolve `old`→`new`; if `new` is
  itself a stale alias, we still point to the current handle stored in the row,
  so chains are pre-flattened at write time). Reserved/duplicate checks at
  rename time prevent loops.

---

## 10. Local dev (`*.localhost`) vs prod (NPM wildcard) parity

The **same** resolver + static handler run in both; only config differs.

| Aspect | Local dev | Prod |
|---|---|---|
| `BaseDomain` | `localhost` | `hosting.example.com` |
| `ControlLabel` | `kotoji` (→ `kotoji.localhost:8080`) | `""` (bare `hosting.example.com`) or `app` |
| TLS | none (`http://`, `*.localhost`→127.0.0.1 auto) | wildcard `*.hosting.example.com` at NPM (TLS terminates at NPM; data plane is HTTP) |
| Proxy | none (browser hits Go directly on `:8080`) | NPM: `*.hosting.example.com`→data plane, `hosting.example.com`→control plane |
| `TrustForwardedHost` | `false` (no proxy; trust real `Host`) | `true` (read NPM's `X-Forwarded-Host`/`Host`) |
| `Secure` cookies | `false` (http) | `true` |
| Path fallback | available (`/host/...`) for parity testing | available |

Parity guarantees:

- The resolver code path is identical; only `Config` differs. A unit test runs
  the same table against both `localhost` and `hosting.example.com` base domains.
- Because `*.localhost` auto-resolves to loopback in modern browsers, dev needs
  **zero** DNS/hosts edits. The Go server must bind `0.0.0.0` (or `::`) and the
  resolver must accept `localhost`-suffixed hosts and strip `:port`.
- Cookie `Secure` and `SameSite` come from config so the preview-grant flow works
  on plain HTTP locally.

---

## 11. URL → resolved target table (examples)

Base config: `BaseDomain=hosting.example.com`, `ControlLabel=""`,
`EnablePathFallback=true`. Site `expense-calc` exists; branches
`published`, `draft`, `feature-bob-fix`.

| Incoming URL | Mode | handle | branch | isPreview | isControl | Result |
|---|---|---|---|---|---|---|
| `https://hosting.example.com/` | Host | — | — | — | ✔ | control plane (dashboard) |
| `https://hosting.example.com/api/me` | Host | — | — | — | ✔ | control API (same-binary mux) |
| `https://expense-calc.hosting.example.com/` | Host | expense-calc | published | ✖ | ✖ | serve published `index.html` |
| `https://expense-calc.hosting.example.com/style.css` | Host | expense-calc | published | ✖ | ✖ | serve `style.css` (root-absolute works; own origin) |
| `https://expense-calc--draft.hosting.example.com/` | Host | expense-calc | draft | ✔ | ✖ | preview → **authz required**, then serve draft |
| `https://expense-calc--feature-bob-fix.hosting.example.com/` | Host | expense-calc | feature-bob-fix | ✔ | ✖ | preview → authz → serve branch |
| `https://expense-calc--published.hosting.example.com/` | Host | — | — | — | — | **400 bad_branch** (published not addressable via `--`) |
| `https://expense-calc.hosting.example.com/sub/` | Host | expense-calc | published | ✖ | ✖ | `sub/index.html` (200) |
| `https://expense-calc.hosting.example.com/sub` (dir) | Host | expense-calc | published | ✖ | ✖ | **301 → `/sub/`** |
| `https://Expense-Calc.Hosting.Example.com/` | Host | expense-calc | published | ✖ | ✖ | lowercased, serves published |
| `https://a.b.hosting.example.com/` | Host | — | — | — | — | **404 not_project_host** (nested label) |
| `https://draft.hosting.example.com/` | Host | — | — | — | — | **404** (reserved word `draft` not a valid handle) |
| `https://nope.hosting.example.com/` | Host | nope | published | ✖ | ✖ | **404 ErrSiteNotFound** (valid shape, no site) |
| `https://evil.com/` (wrong domain hits us) | Host | — | — | — | — | **421 misdirected_request** |
| `https://hosting.example.com/host/expense-calc/style.css` | Path | expense-calc | published | ✖ | ✖ | serve `style.css`, PathPrefix=`/host/expense-calc` |
| `https://hosting.example.com/host/expense-calc--draft/` | Path | expense-calc | draft | ✔ | ✖ | preview authz, then serve; base-href injected |
| `https://hosting.example.com/host/expense-calc--draft/style.css` | Path | expense-calc | draft | ✔ | ✖ | serve (root-absolute `/style.css` in HTML would STILL break — see §6.4) |
| `http://kotoji.localhost:8080/` (dev) | Host | — | — | — | ✔ | control plane (dev) |
| `http://expense-calc.localhost:8080/` (dev) | Host | expense-calc | published | ✖ | ✖ | serve published (dev) |
| `http://expense-calc--draft.localhost:8080/` (dev) | Host | expense-calc | draft | ✔ | ✖ | dev/no-auth → serve; OIDC → authz |
| `https://old-name.hosting.example.com/x` (renamed→`expense-calc`) | Host | old-name | published | ✖ | ✖ | **301 → `expense-calc.hosting.example.com/x`** |

---

## 12. Go test plan (table-driven, interfaces mocked)

All resolver tests run with a fake `HandleValidator`/`BranchValidator` (no DB).
All serving tests run against an in-memory `fs.FS` (`fstest.MapFS`) via a fake
`TreeProvider`, with an injected clock.

### 12.1 Resolver (`resolve/resolver_test.go`)

- `TestResolve_HostMode_Table` — one big table: published, preview, control
  (bare + ControlLabel set), nested-label→404, wrong-domain→421,
  reserved-word→404, mixed-case→lowercased, `--published`→400, multi-`--`→400,
  trailing/leading hyphen handle→400, 63-char overflow→400, missing label
  (bare base with ControlLabel set)→404.
- `TestResolve_PathMode_Table` — `/host/{label}/...` published/preview, prefix
  computed correctly, percent-encoded branch, `/host` (no label)→404, path
  fallback disabled→404, label lowercasing, file path case preserved.
- `TestResolve_EffectiveHost` — `X-Forwarded-Host` honored iff
  `TrustForwardedHost`; first token on comma list; `:port` stripped; empty
  Host→falls back to URL.Host.
- `TestResolve_BaseDomain_localhost` vs `_prod` — same table, two base domains
  (parity).
- `TestResolve_Errors_CarryStatus` — every error path returns the documented
  `(status, code)`.

### 12.2 Static handler (`serve/static_test.go`)

- `TestServe_IndexResolution` — `/`→index, `/sub`→301, `/sub/`→index, no
  index→404, file vs dir trailing slash, `404.html` override, query preserved on
  301.
- `TestServe_MIME_Table` — every ext in the table → exact Content-Type; unknown
  ext → `application/octet-stream` + `attachment` + nosniff.
- `TestServe_PathCleaning` — `..` traversal attempts (`/../etc/passwd`,
  `/a/../../b`, encoded `%2e%2e`), NUL byte, `.git/` segment → 404, never escapes
  root.
- `TestServe_Methods` — GET/HEAD ok, OPTIONS 204+Allow, POST 405+Allow,
  HEAD has no body but correct headers/length.
- `TestServe_SecurityHeaders` — CSP/nosniff/Referrer-Policy/Permissions-Policy/
  frame-ancestors present on 200, 301, AND 404; `Server: kotoji`; no `Set-Cookie`;
  no CORS headers; sandbox present iff `TopLevelSandbox`.
- `TestServe_Caching` — ETag = commitSHA-derived; `If-None-Match`→304;
  Last-Modified = commit time (injected clock); HTML `no-cache` vs asset
  `max-age`; preview→`private, no-store`; `If-Modified-Since` behavior.
- `TestServe_BaseHrefInjection` — path mode + HTML + no existing `<base>` →
  injected after `<head>`/`<meta charset>`; existing `<base>`→not injected;
  Host mode→never injected; non-HTML→never; injected response is `no-store`,
  Content-Length recomputed; `<head>` beyond N KB→served unmodified.
- `TestServe_NotFoundPages` — built-in 404 vs per-site `/404.html`, both status
  404, both carry security headers.
- `TestServe_TreeProviderErrors` — `ErrSiteNotFound`/`ErrBranchNotFound`→404
  (branded), renamed handle→301 with branch+path+query preserved.

### 12.3 Preview authz (`serve/authz_test.go`)

- `TestAuthz_PublishedNeverGated` — published target never calls `PreviewAuthz`.
- `TestAuthz_PreviewRequiresCredential` — no credential→404 (default), or 401
  when `PreviewUnauthStatus=401`.
- `TestAuthz_PreviewGrantCookieFlow` — valid `kpt` signed param → sets host-only
  `kotoji_preview` cookie, 302 strips `kpt`; subsequent request with cookie
  passes; tampered/expired `kpt`→404; cookie for site A rejected on site B.
- `TestAuthz_BearerToken` — scoped bearer token accepted; wrong-site token→404.
- `TestAuthz_Modes` — no-auth mode→open; admin-password→grant flow; OIDC→
  membership required.
- `TestAuthz_NoCookieDomainLeak` — issued preview cookie has no `Domain`
  attribute (host-only), `HttpOnly`, `Secure` per config.

### 12.4 Integration (`serve/integration_test.go`, `httptest.Server`)

- End-to-end: spin a real `httptest` server with resolver+static+authz,
  in-memory tree; assert full responses for a representative slice of the §11
  table, including the 301 trailing-slash + redirect-following, and a path-mode
  preview with base-href injection.
- Concurrency: serve published while a simulated atomic-swap of the served dir
  happens; assert no half-tree is observed (readers see old or new, never
  partial) — validates the rename()-swap contract from §7.2.

---

## 13. Open questions / gaps (考慮漏れ)

- **Q1 (central): per-subdomain cookie isolation vs cross-subdomain session.**
  A hosted app on `evil.hosting.example.com` *can* set a cookie with
  `Domain=.hosting.example.com` (a site can write cookies to a parent domain it
  is a subdomain of). That means a malicious hosted tool could (a) set a
  domain-wide cookie that other projects' JS can read, and (b) potentially
  shadow/observe the kotoji session cookie name. This is the well-known
  "related-domain attack" / cookie-tossing surface of putting untrusted apps on
  subdomains of a domain that also holds an auth cookie. Mitigations to decide:
  (i) use a **separate registrable domain** for hosted content
  (`*.kotoji-usercontent.com`) vs the control/auth domain — strongest, breaks
  the shared-cookie problem entirely, but needs a second domain+cert; (ii) use
  **`__Host-` prefixed, host-only** cookies everywhere (no `Domain`) + the
  preview-grant flow so kotoji never relies on a domain-wide cookie (this is the
  recommended path in §8, but Q is whether to also move hosted content to its own
  domain). **Recommend: ship §8 host-only preview cookies for v1; strongly
  document the second-domain option and consider making it the default in prod.**
  Needs a product decision.
- **Q2: who materializes/garbage-collects preview worktrees?** §7.2 says
  on-demand with TTL. Need a concrete eviction policy (LRU by disk budget? TTL
  since last hit?) and a max number of live preview trees per site to bound disk.
  Belongs in the SiteService contract; flagged here because it affects serve
  latency (cold preview = checkout cost).
- **Q3: top-level `sandbox` CSP directive default.** §6.1 ships it **off**
  because `allow-scripts allow-same-origin` neutralizes it and it can break
  downloads/popups. Confirm we are comfortable relying solely on per-origin
  isolation, or decide to ship it on for an extra layer.
- **Q4: SVG script execution.** Decide whether to serve `.svg` with a stricter
  per-file `Content-Security-Policy: script-src 'none'` (recommended) or rely on
  origin isolation. Cheap to add; recommend adding.
- **Q5: range requests & large media.** We reuse `http.ServeContent` for ranges,
  but the materialized tree is on local disk — fine. If a future object-store
  backend (S3) replaces local dirs, the `fs.FS`/range story changes. Note the
  assumption (local disk fs.FS) so it isn't silently broken.
- **Q6: compression.** gzip/br on text content is left as optional config
  (§6.3). Decide whether NPM does compression (likely yes) so the data plane can
  skip it; if NPM compresses, the data plane should NOT also compress (double).
- **Q7: per-site web-root subdir** (e.g. serve `public/` not repo root). v1
  serves repo root; the TreeProvider has room for it (§7.2) but the config/UI to
  set it is unspecified — defer, but reserve a `web_root` column on the sites
  table so we don't migrate later.
- **Q8: `connect-src *` and exfiltration.** Because hosted JS may run with a
  domain-wide-ish context (pending Q1) and can fetch anywhere, a malicious tool
  can exfiltrate. This is inherent to "host arbitrary AI tools that can fetch".
  Document the trust model: **kotoji is for internal/trusted-author tools**, not
  a public untrusted-UGC sandbox. The README/threat-model doc should state this
  explicitly so operators don't deploy it as open public hosting.
- **Q9: HTTP→HTTPS & HSTS.** Assumed handled by NPM (TLS terminator). If the
  data plane is ever exposed directly, it needs its own redirect + HSTS. Note as
  an NPM responsibility; add `Strict-Transport-Security` via NPM, not the app
  (the app speaks HTTP behind the proxy).
- **Q10: idempotent handle of `published` checkout failures.** If an atomic swap
  fails midway (disk full), what does the data plane serve — last good tree?
  Recommend: swap only succeeds atomically (build-temp + rename), so a failed
  build leaves the previous `served/published/` intact; data plane keeps serving
  the old commit and the control plane surfaces the publish error. Confirm this
  is the SiteService contract.
