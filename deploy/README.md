# Deploying kotoji

kotoji ships as a small Docker Compose stack: **postgres + backend** (one process
running both planes, `RUN_MODE=all`) **+ frontend**. There are two ways to put it
on the internet, and you pick one — the base compose file is deliberately
proxy-less so it composes with either.

| Mode | When | Command |
|------|------|---------|
| **(a) Behind your existing proxy** | You already run a shared edge (Nginx Proxy Manager, Caddy, nginx, Traefik…) | `docker compose -f docker-compose.yml up -d` |
| **(b) All-in-one (turnkey)** | You want a self-contained box with automatic TLS and nothing else to wire up | `docker compose -f docker-compose.yml -f docker-compose.edge.yml up -d` |

First, always:

```bash
cp deploy/.env.example deploy/.env
# edit deploy/.env
```

The whole routing model is in `docs/architecture.md` §1.1. The short version: the
**Host header** decides everything, and the proxy is "dumb" — it never parses
`{handle}/{branch}`, the backend data plane does.

```
bare host    your-domain            /api /auth /mcp /healthz /readyz → backend :8080 (control)
                                    everything else (/, /_next/…)    → frontend :3000 (UI)
2-level host {anything}.your-domain  all paths                       → backend :8081 (data/serve)
```

A hosted site lives one DNS label below the bare host
(`my-tool.your-domain`), and previews add a `--branch` segment to that label
(`my-tool--draft.your-domain`). Because hosted sites are exactly one level below
the apex, a `*.your-domain` DNS wildcard and a `*.your-domain` TLS wildcard
cover **all** of them — but they do **not** cover the bare apex, which needs its
own record/cert.

---

## (a) Behind your existing proxy

Use the base compose only. The backend exposes `:8080` (control) and `:8081`
(serve) and the frontend exposes `:3000` on the host; point your proxy at them.
Copy-paste configs for **Nginx Proxy Manager** and **Caddy** — including the
path split and the long-lived `/mcp` timeout — live in
[`deploy/npm/README.md`](./npm/README.md).

Set in `.env`:

- `KOTOJI_BASE_DOMAIN` / `KOTOJI_CONTROL_BASE_URL` to your real domain.
- `KOTOJI_TRUST_PROXY_HEADERS=true` (default) so the backend honours
  `X-Forwarded-*` from your edge.
- `KOTOJI_COOKIE_SECURE=true` (default) once you terminate TLS at the edge.

Everything in the **Edge / Traefik** block of `.env` is ignored in this mode.

---

## (b) All-in-one (turnkey) with bundled Traefik

The `docker-compose.edge.yml` **overlay** adds one `traefik` service (Traefik v3)
and merges routing labels onto the unchanged backend/frontend services. It owns
ports 80/443 and routes by Host exactly as the table above describes, with the
hosted-site router at the highest priority so 2-level hosts always hit the serve
plane.

```bash
docker compose -f docker-compose.yml -f docker-compose.edge.yml up -d
```

### Local / dev — HTTP, zero TLS setup

The `.env.example` defaults are HTTP-only, so this works out of the box on
`hosting.localhost` (most browsers resolve `*.localhost` to `127.0.0.1`):

```bash
cp deploy/.env.example deploy/.env
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.edge.yml up -d
# open http://hosting.localhost
```

No certs, no redirect — Traefik serves plain HTTP on `:80`. Flip
`KOTOJI_TRAEFIK_DASHBOARD=true` to expose the dashboard on `:8082` for debugging.

### Production — automatic wildcard TLS (DNS-01)

Two one-time pieces of infra, then new sites need no further changes:

1. **DNS** — at your DNS provider, create:
   - `A  your-domain        → <server IP>`
   - `A  *.your-domain      → <server IP>`  (covers every hosted site + preview)

2. **ACME DNS token** — a wildcard cert **requires** the DNS-01 challenge, so you
   must give Traefik an API token for your DNS provider. With Cloudflare, mint a
   token scoped to **Zone → DNS → Edit** for the zone.

Then edit `deploy/.env`:

```dotenv
KOTOJI_ENV=production
KOTOJI_BASE_DOMAIN=your-domain
KOTOJI_BASE_DOMAIN_REGEX=your-domain      # same value, dots escaped: example\.com
KOTOJI_CONTROL_BASE_URL=https://your-domain
KOTOJI_COOKIE_SECURE=true

# --- turn the HTTPS profile on ---
KOTOJI_ACME_EMAIL=you@your-domain
KOTOJI_ACME_DNS_PROVIDER=cloudflare
KOTOJI_CF_DNS_API_TOKEN=<your scoped token>

KOTOJI_EDGE_ENTRYPOINTS=websecure
KOTOJI_EDGE_TLS=true
KOTOJI_EDGE_CERTRESOLVER=letsencrypt
KOTOJI_EDGE_REDIRECT_TO=websecure
KOTOJI_EDGE_REDIRECT_SCHEME=https
```

Bring it up:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.edge.yml up -d
```

Traefik issues **one wildcard cert** for `your-domain` + `*.your-domain` via
DNS-01, redirects `:80 → :443`, and routes the bare host to the UI/control plane
and every 2-level host to the data plane. Issued certs persist in the
`traefikacme` volume across restarts.

> **Why the `_REGEX` twin?** The hosted-site router matches with a Go regexp, so
> the dots in your domain must be escaped there (`example.com → example\.com`).
> It is the only value you have to write twice — keep it in sync with
> `KOTOJI_BASE_DOMAIN`.

### Other DNS providers

`KOTOJI_ACME_DNS_PROVIDER` accepts any
[lego DNS provider code](https://go-acme.github.io/lego/dns/) (e.g. `route53`,
`gcloud`, `digitalocean`). The overlay passes the Cloudflare token env by name;
for another provider, set that provider's required env var(s) on the `traefik`
service via your own `.env`/override and set `KOTOJI_CF_DNS_API_TOKEN` empty.

---

## Files in this directory

| File | Purpose |
|------|---------|
| `docker-compose.yml` | Base stack (postgres + backend + frontend). Proxy-less. |
| `docker-compose.edge.yml` | **Opt-in** overlay: bundled Traefik edge + TLS. |
| `docker-compose.dev.yml` | Dev overlay: Adminer + no-auth + insecure cookies. |
| `backend.Dockerfile` / `frontend.Dockerfile` | Multi-stage build images. |
| `.env.example` | Annotated env template. Copy to `.env`. |
| `npm/README.md` | Nginx Proxy Manager / Caddy configs for mode (a). |

## Health checks

- `GET /healthz` — liveness (always 200 if the process is up).
- `GET /readyz` — readiness (pings Postgres).

Both are on the control plane (`:8080`, routed under the bare host).
