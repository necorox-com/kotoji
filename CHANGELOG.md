# Changelog

All notable changes to kotoji are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While kotoji is pre-1.0 the API surface may change; breaking changes are called
out under each release.

## [Unreleased]

_No unreleased changes yet._

## [0.1.0] - 2026-06-27

First tagged release — the MVP of MCP-native, self-hosted hosting for AI-built
web tools. The full stack ships and runs; expect rough edges and breaking
changes while the API surface settles.

### Added

- **Upload & serve** — drop a `.zip` of static files and get an instant URL.
- **Per-project subdomains** — `name.hosting.example.com`, resolved from the
  `Host` header by the data plane (works with any asset-path style).
- **Draft vs. published with per-branch previews** — work on `draft`/`feature-*`
  branches, each with its own `name--branch` preview URL, and promote to
  production (`published`) with one action.
- **git as the single source of truth** — every save is a commit; history, diff,
  and rollback come for free. Three writers (zip upload, the Monaco editor, MCP)
  funnel through one Site Service, the only component that touches git.
- **In-browser editing** — Monaco editor with diff view.
- **MCP-native access** — connect Claude (or any MCP client) to
  `list / read / write / publish` your sites, selected by `site` handle.
- **Per-user MCP/API tokens (membership-capped)** — one token belongs to a user
  and covers every project they're a member of; the effective scope per site is
  `token.scopes ∩ role-on-that-site`, re-evaluated per request. Issue, list, and
  revoke on the Settings page.
- **Pluggable auth** — Google OAuth out of the box (OIDC abstraction; bring your
  own IdP), single-admin password mode, or a no-auth dev mode. OIDC supports
  email/domain allowlists and admin promotion, configurable in the Web UI (env
  overrides DB when set).
- **First-run admin setup** — `/auth/setup` screen sets the single-admin password
  (bcrypt-hashed in the DB) instead of baking it into the environment.
- **Instance Settings page** — admin GitHub-mirror panel, per-user token panel,
  and an MCP connection guide on one `/settings` screen.
- **GitHub mirror, configured in the GUI** — enable on Settings (org, PAT,
  webhook secret); the PAT is stored encrypted (AES-256-GCM) at rest. Every
  publish mirror-pushes to GitHub as off-box backup. Env vars can bootstrap it;
  the DB config wins.
- **Boot-time migrations** — embedded, advisory-locked goose migrations run on
  startup (toggle with `KOTOJI_AUTO_MIGRATE`).
- **Three deployment modes** — base proxy-less Compose (bring your own edge); the
  opt-in Traefik turnkey overlay (`docker-compose.edge.yml`) with wildcard
  DNS-01 TLS; and kotoji-native on-demand per-host TLS (`docker-compose.tls.yml`,
  CertMagic, DNS-only).
- **kotoji-tōrō branding** — the lantern as the favicon and in-app brand mark.
- **CI** — backend build/vet/gofmt/test (+ conformance), backend integration
  against Postgres, frontend tsc/lint/build, and a drift gate proving the
  generated artifacts (sqlc, oapi-codegen, openapi-typescript) are current.
- **Prebuilt release images (GHCR)** — multi-arch (`linux/amd64`,
  `linux/arm64`) images for both services (`kotoji-backend`,
  `kotoji-frontend`) published to GHCR on each `v*` git tag, plus a
  `deploy/docker-compose.ghcr.yml` overlay to run the stack from the prebuilt
  images instead of building from source.
- **OSS hygiene** — `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
  issue and pull-request templates, and this changelog.

### Security

- Hardened over several passes: dependency surface cut from 11 reachable CVEs to
  0; OIDC `state`/`nonce` are server-side single-use; login-state and preview
  URLs are HMAC-signed; the zip-upload path is CSRF-protected; the GitHub mirror
  runs behind an allowlist; MCP authorization has regression tests; and
  `KOTOJI_SECRET_KEY` is required in production so stored secrets are encrypted
  at rest.

[Unreleased]: https://github.com/necorox-com/kotoji/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/necorox-com/kotoji/releases/tag/v0.1.0
