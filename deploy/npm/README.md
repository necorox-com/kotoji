# Reverse-proxy configuration (Nginx Proxy Manager / Caddy)

kotoji needs exactly **two** proxy hosts, configured once and never per-site. The
proxy is "dumb": it routes by `Host` only and holds no authority over
`{handle}/{branch}` resolution — the backend data plane re-parses the `Host`
header itself. See `docs/architecture.md` §1.1 for the authoritative routing
table.

```
bare host   hosting.example.com    → path split: /api,/auth,/mcp,/healthz,/readyz → backend :8080
                                                  everything else (/, /_next/…)    → frontend :3000
wildcard    *.hosting.example.com  → single upstream: backend data plane :8081
```

Get a wildcard TLS cert for `*.hosting.example.com` **plus** a cert for the bare
`hosting.example.com` (a wildcard does not cover the apex label).

Always pass `Host` through unchanged (`proxy_set_header Host $host`) so the data
plane can resolve `{handle}--{branch}`.

---

## Nginx Proxy Manager (NPM)

NPM is GUI-driven; create two Proxy Hosts.

### A. Bare control host — `hosting.example.com`

- **Forward Hostname/Port:** `frontend` / `3000` (the default upstream).
- **Websockets Support:** on.
- **SSL:** request/assign the cert for `hosting.example.com`; Force SSL + HTTP/2.
- **Custom Nginx Configuration** (Advanced tab) — route the backend path prefixes
  to `:8080` and keep `/mcp` long-lived:

  ```nginx
  location /api/   { proxy_pass http://backend:8080; include /etc/nginx/proxy_params; }
  location /auth/  { proxy_pass http://backend:8080; include /etc/nginx/proxy_params; }
  location /healthz { proxy_pass http://backend:8080; }
  location /readyz  { proxy_pass http://backend:8080; }

  # MCP is Streamable HTTP / long-lived: disable buffering, 7-day read timeout.
  location /mcp {
      proxy_pass http://backend:8080;
      proxy_http_version 1.1;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection "upgrade";
      proxy_buffering off;
      proxy_read_timeout 7d;
      proxy_send_timeout 7d;
  }
  # Everything else falls through to the GUI default upstream (frontend:3000).
  ```

### B. Wildcard host — `*.hosting.example.com`

- **Domain Names:** `*.hosting.example.com`.
- **Forward Hostname/Port:** `backend` / `8081` (the data plane).
- **SSL:** assign the `*.hosting.example.com` wildcard cert; Force SSL.
- Nothing else — a single upstream for every per-site / preview subdomain.

---

## Caddy

`Caddyfile` equivalent (Caddy auto-provisions TLS, incl. the wildcard via a
DNS-01 challenge — configure your DNS provider plugin):

```caddyfile
# A. Bare control host: path split between backend and frontend.
hosting.example.com {
    @backend path /api/* /auth/* /mcp* /healthz /readyz
    handle @backend {
        reverse_proxy backend:8080 {
            # Keep /mcp long-lived (Streamable HTTP).
            transport http {
                read_timeout 7d
                write_timeout 7d
            }
        }
    }
    handle {
        reverse_proxy frontend:3000
    }
}

# B. Wildcard: every site + preview subdomain → data plane.
*.hosting.example.com {
    # DNS-01 needed for the wildcard cert (configure your provider's plugin).
    reverse_proxy backend:8081
}
```

---

## Dev notes

- Local dev typically skips the proxy: hit the backend directly on `:8080`
  (control) and `:8081` (serve), and the frontend on `:3000`. Wildcard
  subdomains on `hosting.localhost` resolve to `127.0.0.1` in most browsers, or
  add explicit `/etc/hosts` entries (e.g. `expense-calc.hosting.localhost`).
- `docker-compose.dev.yml` also exposes Adminer on `:8090` for the database.
- The data plane never sets cookies and the control session cookie is scoped to
  the bare host only (no leading dot) — do NOT host the control UI on a
  `*.hosting…` subdomain (see `docs/architecture.md` §8.1).
