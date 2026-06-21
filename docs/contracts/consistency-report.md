# kotoji — Cross-Document Consistency Report

> **Purpose.** Six design docs were authored in parallel by different agents. They are
> individually strong but, being parallel, they **drifted**: the same Go interface is
> defined three incompatible ways, the token table is defined twice with different
> columns, the URL/branch grammar differs, the auth/visibility model differs, and several
> docs reference companion files under names that don't exist.
>
> This report (a) lists **every** inconsistency found, citing both sources and giving the
> **canonical resolution**, then (b) consolidates the union of all "Open questions / gaps"
> (考慮漏れ) into one prioritized P0/P1/P2 list with a recommended resolution each.
>
> **How to use it:** the resolutions here are *binding* — when a doc and this report
> disagree, this report wins and the doc must be patched. Patch-list is in §3.

Documents under review:

- `D1` = [`architecture.md`](../architecture.md)
- `D2` = [`design.md`](../design.md) (frontend)
- `D3` = [`contracts/site-service.md`](./site-service.md)
- `D4` = [`contracts/data-model.md`](./data-model.md)
- `D5` = [`contracts/mcp.md`](./mcp.md)
- `D6` = [`contracts/routing-and-serving.md`](./routing-and-serving.md)

---

## 1. Inconsistencies (with citations + canonical resolution)

### 1.1 The `SiteService` interface is defined THREE incompatible ways — **P0, blocking**

This is the single most serious finding. The DI seam that the whole codebase depends on
has three divergent definitions. They differ in **package name, interface name, ID type,
method shapes, struct names, and error model**. Pick one canonical version *before* any
backend code is written.

| Aspect | D1 (`architecture.md` §2.2) | D3 (`site-service.md`) | D5 (`mcp.md` §7.1) |
|---|---|---|---|
| Package | `siteservice` | `site` | `site` |
| Interface name | `SiteService` | `Service` | `Service` |
| Site identity | `SiteRef struct{ UUID uuid.UUID }` | `SiteID string` | `uuid.UUID` (bare) |
| Author/identity struct | `Author struct{ Name, Email; UserID uuid.UUID }` | `Actor struct{ Subject, Name, Email string }` | `AuthorID uuid.UUID` field on requests |
| Write input | `WriteFileInput{ Ref, Branch, Path, Content, BaseSHA, Author }` | `WriteFileInput{ SiteID, Branch, Path, Content, BaseSHA, Actor }` | `WriteRequest{ SiteID, Branch, Path, Content, BaseSHA, Commit, Message, AuthorID, Via }` |
| Read result | `FileBlob` | `FileContent` | `*Blob` |
| Diff method | `Diff(ctx, ref, fromRef, toRef)` | `GetDiff(ctx, id, from, to)` | `Diff(ctx, DiffRequest)` |
| Log method | `Log(ctx, ref, branch, limit, offset)` | `GetLog(ctx, id, LogOptions{... Before cursor})` | `Log(ctx, LogRequest{... Before cursor})` |
| Publish | `Publish(ctx, ref, PublishOptions)` | `Publish(ctx, id, from)` | `Publish(ctx, PublishRequest{ From, BaseSHA, Message })` |
| Zip import | `InitFromZip(...)` + create-only | `ImportZip(...)` + `CreateSiteInput.Zip` | (delegated; not specified) |
| Conflict error | (not shown) | sentinel `ErrConflict` + `ConflictError` struct (`Expected`/`Actual`) | struct `ErrConflict` (`BaseSHA`/`CurrentSHA`/`Changed`) — **no sentinel** |
| Data-plane read | `OpenTree`→`TreeFS`, `HeadSHA`, `ReadOnlySiteService` sub-iface | `ResolveForServing`→`ServeTarget`, `OpenTree` not present | (n/a) — D6 defines its own `TreeProvider` |
| Mirror/remote | explicit `SetRemote`/`MirrorPush`/`FetchAndUpdate` methods | folded into Commit/Publish; `PullPublished` is HTTP-only | implicit (push reflected in result `pushed` bool) |

**Canonical resolution (binding):**

- **Package** `internal/site`. **Interface** `site.Service`. (D3/D5 agree; D1 is the outlier — patch D1.)
- **Site identity is `uuid.UUID`**, not a `string` alias and not a `SiteRef` wrapper. Reasons: D4 (the DB) stores `sites.id UUID`; pgx/sqlc emit `uuid.UUID` (via `pgtype`/`google/uuid`); a bare `uuid.UUID` avoids a conversion layer at every call site and matches D5. The `SiteRef` wrapper (D1) and `SiteID string` (D3) are both rejected. (If a named type is wanted for readability, `type SiteID = uuid.UUID` as a **type alias** is acceptable since it stays assignable; but the interface signatures use `uuid.UUID`.)
- **Identity-on-commit struct = `Actor`** (D3's richer shape: `Subject` + `Name` + `Email`), NOT D1's `Author` and NOT D5's loose `AuthorID uuid.UUID`. `Subject` is the audit primary key; `Name`/`Email` become the git author. The MCP layer constructs an `Actor` from `claims.UserID` (look up email/name once). Add a `Via WriteSource` field (`upload|editor|mcp|system`, from D5) so the `Kotoji-Via` git trailer + `audit_log.source` are populated uniformly.
- **Write/commit/publish inputs are request structs** (D3 style, with D5's additions): `WriteFileInput`, `CommitInput`, `PublishInput`, `RollbackInput`, `CreateSiteInput`, `LogOptions`, `DiffOptions`. All carry `SiteID uuid.UUID`, `Actor`, and (for mutations) `BaseSHA`. Do **not** use D5's parallel `WriteRequest`/`CommitRequest` names — collapse to D3's `*Input` names.
- **Method names = D3's `Get`-prefixed history verbs**: `GetDiff`, `GetLog` (not D1/D5 `Diff`/`Log`). Read result struct = **`FileContent`** (D3), not `FileBlob`/`Blob`.
- **Errors = D3's taxonomy**: sentinel `ErrConflict` + typed `ConflictError{ Branch, Expected, Actual }` with `Is(ErrConflict)`. D5's struct-only `ErrConflict` (no sentinel, fields named `BaseSHA`/`CurrentSHA`/`Changed`) is rejected — but **adopt D5's `Changed []string` field** into D3's `ConflictError` (rename to `ChangedPaths`) because the MCP/UI conflict UX needs the changed-paths hint. Final shape: `ConflictError{ Branch BranchName; Expected, Actual string; ChangedPaths []string }`.
- **Data-plane read surface:** the data plane consumes a **`TreeProvider`** (D6) returning `TreeHandle{ FS fs.FS, CommitSHA, CommitTime, Exists }` — NOT D1's `OpenTree`→`TreeFS` and NOT D3's `ResolveForServing`→`ServeTarget`. Rationale: D6 is the most complete and correct (materialized worktree, no git on request path, atomic swap). `site.Service` should expose a narrow read-only method that backs `TreeProvider` (e.g. `ServedTree(ctx, siteID, branch) (TreeHandle, error)`); resolution of `Host`→`{handle,branch}` lives in `resolve.Resolver` (D6), and handle→uuid lookup lives in the `Store`, **not** in `SiteService`. Delete D3 §12's `ResolveForServing` from the interface (keep it as prose describing the *resolver*, which is a separate package).
- **Mirror/remote methods:** keep them **explicit on the interface** (D1: `SetRemote`, `MirrorPush`, `FetchAndUpdate`) rather than fully hidden (D3). Rationale: explicit methods are independently testable and let the webhook handler call `FetchAndUpdate` without reaching into Publish internals. Mirror-push on save stays *best-effort* and is invoked internally by `WriteFile`/`Commit`/`Publish`; `FetchAndUpdate` is the webhook entry point. This also resolves D3 gap #11 (`SyncFromRemote` testability) — yes, expose it.

> **Action:** author one canonical `site.Service` interface in `site-service.md` matching
> the above; make D1 §2.2 and D5 §7.1 *reference* it (show only the MCP-relevant subset in
> D5, explicitly labeled "subset; authoritative in site-service.md").

---

### 1.2 `site_tokens` table defined twice with different columns — **P0**

`access_tokens`/`site_tokens` is defined in **D1 §7.1**, **D4 §1.6**, and **D5 §3.2** — three
different shapes.

| Column | D1 (`access_tokens`) | D4 (`site_tokens`) | D5 (`site_tokens`) |
|---|---|---|---|
| table name | **`access_tokens`** | `site_tokens` | `site_tokens` |
| user FK | `user_id` | `created_by` | `user_id` |
| site FK | `site_id` (nullable → account-wide) | `site_id` (NOT NULL) | `site_id` (NOT NULL) |
| scope model | `scopes text[]` `{read,write,publish}` | `scope site_role` (single enum) | `scopes TEXT[]` `{read,write,publish}` |
| prefix col | — | `token_prefix` (~8 chars) | `token_prefix` (12 chars) |
| hash col | `token_hash bytea` | `token_hash BYTEA` + len CHECK | `token_hash BYTEA` |
| hash unique | index `WHERE revoked_at IS NULL` | `uq_site_tokens_hash` UNIQUE | `site_tokens_token_hash_key` UNIQUE |
| `can_create_sites` flag | — | — | referenced in §5.5 prose but **not in DDL** |

**Canonical resolution (binding):**

- **Table name `site_tokens`** (D4/D5). D1's `access_tokens` is the outlier — patch D1.
- **User FK = `created_by`** (D4) — clearer ("the human this token acts as"); patch D5's `user_id` → `created_by`. (Keep `ON DELETE CASCADE`.)
- **`site_id` NOT NULL** (D4/D5). D1's nullable "account-wide token" is **rejected for v1** — it contradicts D5's headline security property ("one token = one site", §11.1) and reopens the cross-site pivot surface. Account-wide tokens are deferred (see §2 gap list, P2).
- **Scope model = `scopes TEXT[]`** with `{read,write,publish}` (D1/D5), **NOT** D4's single `scope site_role` enum. Rationale: D5's tool catalogue maps each tool to a `read|write|publish` scope, and the superset chain (`publish ⊇ write ⊇ read`) is the contract the MCP layer enforces. The `site_role` enum (`owner|editor|viewer`) is the **membership** model and is a different axis — do not conflate token scope with member role. Patch D4 §1.6 to `scopes TEXT[]`. (`owner`-equivalent token power, e.g. delete-site, is intentionally *not* grantable to a token in v1.)
- **`token_prefix` length = 12 chars** (D5) — fix the value across D4 (which says "~8") and the token-format contract. D4 gap #9 ("confirm prefix length") is hereby resolved: **12**.
- **Add the `can_create_sites BOOLEAN NOT NULL DEFAULT FALSE` column** to the `site_tokens` DDL — D5 §5.5 relies on it but no DDL declares it. (See P1 gap on `create_site`-over-MCP.)
- **Token plaintext format = `kotoji_pat_<base62>`** (D5 §3.1), ≥160 bits CSPRNG. D4's prose `ktj_{site-short}_{random}` is rejected (embedding a site fragment leaks structure and complicates rotation). One format, `kotoji_pat_`, greppable for leak scanning.

---

### 1.3 URL / branch grammar mismatch between architecture and routing — **P1**

| Topic | D1 / D3 | D6 (routing, authoritative for serving) |
|---|---|---|
| Handle length | D3: `HandleMinLen=2, HandleMaxLen=63`; D4: **min 3**, max 63; D1 env: `HANDLE_MIN_LEN=2` | resolver accepts **1–63** (so short legacy handles resolve); create-time min is control-plane's call |
| `{handle}--published` | D3: not explicitly rejected; resolver "no `--` ⇒ published" | D6: **explicitly `400 bad_branch`** — published not addressable via `--` |
| Branch name in URL | D3: "standardize `feature-{user}-{slug}` (no slashes)"; D1 gap [OPEN] same | D6: branch grammar `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, host-safe only; `feature/x` via percent-encoded path mode only |
| Old-handle resolution | D3 §12: serving follows redirects, control API 404s | D6 §9: serving emits `301` (consistent) |
| Multi-`--` label | D3: split on first `--` | D6: split on first `--`, residual fails branch validation → `400` (consistent, more explicit) |

**Canonical resolution (binding):**

- **Handle length: min 3, max 63.** Reconcile to D4's **min 3** (friendlier, avoids 1–2 char handle squatting) as the *create-time* rule; the **resolver accepts 1–63** (D6) so any already-created handle still resolves. Patch D3's `HandleMinLen=2` → 3 and `HANDLE_MIN_LEN` default → 3. (Min/max remain env-tunable per D1.)
- **`{handle}--published` is rejected `400 bad_branch`** (D6). Add this rule explicitly to D3 §12 and the handle/branch validation contract.
- **Branch refs for previews are `feature-<user>-<slug>`, host-safe, no slashes** — promote D1/D3's recommendation to a **decision** (resolves D1 gap "Branch ref naming" and D3 gap #3 indirectly). Non-host-safe branches get no clean subdomain and are reachable only via path-mode percent-encoding.
- **Branch/handle grammar regex is identical** across docs: `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` + the no-`--` post-check. This already matches D3 §11, D4 §3, and D6 §2 — **good, keep consistent**; consolidate into the owed `contracts/identifiers.md`.

---

### 1.4 Visibility / auth model mismatch — `site_visibility` is 2-valued vs 3-valued — **P1**

| Doc | Visibility model |
|---|---|
| D1 §7.1 | `visibility text CHECK IN ('public','private')` — **2 values** |
| D4 §1.0 | `CREATE TYPE site_visibility AS ENUM ('public','internal','private')` — **3 values** |
| D6 §8 | "published is PUBLIC; every preview requires auth" — implies per-site public/private of *published*, plus preview gating |
| D1 env | `PUBLISHED_PUBLIC=true` (instance-global default) |

**Canonical resolution (binding):**

- **Adopt D4's 3-valued `site_visibility` enum: `public | internal | private`.** It strictly dominates D1's 2-valued CHECK and serves the internal/company use-case (`internal` = "any logged-in user of this instance can view served content"). Patch D1 §7.1 to use the enum (or `TEXT + CHECK (... IN ('public','internal','private'))` per D4's enum-evolution note).
- This governs **published** content exposure. **Previews (any non-published branch) are always auth-gated** regardless of `visibility` (D6 §8) — the two are orthogonal: `visibility` = who sees *published*; preview-grant = who sees *drafts*.
- `PUBLISHED_PUBLIC` env (D1) becomes the **instance-level cap**: when `false`, even `visibility='public'` sites require auth (a fully-private instance). Document the interaction: effective published access = `visibility='public' AND PUBLISHED_PUBLIC=true`.

---

### 1.5 Preview/session cookie model — three partially-conflicting descriptions — **P1**

| Doc | Cookie posture |
|---|---|
| D1 §8.1.6 | Session cookie `Domain = hosting.example.com` (bare, **no leading dot**); MUST NOT be the wildcard parent; preview cookie host-scoped (gap [OPEN]) |
| D1 §3g | Sets cookie `Domain=hosting.example.com` |
| D6 §8.1 path 1 | Session cookie `Domain=.hosting.example.com` (**with leading dot**, sent to all subdomains) — then immediately notes this breaks isolation |
| D6 §8.1 path 2 | Preferred: **host-only** signed `kotoji_preview` cookie via `/api/sites/{id}/preview-grant` (no `Domain` attr) |
| D2 §4.3 | "opaque session cookie (HttpOnly, Secure, SameSite=Lax)" — domain unspecified |

**Canonical resolution (binding):**

- **The kotoji *session* cookie is host-only on the control host** (`Domain` attribute **omitted**, or bare `hosting.example.com` with **no leading dot**; prefer omitting `Domain` entirely → host-only). Use the `__Host-` prefix (`__Host-kotoji_session`) to enforce host-only + `Secure` + `Path=/`. This is D1's intent stated more strongly; **reject D6 §8.1 path 1's `Domain=.hosting.example.com`** session cookie.
- **Preview access uses D6 §8.1 path 2** (the signed preview-grant → host-only `kotoji_preview` cookie) as the *primary* mechanism. The domain-wide session cookie is **not** used for previews. This resolves D1 gap §8.1.6 [OPEN] and D6 Q1 (partially): host-only everywhere; never rely on a domain-wide cookie.
- The deeper "separate registrable domain for hosted content" question (D6 Q1) remains a **product decision** — surfaced in §2 (P1).

---

### 1.6 Optimistic-lock conflict shape differs between SiteService and MCP — **P1**

- D3 §5: `ConflictError{ Branch, Expected, Actual }`.
- D5 §5.4 / §7.1: conflict body has `base_sha` / `current_sha` / `changed_paths`; the Go `ErrConflict` struct has `BaseSHA`/`CurrentSHA`/`Changed`.
- D2 §4.1: frontend error envelope `details: { baseSha, currentSha }`.

**Canonical resolution:** one struct, **`site.ConflictError{ Branch BranchName; Expected, Actual string; ChangedPaths []string }`** (per §1.1). The wire/JSON mapping is fixed: REST + MCP both serialize `{ "expected": ..., "actual": ..., "changedPaths": [...] }`. The frontend (D2) must rename `baseSha`/`currentSha` → `expected`/`actual` (or the OpenAPI spec maps them — pick one and put it in `openapi.yaml`). **Recommendation:** wire names `expected`/`actual`/`changedPaths` everywhere; patch D2 and D5 to match.

---

### 1.7 Zip / upload limit constants disagree across docs — **P2**

| Limit | D1 env (§6.1) | D3 §7 constants |
|---|---|---|
| Max compressed zip | `MAX_UPLOAD_BYTES=50MB` | `MaxZipBytes=50 MiB` ✓ |
| Max uncompressed total | `ZIP_MAX_TOTAL_BYTES=200MB` | `MaxUncompressedBytes=200 MiB` ✓ |
| Max entries | `ZIP_MAX_FILES=2000` | `MaxZipEntries=2000` ✓ |
| Max ratio | `ZIP_MAX_RATIO=100` | `MaxCompressionRatio=100` ✓ |
| Per-entry cap | (none) | `MaxEntryUncompressed=50 MiB` |
| MCP single-file write | (n/a) | D5: 5 MiB (`KOTOJI_MCP_MAX_FILE_BYTES`) |

**Canonical resolution:** values agree (good). Two nits: (a) **MB vs MiB** — D1 uses decimal bytes (`52428800` = 50 MiB actually, so it's MiB mislabeled "50MB"); standardize on **MiB** and fix the labels. (b) Add D3's `MaxEntryUncompressed` (per-entry 50 MiB) as an env var `ZIP_MAX_ENTRY_BYTES` in D1. (c) The extension allowlist must be **one source** — see §1.8.

---

### 1.8 Extension allowlist defined in 4 places, with drift — **P2**

| Doc | Allowlist |
|---|---|
| D1 env `ZIP_ALLOWED_EXT` | `.html,.htm,.css,.js,.mjs,.json,.svg,.png,.jpg,.jpeg,.gif,.webp,.ico,.woff,.woff2,.ttf,.txt,.md,.map,.xml,.csv,.wasm` |
| D3 §7.4 | `.html .htm .css .js .mjs .json .svg .png .jpg .jpeg .gif .webp .ico .txt .md .woff .woff2 .ttf .map .xml .webmanifest` (**adds** `.webmanifest`, **drops** `.csv .wasm`) |
| D5 §4.3 | `.html .htm .css .js .mjs .json .svg .png .jpg .jpeg .gif .webp .ico .txt .md .woff .woff2 .map` (**drops** `.ttf .xml .wasm .csv .webmanifest`) |
| D6 §5.3 MIME table | superset incl. `.avif .otf .pdf .mp4 .webm .mp3 .wav .csv .wasm .manifest` |

**Canonical resolution (binding):** **D6's MIME table is the single source of truth** (§5.3 says so, and a serve-side type must exist for any uploadable ext, else it uploads but can't serve). Export it as one Go map `site.MIMEByExt`; the upload allowlist = `keys(MIMEByExt)`; the MCP `write_file` allowlist = the same set **minus** large-binary/media types (MCP is text-first — see P1 gap on binary-via-MCP). Patch D1 env doc, D3 §7.4, D5 §4.3 to all say "the allowlist is `site.MIMEByExt`'s keys; see routing-and-serving.md §5.3." Concretely include `.wasm` (AI tools ship wasm), `.webmanifest`/`.manifest`, `.csv`, `.avif`, `.otf`; decide media (`.mp4/.webm/.mp3/.wav`) per the large-media policy (P1 gap).

---

### 1.9 Data-plane port / run-mode: same binary vs sibling — consistent but under-pinned — **P2**

- D1 §4.2: **same binary**, `RUN_MODE=all|control|serve`, control `:8080` + data `:8081`; RO sub-interface when split.
- D3 §9: data plane reads via `ResolveForServing` worktrees.
- D6 §8.3: "control plane and data plane may be the same binary (run-mode `serve`)."
- D1 system diagram + NPM rules: data plane `:8081`.

**Resolution:** consistent. Pin: **`RUN_MODE` ∈ {`all`,`control`,`serve`}**, control `:8080`, data `:8081`. The data plane depends on **`TreeProvider` + `resolve.Resolver`** (§1.1), not the full `Service`. No conflict; just ensure D3 stops implying `ResolveForServing` is on the `Service` interface (it's the resolver package).

---

### 1.10 Roles/permissions defined inconsistently — **P1 (also a gap)**

- D1 §7.1: `site_members.role CHECK IN ('admin','editor','viewer')`.
- D4 §1.0: `CREATE TYPE site_role AS ENUM ('owner','editor','viewer')`.
- D2 §3.2 / §5.2: assumes `owner/editor/viewer` + instance `admin`.

**Mismatch:** D1 uses **`admin`** as a site role; D4 uses **`owner`**. Different words for the per-site top role; `admin` also collides with the *instance*-level `is_admin` flag (D4 `users.is_admin`).

**Canonical resolution (binding):** per-site roles = **`owner | editor | viewer`** (D4's `site_role` enum). Instance-level superuser = **`users.is_admin`** (separate axis). Patch D1 §7.1 `site_members.role` to `owner/editor/viewer` (or use the `site_role` enum). The exact *capabilities* of each role (who publishes, can viewers see drafts, per-branch perms) remain a **gap** — see §2 P0 gap "roles & permissions". The DB enum is fixed; the policy table is owed.

---

### 1.11 Companion-doc filename drift (broken cross-references) — **P2**

Docs reference companion files under inconsistent names; some don't exist:

| Referenced as | By | Canonical file |
|---|---|---|
| `docs/siteservice.md` | D1 §0, §2 | `docs/contracts/site-service.md` (exists) |
| `docs/db-schema.md` | D1 §0 | `docs/contracts/data-model.md` (exists) |
| `docs/contracts/db.md` | D5 §intro, §3.2 | `docs/contracts/data-model.md` |
| `docs/contracts/api.md` | D5, D6 | `docs/contracts/openapi.yaml` (**not yet written**) + a prose `api.md` (owed) |
| `docs/contracts/api-openapi.yaml` | D4 §0 | `docs/contracts/openapi.yaml` |
| `docs/contracts/mcp-tools.md` | D1 §2 | currently inlined in `mcp.md` (extract or re-point) |
| `docs/contracts/identifiers.md` | D6 §intro, §2 | **not yet written** (handle/branch rules currently restated in D3/D4/D6) |
| `docs/responsive-design.md` | D1 §2 frontend tree | `docs/design.md` (exists) |

**Canonical resolution:** standardize on the filenames in the right column. Patch every
cross-reference. **Write the two owed files** (`openapi.yaml`, `identifiers.md`) and either
extract `mcp-tools.md` or have D1 reference `mcp.md` directly. (Tracked as P0/P1 gaps below.)

---

### 1.12 `audit_log` source enum vs `via` free-text — **P2**

- D4 §1.0: `CREATE TYPE audit_source AS ENUM ('upload','editor','mcp','system')`; column `source audit_source`.
- D1 §7.1: `audit_log.via text` with values `'ui'|'mcp'|'upload'|'webhook'|'admin'`.
- D5 §5.9: `via` provenance `mcp|monaco|upload|github`.

**Mismatch:** three different value sets (`editor` vs `ui` vs `monaco`; `system` vs `webhook`+`admin`; `github`).

**Canonical resolution (binding):** adopt **D4's `audit_source` enum `{upload, editor, mcp, system}`** as the DB column (`source`, not `via`). Map the others: `ui`/`monaco` → `editor`; `webhook`/`github`/`admin` → `system` (with the finer distinction in `metadata.kind`). The git `Kotoji-Via` trailer (D5) and the API `via` field may use the friendlier 4-value set but must map 1:1 to the enum. Patch D1 (`via` → `source` enum) and D5 (`monaco`→`editor`, `github`→`system`).

---

### 1.13 `published` SHA column name + `is_published` divergence — **P2**

- D1 §7.1: `sites.published_sha text` + `published_at timestamptz`.
- D4 §1.3: `sites.published_commit_sha TEXT` (+ format CHECK); **no `published_at`**; `Site.HasPublished` derived.
- D3: `Site.HasPublished bool` (derived from published branch existence).
- D5 §5.7/§7: references `sites.is_published` / `published_commit` columns.

**Canonical resolution (binding):** column = **`published_commit_sha`** (D4, with its hex CHECK). `is_published` is **derived** (`published_commit_sha IS NOT NULL`), not a stored column — patch D5's `is_published` references. **Add `published_at timestamptz`** (D1) to D4's `sites` table — the dashboard "published 2h ago" badge needs it and it's cheap. Patch D1 `published_sha` → `published_commit_sha`.

---

### 1.14 `ResolvedRef` / list-files signature mismatch — **P2**

- D5 §7.1: `ListFiles(ctx, siteID, branch, ref, pathPrefix) ([]FileEntry, ResolvedRef, error)` — note 4 args + extra `ResolvedRef` return, and a positional `ref`.
- D3 §4: `ListFiles(ctx, id, branch, dir, recursive) ([]FileEntry, error)` — `recursive bool`, no `ref`, no `ResolvedRef`.

**Canonical resolution:** merge to one signature. MCP needs `ref` (list at a commit) and the resolved SHA; D3 needs `recursive`. Final: `ListFiles(ctx, in ListFilesInput) ([]FileEntry, ResolvedRef, error)` where `ListFilesInput{ SiteID, Branch, Dir, Ref string, Recursive bool }` and `ResolvedRef{ SHA string }`. Adopt the request-struct form (consistent with §1.1). Patch both D3 and D5.

---

### 1.15 Minor naming nits — **P2 (batch-fix)**

- **`BranchName` type** (D3) vs bare `string` branch (D5/D6). Use `string` at the *wire/MCP* boundary, `site.BranchName` inside the Go core; document the cast at the edge. (D3's typed `BranchName` is fine internally.)
- **`PreviewSubdomain`** (D3 `Branch.PreviewSubdomain`) vs D5's `published_url`/`draft_url` (full URLs). The Go core returns the *label fragment*; the API/MCP layer composes full URLs from `CONTROL_BASE_URL`/`HOSTING_BASE_DOMAIN`. Keep both, document the layering.
- **`HOSTING_BASE_DOMAIN`** (D1) vs **`KOTOJI_BASE_DOMAIN`** (D6) vs `BaseDomain` (D6 `resolve.Config`). One env name: **`KOTOJI_BASE_DOMAIN`** (namespaced, matches D5's `KOTOJI_MCP_*` and D6). Patch D1.
- **`SESSION_COOKIE_DOMAIN`** (D1) — given §1.5's host-only decision, this env should default to **empty (host-only)** and be documented as "leave empty unless you know you need cross-subdomain". Patch D1's "derive from CONTROL_BASE_URL" default.
- **`citext` for `email`/`handle`** is consistent across D1/D4 (good).
- **`description` column** exists in D4 `sites` and D5 `create_site` but is **absent from D1 §7.1 `sites`** — add it to D1 (or just defer to D4 as canonical, which it is).

---

## 2. Consolidated Open Questions / Gaps (prioritized)

The union of every "考慮漏れ" across all six docs, **deduplicated** and prioritized. Each
has a recommended resolution. **P0 = blocks coding or first release; P1 = needed before
the feature it gates ships; P2 = polish / future.** "(needs user)" = a product decision I
cannot make unilaterally.

### P0 — resolve before / during foundation

| # | Gap (source docs) | Recommended resolution |
|---|---|---|
| P0-1 | **Canonical `site.Service` interface** — three incompatible defs (§1.1). | Adopt §1.1 canonical version; author it once in `site-service.md`; make D1/D5 reference it. **Do this first** — everything depends on it. |
| P0-2 | **`site_tokens` DDL** — three shapes (§1.2). | Adopt §1.2 canonical DDL (`scopes TEXT[]`, `created_by`, `site_id NOT NULL`, `can_create_sites`, prefix=12). One migration. |
| P0-3 | **OpenAPI spec ownership & drift CI** (D1 §7, D2 §4.1/#11). The single-source-of-truth REST contract `openapi.yaml` does not exist; without it the Go↔Next types can't be generated and the two-language split rots. | **Hand-author `openapi.yaml`** (OpenAPI 3.1) alongside the Go handlers. Backend: `oapi-codegen -generate types` for Go DTOs; Frontend: `openapi-typescript` + `openapi-fetch`. **CI gate:** regenerate both; fail if working tree changes. Decide now: spec is hand-written, types generated *from* it (recommended). (needs user to ratify the toolchain.) |
| P0-4 | **Roles & permissions capability matrix** (D2 #2, D4 §1.0, §1.10). Enum is fixed (`owner/editor/viewer`) but capabilities aren't. | Define the matrix: **owner** = all incl. delete/rename/members/publish/tokens; **editor** = save/commit/create-branch/request-publish/Monaco+MCP-write, **and direct publish** (small-team default; gate behind a per-site `require_publish_approval` flag if stricter); **viewer** = read files/history/**preview** (yes, viewers see drafts — they're trusted members), no writes. **MCP token scope (`read/write/publish`) is a separate axis** capped by the creating user's role. Put this table in `identifiers.md` or a new `authz.md`. (needs user to confirm "can editors publish directly?" and "do viewers see drafts?".) |
| P0-5 | **Authz boundary: is `site.Service` authz-aware?** (D3 #13). Confused-deputy risk: an MCP token for site A `WriteFile`-ing site B. | **`site.Service` is NOT authz-aware** for membership; it trusts the `SiteID` it's given. Authorization (session→role, token→site+scope) is enforced **above** it in the API/MCP middleware. The MCP layer's structural guarantee (no tool takes a site selector; always calls with `claims.SiteID`) is what prevents the pivot (D5 §4.1). **However**, `site.Service` MUST still validate paths/branches/baseSHA (defense in depth) and return `ErrForbidden` only for git-level ownership of *operations it owns* (e.g. refusing to delete `published`). Document this boundary explicitly in `site-service.md` §4. |
| P0-6 | **Hard vs soft delete for sites** (D3 #10, D4 #6, D1 §8.4). Conflicting: D1 has `deleted_at`, D4 hard-deletes, D3 recommends soft. | **Soft-delete:** add `sites.deleted_at timestamptz` (D1 already has it; D4 must add it). Delete = set `deleted_at`, handle stays reserved during grace, repo retained. A reaper job after **30d** `git bundle`s to `/data/backups/{uuid}` then `rm -rf`s. Resolves the divergence (D4 currently hard-deletes — patch it). (needs user to confirm 30d grace.) |
| P0-7 | **Preview/session cookie isolation** (§1.5, D6 Q1, D1 §8.1.6). | Host-only `__Host-` cookies everywhere; preview = signed preview-grant → host-only `kotoji_preview` (D6 path 2). Ship for v1. **The "separate registrable domain for hosted content" is P1** (below). |

### P1 — resolve before the gated feature ships

| # | Gap (source docs) | Recommended resolution |
|---|---|---|
| P1-1 | **Separate domain for hosted content** (D6 Q1, Q8). Hosted apps on `*.hosting.example.com` can set `Domain=.hosting.example.com` cookies (cookie-tossing) and `connect-src *` enables exfiltration. | Document loudly: **kotoji is for internal/trusted-author tools, NOT public untrusted UGC** (D6 Q8). For prod, **strongly recommend a second registrable domain** (`*.kotoji-usercontent.com`) for served content, separate from the control/auth domain; make it configurable (`KOTOJI_USERCONTENT_DOMAIN`, optional). v1 works on one domain with host-only cookies; two-domain is the hardening upgrade. (needs user: buy a second domain?) |
| P1-2 | **MCP token management UI** (D2 #3, D5 §8). No screen issues/revokes tokens. | Add a **"Connect AI / MCP" panel** in ProjectDetail → Settings: create (copy-once), list (prefix + last-used + scope + expiry), revoke, and the ready-to-paste connection snippet (D5 §8). REST endpoints: `POST/GET/DELETE /api/sites/{id}/tokens`. Add to D2 inventory. |
| P1-3 | **"Request publish" vs direct publish — mode & surfacing** (D2 #4, D5 §5.7, D1 §3d). Per-project? per-role? global? PR-status reflection undefined. | **Per-site setting `publish_mode ∈ {direct, request}`** (column on `sites`, default `direct` for small teams). `direct` → editor/owner publish immediately. `request` → non-owners get "公開をリクエスト" which opens/updates a GitHub PR via mirror; merge→webhook→pull→publish. PR/pending state reflected via a `publish_requests` view or by reading the mirror PR (defer the GitHub-PR-status polling to P2). Resolves D2 #4. (needs user to confirm per-site vs global.) |
| P1-4 | **GitHub-driven state & notifications** (D2 #5, D5 §11.8). Merge→webhook→pull→redeploy is async/external; no UI surface. | Add a lightweight **activity/notifications** read from `audit_log` (source=`system`, kind=`webhook`): a per-site "published changed via GitHub" banner + a bell with recent external events. Reconcile optimistic local state by invalidating TanStack queries on a polled `/api/sites/{id}/activity` (or SSE if MCP goes stateful — P1-9). |
| P1-5 | **Config endpoint for upload limits** (D2 #8, §1.7/§1.8). UI hard-codes limits today. | Add `GET /api/config` returning `{ maxUploadBytes, zipMaxFiles, allowedExtensions, handleMinLen, handleMaxLen, reservedHandles, baseDomain, authModes, publishMode }` (public-safe subset). Frontend fetches it for accurate pre-upload guidance and handle-validation hints. |
| P1-6 | **Optimistic-lock UX edge cases** (D2 #9, D3 #4). Multi-file conflict, conflict-during-publish, what "overwrite" does at git level. | Backend semantics: **branch-level lock, no force flag in v1** (D3 §5). "Overwrite" in the UI = re-read server state, re-apply the user's edit on top, commit with the new baseSHA (a *new commit*, never a git force). Conflict-during-publish → `ErrPublishConflict` with paths; user resolves on a branch and republishes (D3 §6). Make D2's ConflictResolver copy truthful to this. 3-way disjoint-file merge is **deferred** (D3 #4). |
| P1-7 | **Binary files: read/write/edit rules** (D3 #5, D5 #5, D1 §8.5.3). | **Binaries are upload-only (Zip/dashboard)**; MCP `write_file` and Monaco are **text-first**. `read_file`/MCP returns `encoding:"base64"` for binary with a truncation cap (D5 §10); Monaco shows a read-only "binary asset" panel (D1 §8.5.3); `get_diff` marks "binary changed". This resolves D3 #5, D5 #5 consistently. |
| P1-8 | **Branch creation over MCP** (D5 #6). `write_file(branch:"feature-x")` — auto-create or explicit tool? | Add an explicit **`create_branch` tool** (scope `write`) with a per-site cap on live feature-branches (e.g. 20) to bound preview-URL/worktree growth. **No auto-create on first write** (prevents an AI spawning unbounded branches). `site.Service.CreateBranch` already exists (D3). |
| P1-9 | **MCP Streamable-HTTP stateless vs stateful** (D5 #4). Affects horizontal scale + server→client push. | **v1 = stateless** (no `Mcp-Session-Id`, no sticky proxy). Push notifications (e.g. "draft changed under you") are deferred; the frontend gets reconciliation via polling (P1-4). Revisit stateful when multi-replica + live-collab is on the roadmap. Pairs with the single-replica decision (P1-12). |
| P1-10 | **Worktree disk cost & eviction for previews** (D3 #12, D6 Q2). Per-(uuid,branch) worktrees grow unbounded. | Policy: **published** → permanent materialized `served/published/` (atomic swap). **Previews** → on-demand checkout into `served/branch/{branch}/`, **LRU-evicted by a disk budget** (`KOTOJI_PREVIEW_CACHE_BYTES`, default 2 GiB) + **TTL since last hit** (default 24h), max live preview trees/site (default 10). Put this in `site-service.md`. |
| P1-11 | **Mirror-push failure surfacing channel** (D3 #9, D5 §5.4). | **Response `warnings []string`** on the save/publish result (D5 already does this for MCP) **plus** an `audit_log` row (source=`system`, kind=`mirror_failed`) and a `sites.mirror_status` flag for the dashboard "GitHub sync pending" banner. Three channels, but `warnings[]` is the synchronous one the UI shows immediately. |
| P1-12 | **Multi-replica horizontal scaling?** (D1 §8.6, D3 #2, D6 implicit). Affects flock vs pg_advisory_lock vs in-proc mutex. | **v1 = single backend replica** (in-process keyed mutex + flock for the control/serve split). Reserve the seam: the lock acquisition is behind an interface so a `pg_advisory_xract_lock(hashtext(uuid))` impl can drop in for HA. The baseSHA optimistic check is the cross-process safety net meanwhile. (needs user to confirm HA is not a v1 goal.) |
| P1-13 | **i18n library & launch language** (D2 #1). | **`next-intl`** (house standard: tsumo/shop/uma all use it; App-Router-native). **Launch ja-only** with all copy as message keys (so en is a later drop-in, not a refactor). Resolves D2 #1. (needs user to confirm ja-only-at-launch.) |
| P1-14 | **`create_site` over MCP** (D5 #2). | **Disabled by default** via `site_tokens.can_create_sites=false` (P0-2 adds the column). Expose the capability only behind an explicit dashboard opt-in; the new site's first token is still issued **only in the dashboard** (never minted over MCP, D5 §11.2). (needs user to decide if AI-autonomous site creation is ever desired.) |
| P1-15 | **`Commit` vs `WriteFile` overlap + working-tree diff** (D3 #6, #7). With `WriteFile` committing per-call, is multi-file `Commit` / a staging area real? | **Keep `Commit` for the multi-file batch case** (MCP `write_file(commit:false)` ×N then `save`; Monaco "save all"), implemented as a real staged-then-commit under one lock. Therefore **`GetDiff(to=="")` against the working tree IS meaningful** and stays. If product decides single-file-per-commit only, drop both — but the MCP `save` tool (D5 §5.6) needs the batch path, so **keep it**. |
| P1-16 | **Clean URLs (`/foo`→`foo.html`) default** (D1 §8.6, D6 §5.2). | **Off by default** (D6 §5.2 `PrettyURLs` config, default off) — predictable behavior; opt-in per site. Resolves the [OPEN]. |
| P1-17 | **SVG script execution** (D6 Q4). | **Serve `.svg` with per-file `Content-Security-Policy: script-src 'none'`** (recommended, cheap). Add to the data-plane header logic. |

### P2 — polish / future

| # | Gap (source docs) | Recommended resolution |
|---|---|---|
| P2-1 | Account-wide / multi-site MCP tokens (D5 #1). | Defer. If added, a join table `token_sites` + the content tools take a site arg validated against the allow-list. v1 stays one-token-one-site. |
| P2-2 | Token verification per-call vs cached (D5 #3). | **Per-call DB lookup** for v1 (instant revoke). Revisit with metrics; if hot, cache `TokenInfo` 30s with a revocation epoch. |
| P2-3 | Quotas modeled in DB (D4 #2, D1 §8.4). | Add `users.max_sites`, `users.max_repo_bytes` (nullable → instance default) + `sites.repo_bytes` cache (updated post-`git gc`). Enforce `413`/`quota_exceeded`. Own contract when built. |
| P2-4 | `sites.repo_bytes` / file-count cache (D4 #3, D3 implicit). | Add as part of P2-3; explicitly a git-derived read-through cache. |
| P2-5 | `site_branch_cache` for cross-site branch queries (D4 #1). | Defer until a real admin/analytics use case. Branches stay git-only. |
| P2-6 | Per-branch visibility / preview tokens table (D4 #4, D3 implicit). | Covered functionally by the host-only preview-grant (P0-7); a `branch_preview_tokens` table only if shareable-by-link previews are wanted. Defer. |
| P2-7 | GitHub linkage detail table `site_github` (D4 #5). | When mirror/webhook lands: 1:1 `site_github(site_id, remote_url, installation_id, webhook_secret, last_pulled_sha)` to keep secrets out of `sites`. |
| P2-8 | `audit_log` retention / partitioning (D4 #7). | Fine at small scale; revisit with `PARTITION BY RANGE (created_at)` + retention at millions of rows. |
| P2-9 | Email-based identity merge across IdPs (D4 #8). | Accepted for internal/company scope; documented. Match on `(provider, subject)` primarily; `email` join is the upsert key. |
| P2-10 | citext collation widening (D4 #10). | Safe (handles ASCII-only by CHECK). Flagged so charset isn't widened without revisiting. |
| P2-11 | Rollback target reachability (D3 #8, D5 §5.10). | **`to_sha` must be an ancestor reachable on the branch** (D5 already enforces; D3 #8 asks). Confirm in `site-service.md`: reject non-ancestor with `not_found`. |
| P2-12 | Old-handle redirect TTL / reclaim (D3 #1, D4 §1.7). | **Redirects are permanent** (D4 §1.7: cheap rows, don't break external links); admin can prune. Serving 301s, control API 404s old handles (D3 §12) — confirmed asymmetry, documented. |
| P2-13 | ImportZip replace vs merge on existing branch (D3 #3). | **Replace** for v1 (matches "drop a folder, get a URL"). A merge/add mode + UI toggle is future. |
| P2-14 | git author/agent identity richness for MCP (D5 #7). | Add `Kotoji-Token: <token-name>` trailer (never the token value) for audit. Cheap; do it. |
| P2-15 | MCP resources/prompts (D5 #9). | Out of scope v1 (tools only). Conscious omission. |
| P2-16 | MCP OAuth discovery (RFC 9728 / DCR) (D5 #10). | Static bearer tokens for v1 (copy-from-dashboard UX). Revisit if a client mandates it. |
| P2-17 | Brand assets: glyph mark, favicon, OG, empty-state illustration, gold-glint spec (D2 #6). | Design task: produce the koto-bridge "人" SVG mark + light/dark empty-state line art + a concrete gold-glint (brief shimmer on success icon, suppressed under reduced-motion). |
| P2-18 | Mobile read-only editor sign-off + file CRUD UX on tablet (D2 #7). | Confirm phones are read-only for code (principle #6); design "open on larger screen / ask AI to edit" affordance + tablet new-file/rename/delete beyond context-menu. (needs user sign-off.) |
| P2-19 | First-run / onboarding for the 3-min trial (D2 #10). | Guided empty state (create → upload zip → get URL) + a sample/template project seeded in dev. |
| P2-20 | ProjectCard thumbnail (D2 #12). | **Static placeholder by status** for v1 (sandboxed headless screenshot of arbitrary HTML is a security surface; defer). |
| P2-21 | Path-mode base-href injection (D1 §8.5.1, D6 §6.4). | **Ship it, opt-in, default-on in path mode** (D6 §6.4) with the documented limits (only fixes relative URLs, not root-absolute or JS-constructed). Host mode (default) needs none. |
| P2-22 | git-LFS / large-media policy (D1 §8.5.2). | **Out of scope v1.** Enforce per-file cap + `SITE_QUOTA_BYTES`; document kotoji is for tools, not media. Decide whether `.mp4/.webm/.mp3/.wav` are even in the allowlist (lean: drop media from default allowlist; per-file cap if kept). |
| P2-23 | Top-level CSP `sandbox` directive default (D6 Q3). | **Off** (D6 §6.1: `allow-scripts allow-same-origin` neutralizes it; can break downloads/popups). Per-origin isolation is the real control. Opt-in toggle `TopLevelSandbox`. |
| P2-24 | Range requests + future object-store backend (D6 Q5). | Assumption (local-disk `fs.FS`) documented. `http.ServeContent` handles ranges on local disk. S3 backend would change it; not v1. |
| P2-25 | Compression ownership (D6 Q6). | **NPM owns gzip/br**; data plane does **not** double-compress (sends `Vary: Accept-Encoding` only if it ever does). Document as NPM responsibility. |
| P2-26 | Per-site web-root subdir (D6 Q7). | v1 serves repo root. **Reserve a `sites.web_root TEXT` column now** (default `''`) to avoid a later migration. |
| P2-27 | HTTP→HTTPS + HSTS (D6 Q9). | **NPM's responsibility** (data plane speaks HTTP behind proxy). Confirm the app is never directly exposed; HSTS set at NPM. |
| P2-28 | Atomic-publish failure leaves last-good tree (D6 Q10). | Confirmed contract: build-temp + `rename()` swap → a failed publish leaves previous `served/published/` intact; data plane keeps serving last-good; control plane surfaces the error. Assert in the SiteService integration test. |

---

## 3. Patch list (concrete edits these resolutions require)

To execute the resolutions above, the docs need these edits (do before/alongside coding):

1. **`site-service.md`** — make it the *sole* home of the canonical `site.Service` interface (§1.1): `uuid.UUID` IDs, `Actor` (+`Via`), `*Input` request structs, `GetDiff`/`GetLog`, `FileContent`, `ConflictError{..., ChangedPaths}`, explicit `SetRemote`/`MirrorPush`/`FetchAndUpdate`; remove `ResolveForServing` from the interface (it's the resolver pkg); add `ServedTree` backing `TreeProvider`; merge `ListFiles` signature (§1.14); `HandleMinLen=3`; reject `{handle}--published` (§1.3); rollback ancestor-only (P2-11); add preview eviction policy (P1-10).
2. **`architecture.md`** — patch package `siteservice`→`site`, `SiteRef`/`Author`→`uuid.UUID`/`Actor`, `Diff`/`Log`→`GetDiff`/`GetLog`; `access_tokens`→`site_tokens` with §1.2 columns; `published_sha`→`published_commit_sha` + keep `published_at`; `site_members.role` `admin`→`owner`; `visibility` 2-val→3-val enum (§1.4); `via`→`source` enum (§1.12); `HOSTING_BASE_DOMAIN`→`KOTOJI_BASE_DOMAIN`; `SESSION_COOKIE_DOMAIN` default empty/host-only; add `description`, `deleted_at`, reserve `web_root`; fix MB→MiB; add `ZIP_MAX_ENTRY_BYTES`.
3. **`data-model.md`** — `site_tokens.scope site_role`→`scopes TEXT[]`; `token_prefix` 8→12; add `can_create_sites`, `sites.published_at`, `sites.deleted_at` (soft-delete), `sites.publish_mode`, reserve `sites.web_root`; reconcile delete to soft (§P0-6); align `audit_source` mapping note.
4. **`mcp.md`** — present the `site.Service` subset as "authoritative in site-service.md"; `WriteRequest`→`WriteFileInput` names; `is_published`→derived; `user_id`→`created_by`; `monaco`/`github` `via` → map to `editor`/`system`; conflict fields → `expected`/`actual`/`changedPaths`; add `create_branch` tool (P1-8); binary upload-only note (P1-7).
5. **`routing-and-serving.md`** — confirm host-only session cookie (reject `Domain=.hosting…`, §1.5); add SVG `script-src 'none'` (P1-17); fix companion refs (`identifiers.md`, `data-model.md`); document the `KOTOJI_USERCONTENT_DOMAIN` option (P1-1).
6. **`design.md`** — conflict envelope field names (§1.6); add MCP-token panel + config-endpoint usage (P1-2/P1-5); set i18n = next-intl, ja-only launch (P1-13).
7. **New files owed:** `contracts/openapi.yaml` (P0-3), `contracts/identifiers.md` (handle/branch/reserved + role-capability matrix), and either extract `contracts/mcp-tools.md` or re-point references to `mcp.md`.

---

## 4. Summary — what blocks the first line of code

The **P0** set, in order: **(1) freeze the `site.Service` interface, (2) freeze the
`site_tokens` + `sites` DDL, (3) stand up `openapi.yaml` + the generation/CI pipeline,
(4) write the role-capability matrix, (5) pin the authz boundary, (6) decide soft-delete,
(7) lock the host-only cookie model.** Items 4 and 6 and the toolchain in 3 want a quick
user confirmation; the rest I can finalize from the resolutions above. Everything else
(P1/P2) is sequenced behind the feature it gates and tracked in
[`IMPLEMENTATION-PLAN.md`](../IMPLEMENTATION-PLAN.md).
