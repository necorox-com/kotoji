# kotoji E2E (Playwright)

End-to-end browser tests that prove kotoji's **core user journey** against a
**real composed stack**:

1. first-run admin setup / password sign-in → authenticated dashboard
2. create a site (seeded *From a zip*) → ProjectDetail, file in the tree
3. publish the draft → published state
4. the published content is served on its subdomain (`<handle>.<base domain>`)
5. issue a per-user MCP / API token on `/settings` (show-once secret)

## Why the edge overlay, in HTTP mode

The base compose (`deploy/docker-compose.yml`) is **proxy-less**: it publishes
backend `:8080` (control API/auth/MCP), backend `:8081` (serve/data plane) and
frontend `:3000` on **separate** ports with no single origin, so the browser
dashboard cannot reach `/api` out of the box.

These tests need a **single origin** where the dashboard, `/api`, `/auth` and the
served subdomains all share one host. We get that by layering the **edge overlay**
(`deploy/docker-compose.edge.yml`) — a Traefik v3 service — **in HTTP mode**
(`KOTOJI_EDGE_TLS=false`, entrypoint `web`), with `KOTOJI_BASE_DOMAIN=hosting.localhost`.
On that origin:

- `http://hosting.localhost` → frontend (dashboard) + `/api` `/auth` `/mcp` (control plane)
- `http://<handle>.hosting.localhost` → serve plane (published sites)

Chromium resolves `*.localhost` to loopback automatically (RFC 6761), so the
served-subdomain step needs no `/etc/hosts` entry inside the browser.

> We deliberately do **not** use the two-origin dev path (`NEXT_PUBLIC_API_BASE_URL`
> pointing at the backend). Those `NEXT_PUBLIC_*` values are inlined at **build**
> time by Next, and the frontend image is built with them empty (same-origin) —
> so the single-origin edge is the path that works against the committed images
> without rebuilding. See `deploy/frontend.Dockerfile`.

## Auth: real first-run, deterministic password

The stack is started in **password** mode **without** an env admin password, so the
**first-run setup screen** appears. The test sets the admin password itself (real
first-run journey), then signs in with it. The helper is idempotent: on a re-run
against a persisted DB it falls back to the break-glass sign-in form with the same
password.

## Run it

### 1. Bring up the single-origin stack (HTTP edge, test env)

From the repo root:

```bash
docker compose \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.edge.yml \
  --env-file e2e/.env.e2e \
  up -d --build
```

`e2e/.env.e2e` (committed, secret-free) pins the test env: development env,
HTTP edge, `hosting.localhost`, password mode with no env admin password,
insecure cookies, a throwaway `KOTOJI_SECRET_KEY`.

Wait for health (the edge proxies `/healthz` on the control plane):

```bash
until curl -fsS -H 'Host: hosting.localhost' http://127.0.0.1/healthz >/dev/null; do
  sleep 2
done
```

### 2. Install browsers + run

```bash
cd e2e
npm ci
npx playwright install --with-deps chromium
npm test
```

### 3. Tear down

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.edge.yml down -v
```

## Configuration (env)

| Var                        | Default                    | Meaning                                              |
| -------------------------- | -------------------------- | ---------------------------------------------------- |
| `KOTOJI_E2E_BASE_URL`      | `http://hosting.localhost` | Single-origin control host the suite drives          |
| `KOTOJI_E2E_BASE_DOMAIN`   | `hosting.localhost`        | Base domain for served subdomains (`KOTOJI_BASE_DOMAIN`) |
| `KOTOJI_E2E_ADMIN_PASSWORD`| `e2e-admin-pass-123`       | Admin password the first-run setup sets / signs in with |

Point the suite at any already-running single-origin box by overriding
`KOTOJI_E2E_BASE_URL` (and the matching base domain).
