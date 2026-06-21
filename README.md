# kotoji 🎍

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
- **git is the source of truth** — every save is a commit. History, diff and rollback come for free. Optional mirror-push to GitHub.
- **Draft vs. published** — work safely on branches; promote to production with one action. Each branch gets its own preview URL.
- **Pluggable auth** — Google OAuth out of the box (OIDC abstraction underneath; bring your own IdP). A no-auth dev mode for quick trials.
- **Bring-your-own proxy** — the app resolves projects from the `Host` header, so it runs behind Nginx Proxy Manager, Caddy, nginx, or plain `*.localhost` in dev.

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

```bash
git clone https://github.com/necorox-com/kotoji
cd kotoji
docker compose up
```

Then open `http://kotoji.localhost:8080`. Any `*.localhost` subdomain resolves to `127.0.0.1` automatically — no DNS or TLS setup needed locally.

> Detailed setup, configuration and the MCP connection guide live in [`docs/`](./docs) *(coming soon)*.

## Production (Nginx Proxy Manager + Cloudflare)

One-time setup; after this, new projects need no infra changes.

1. **DNS (Cloudflare):** add an `A` record `*.hosting` → your server IP (DNS-only / grey cloud). DNS wildcards only match one label, so `*.example.com` does **not** cover `*.hosting.example.com` — you need this record.
2. **NPM:** issue a Let's Encrypt **wildcard** cert for `*.hosting.example.com` via the **DNS-01 challenge** (Cloudflare API token). Wildcard certs require DNS validation.
3. **NPM:** proxy `*.hosting.example.com` and `hosting.example.com` to the kotoji app.

## Status

🚧 Early design / pre-alpha. Specification and architecture are locked; implementation is starting.

## License

[AGPL-3.0](./LICENSE) © necorox
