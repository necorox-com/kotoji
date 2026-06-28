# kotoji 🎍

*Read this in [日本語](./README.ja.md).*

> **MCP-native, self-hosted hosting for AI-built web tools.**

Give the web tools you (and your AI) build a home. Drop a folder of HTML/CSS/JS, get a clean URL, edit it in the browser, and let your AI read and write it directly over **MCP** — all on your own server, with **git** quietly versioning every change.

The name comes from the **kotoji-tōrō** lantern of Kenrokuen garden in Kanazawa. *Kotoji* (琴柱) is the bridge that supports the strings of a koto — just as kotoji supports and gives voice to the tools you place on it.

---

## Why kotoji?

AI now lets non-engineers build real, useful web tools in minutes. But *hosting* them is still painful:

- SaaS builders (Val Town, v0, Bolt, Lovable) are proprietary and can't be self-hosted.
- Self-hosted PaaS (Coolify, Dokploy) assume a git-push CI flow, with no in-browser editor and no AI integration.
- Nothing combines **self-hosting + MCP-native AI access + git-as-source-of-truth + a non-engineer-friendly editor** in one place.

kotoji fills that gap.

## Features

- **Upload & serve** — drop a `.zip` of static files, get an instant URL.
- **Per-project subdomains** — `your-tool.hosting.example.com`, works with any asset path style.
- **In-browser editing** — Monaco editor with diff view for quick fixes.
- **MCP-native** — connect Claude (or any MCP client) from your own machine to `list / read / write / publish` your sites directly.
- **Per-user MCP/API tokens that span your projects** — one token belongs to *you* and automatically covers every project you're a member of. The effective scope on each site is `token.scopes ∩ your-role-on-that-site`, re-evaluated per request, so dropping a membership instantly limits the token. Issue, list and revoke them on the Settings page; the MCP tools take a `site` handle selector.
- **git is the source of truth** — every save is a commit. History, diff and rollback come for free.
- **Draft vs. published** — work safely on branches; promote to production with one action. Each branch gets its own per-branch preview URL.
- **GitHub mirror, configured in the GUI** — an admin enables it on the Settings page (org, PAT, webhook secret); the PAT is stored encrypted (AES-256-GCM) at rest. Every publish mirror-pushes to GitHub as off-box backup (env vars can still bootstrap it; the DB config wins).
- **Instance Settings page** — one `/settings` screen for everyone: an admin GitHub-mirror panel, your own MCP/API token panel, and an MCP connection guide.
- **First-run admin setup** — in single-admin (`password`) mode you no longer have to bake a password into the environment; leave it empty and a first-run `/auth/setup` screen sets it (stored as a bcrypt hash in the DB). Password / setup users are promoted to instance admin.
- **Pluggable auth** — Google OAuth out of the box (OIDC abstraction underneath; bring your own IdP), single-admin password mode, or a no-auth dev mode for quick trials.
- **Boot-time migrations** — embedded, advisory-locked goose migrations run automatically on startup (toggle with `KOTOJI_AUTO_MIGRATE`).
- **Two deployment modes** — bring your own proxy (the app resolves projects from the `Host` header, so it runs behind Nginx Proxy Manager, Caddy, nginx, Traefik, or plain `*.localhost` in dev), or add the opt-in Traefik turnkey overlay for a self-contained box with automatic wildcard TLS.
- **kotoji-tōrō branding** — the favicon and brand mark are the kotoji-tōrō lantern as an SVG.

## Architecture

```
                         ┌──── Control plane ─────────────────┐
  Your machine ─MCP/HTTP─▶│  Next.js                          │
  (Claude, ...)          │  · Auth (Google / OIDC)            │
  Browser ───────────────▶│  · Monaco editor / dashboard      │
                         │  · REST API (/upload, ...)         │
                         │  · MCP server                      │
                         │            │  Site Service (DI)    │
                         └────────────┼───────────────────────┘
                                      ▼
                          /data/sites/{uuid}/.git  (1 site = 1 repo)
                            ├ published   ← served in production
                            ├ draft       ← default working branch
                            └ feature-*   ← per-user / AI proposals
                                      │
                         ┌────────────┼──── Data plane ────────┐
                         │            ▼  resolve {name} by Host │
                         │  name.hosting.example.com   → published
                         │  name--branch.hosting.example.com → preview
                         └─────────────────────────────────────┘

  Metadata: PostgreSQL   ·   Deploy: Docker Compose
```

**git is the single source of truth.** Three writers — Zip upload, the Monaco editor, and MCP — all funnel through one **Site Service**, the only component that touches git. That keeps the design testable (mock git at the interface) and makes versioning a side effect rather than a feature.

## Quick start (local)

The base compose is deliberately **proxy-less** (separate `:8080` control,
`:8081` serve and `:3000` UI ports — see [Production](#production)), so the
*dashboard* lives behind a single-origin edge. For a one-URL local run, bring up
the base stack **with the turnkey Traefik overlay** in plain-HTTP mode:

```bash
git clone https://github.com/necorox-com/kotoji
cd kotoji/deploy
cp .env.example .env   # the defaults already target hosting.localhost in HTTP mode
docker compose -f docker-compose.yml -f docker-compose.edge.yml up -d --build
```

Then open **`http://hosting.localhost`** — one origin serves the dashboard,
`/api`, and every published site at `<handle>.hosting.localhost`. Any
`*.localhost` host resolves to `127.0.0.1` automatically, so no DNS or TLS setup
is needed locally. First run shows the admin-password setup screen.

> Why the overlay? The base `docker-compose.yml` alone publishes the API and the
> UI on *separate* ports with no proxy, so the browser dashboard can't reach
> `/api` same-origin — the edge overlay (or your own proxy) is what assembles the
> single origin. See [Production](#production) for the proxy-less mode.

> Detailed setup, configuration and the MCP connection guide live in [`docs/`](./docs); the deployment guide is in [`deploy/README.md`](./deploy/README.md).

## Production

kotoji is a `postgres + backend + frontend` Docker Compose stack. The base
compose is deliberately **proxy-less**, so you choose one of two deployment
modes. Full steps (DNS records, copy-paste proxy configs, env) live in
[`deploy/README.md`](./deploy/README.md).

### (a) Behind your existing proxy

If you already run a shared edge (Nginx Proxy Manager, Caddy, nginx, Traefik…),
use the base compose and point your proxy at the backend (`:8080` control,
`:8081` serve) and frontend (`:3000`):

```bash
docker compose -f deploy/docker-compose.yml up -d
```

Copy-paste NPM and Caddy configs are in [`deploy/npm/README.md`](./deploy/npm/README.md).

### (b) All-in-one (turnkey)

If you want a self-contained box with automatic TLS, add the opt-in Traefik
overlay — it bundles a Traefik v3 edge and issues a wildcard cert for you:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.edge.yml up -d
```

One-time infra; after this, new projects need no changes:

1. **DNS:** add `A` records `your-domain` → server IP **and** `*.your-domain` →
   server IP. DNS wildcards match one label, so `*.your-domain` covers every
   hosted site (`my-tool.your-domain`) but not the bare apex — you need both.
2. **ACME DNS token:** a wildcard cert requires the **DNS-01 challenge**, so set
   `KOTOJI_ACME_EMAIL` + a DNS provider token (e.g. `KOTOJI_CF_DNS_API_TOKEN` for
   Cloudflare) in `deploy/.env`. Traefik then auto-issues the
   `your-domain` + `*.your-domain` wildcard cert.

Leave the ACME vars empty and the overlay serves plain **HTTP** — handy for
`hosting.localhost` and first runs.

### Run prebuilt images (GHCR)

Don't want to build from source? Each `v*` release publishes multi-arch
(`amd64` + `arm64`) images to GHCR. Add the `docker-compose.ghcr.yml` overlay to
swap the `build:` blocks for the published
`ghcr.io/necorox-com/kotoji-backend` / `-frontend` images:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.ghcr.yml up -d
```

It composes with the edge/TLS overlays the same way; pin a version with
`KOTOJI_IMAGE_TAG` (e.g. `0.1.0` — note the git tag is `v0.1.0` but the image
tag drops the `v`; defaults to `latest`).

## Status

✅ Implemented and deployed (MVP). The full stack ships and runs: upload/serve, Monaco
editing, per-branch previews, draft → publish, the MCP server with per-user
membership-capped tokens, GUI GitHub-mirror config, first-run admin setup, boot-time
migrations, and the opt-in Traefik turnkey overlay. Expect rough edges and breaking
changes while the API surface settles.

## Contributing & security

Contributions are welcome — see [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the
build/test/lint and codegen commands and the PR workflow. Found a vulnerability?
Please report it privately via [`SECURITY.md`](./SECURITY.md), not a public
issue.

## License

[AGPL-3.0](./LICENSE) © necorox
