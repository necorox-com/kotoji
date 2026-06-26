# kotoji docs

> **MCP-native, self-hosted hosting for AI-built web tools.**
> This directory is the implementation source of truth. kotoji is now an
> **implemented, deployed MVP**: read these before changing code, and update the
> doc *before* the code when a decision changes. On any conflict,
> [`contracts/CANONICAL.md`](./contracts/CANONICAL.md) and
> [`contracts/openapi.yaml`](./contracts/openapi.yaml) win.

kotoji has two planes (a Go **control plane** = REST API + auth + MCP, plus a Next.js
UI; and a Go **data plane** = read-only static serving resolved from the `Host` header)
and one DI seam (**SiteService**, the only component that touches git). These docs
specify each piece concretely — exact Go signatures, SQL DDL, URL grammar, token model,
and the frontend atomic-design system.

## Reading order

Read top-to-bottom the first time; thereafter use as a lookup.

1. [`architecture.md`](./architecture.md) — **start here.** System diagram, the two planes, monorepo layout, request flows (upload / save / MCP / publish / serve / webhook / login), Docker Compose topology, the full env matrix, the Go↔Next OpenAPI contract strategy, and the canonical SQL DDL subset. The big-picture blueprint everything else refines.
2. [`contracts/data-model.md`](./contracts/data-model.md) — the authoritative PostgreSQL schema (goose DDL + sqlc queries). "git is authoritative, the DB is a directory" — what lives in git vs Postgres, every table, FK behavior, handle validation, and the reserved-word source of truth.
3. [`contracts/site-service.md`](./contracts/site-service.md) — the single git boundary: the `SiteService` Go interface, domain structs, optimistic-locking semantics, the git-level publish algorithm, ImportZip security guards, per-repo concurrency, the error taxonomy, and the table-driven test plan. **The DI seam all three writers funnel through.**
4. [`contracts/mcp.md`](./contracts/mcp.md) — the MCP server: Streamable-HTTP transport, per-user membership-capped bearer tokens (a token is owned by a user and spans all their projects; tools take a `site` handle selector), scope enforcement (effective scope = `token.scopes ∩ role scopes`, re-evaluated per request), the 10-tool catalogue with full I/O schemas, the error model, limits/rate-limiting, client config, and the test matrix. A thin authenticated adapter over `SiteService`.
5. [`contracts/routing-and-serving.md`](./contracts/routing-and-serving.md) — the data plane: the `{handle}--{branch}` Host/path resolver, static-serving rules (index/MIME/404), the full security-header policy + CSP trade-offs, caching, base-href fallback, the published-tree materialization strategy, and preview/draft authorization.
6. [`design.md`](./design.md) — the frontend: brand, design tokens (OKLCH + hex), the full atomic-design inventory (atoms→molecules→organisms→templates→pages), wireframes for every screen, responsive bands, the TanStack-Query data layer, theming, and the WCAG-AA a11y checklist.

## Cross-cutting

- [`contracts/CANONICAL.md`](./contracts/CANONICAL.md) — **the law.** The frozen `site.Service` interface, domain structs, error taxonomy, the complete PostgreSQL DDL, identifier/handle/branch rules, the role → capability matrix, and the per-user token-scope capping model. On any conflict with the other docs, this file wins.
- [`contracts/openapi.yaml`](./contracts/openapi.yaml) — the REST API source of truth (Go ↔ Next codegen). Includes the per-user `/api/tokens` endpoints.
- [`contracts/consistency-report.md`](./contracts/consistency-report.md) — **read before implementing.** Cross-document consistency review: every naming/type/grammar/auth divergence between the docs above, with the **canonical resolution** for each, plus the consolidated, prioritized (P0/P1/P2) list of all open questions / 考慮漏れ and recommended resolutions.
- [`IMPLEMENTATION-PLAN.md`](./IMPLEMENTATION-PLAN.md) — the concrete build plan: exact monorepo folder structure, Go module + npm dependency lists, and the phased task breakdown (scaffold → migrations → SiteService → auth → REST/upload → data plane → MCP → frontend) noting what parallelizes and which decisions to confirm before coding.
- [`operations.md`](./operations.md) — **Day-2 ops.** Backup/restore (`deploy/backup.sh` + `deploy/restore.sh`), upgrade (forward-only advisory-locked migrations → safe rolling restart; preserve `KOTOJI_SECRET_KEY`), health (`/healthz` + `/readyz`) & structured slog logs, and disaster recovery (re-clone from the GitHub mirror when enabled). Pairs with [`deploy/README.md`](../deploy/README.md) (first-time deploy + the three TLS/edge modes).

## Status of referenced-but-not-yet-written artifacts

The REST API source of truth, [`contracts/openapi.yaml`](./contracts/openapi.yaml),
is now authored and live. A couple of editorial split-outs remain optional (their
content already lives in the docs above):

- `contracts/mcp-tools.md` — MCP tool JSON schemas as a standalone artifact (currently inlined in `mcp.md`).
- `contracts/identifiers.md` — handle/branch validation contract (currently the authoritative copy is `CANONICAL.md` §5; restated in several docs).

> Note: several docs cross-reference each other under slightly different filenames
> (`siteservice.md`, `db-schema.md`, `db.md`, `api.md`, `responsive-design.md`). The
> canonical filenames are the ones listed above; see the consistency report for the
> full alias map.
