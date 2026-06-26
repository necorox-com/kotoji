# Security Policy

kotoji is a self-hosted hosting control plane: it stores credentials (a GitHub
PAT, OIDC client secrets — encrypted at rest with AES-256-GCM), mints per-user
MCP/API tokens, and writes to git on your behalf. We take reports seriously.

## Reporting a vulnerability

**Please do not open a public GitHub issue for security problems.** A public
issue discloses the flaw before a fix exists.

Report privately through one of:

1. **GitHub Security Advisories (preferred).** On the repository, go to the
   **Security** tab → **Report a vulnerability** ("Privately report a security
   vulnerability"). This opens a private advisory thread with the maintainers
   and lets us collaborate on a fix and a CVE if warranted.
2. **Email.** If you cannot use Advisories, email **security@example.com** with
   the details. Replace this with the maintainer contact for your fork or
   instance.

Please include, as far as you can:

- the affected version / commit (`git rev-parse HEAD`) and deployment mode
  (behind your proxy, the Traefik edge overlay, or kotoji-native TLS),
- a description of the issue and its impact (auth bypass, token scope escalation,
  SSRF via the GitHub mirror, path traversal in the data plane, etc.),
- reproduction steps or a proof of concept, and
- any suggested remediation.

### What to expect

- **Acknowledgement** within **3 business days**.
- An initial **assessment** (severity, affected versions) within **7 business
  days**.
- We will keep you updated on remediation progress and coordinate a disclosure
  timeline with you. We aim to ship a fix for high-severity issues within **30
  days** and will credit you in the release notes / advisory unless you prefer to
  remain anonymous.

Please give us a reasonable window to remediate before any public disclosure.

## Supported versions

kotoji is pre-1.0; the API surface is still settling and only the latest line
receives security fixes. Run a recent release.

| Version    | Supported          |
| ---------- | ------------------ |
| `0.1.x`    | :white_check_mark: |
| `< 0.1.0`  | :x:                |

Once a `1.0` line exists this table will be updated to track the supported
releases.

## Audit posture

kotoji has been through several internal hardening passes (see the `security:`
commits in the history): the dependency surface was cut from 11 reachable CVEs to
0, OIDC `state`/`nonce` are server-side single-use, login-state and preview URLs
are HMAC-signed, the zip-upload path is CSRF-protected, the GitHub mirror runs
behind an allowlist, MCP authorization has regression tests, and
`KOTOJI_SECRET_KEY` is **required** in production so stored secrets are never
left unencrypted.

This is best-effort, not a third-party audit. If you operate kotoji on the public
internet:

- always set a strong `KOTOJI_SECRET_KEY` (`openssl rand -hex 32`) and a real
  auth provider (OIDC or the single-admin password) — never the no-auth dev mode,
- keep `KOTOJI_COOKIE_SECURE=true` and terminate TLS at your edge,
- restrict who can publish, and rotate the GitHub PAT / MCP tokens periodically,
- keep the images current (the GHCR release images are rebuilt per tag).

## Single-domain isolation model

kotoji serves hosted sites on subdomains of the **same registrable domain** as
the control plane (one DNS wildcard, e.g. `*.hosting.example.com` with the
control plane at `hosting.example.com`). This is a deliberate operational choice:
one certificate, one wildcard record, one origin to reason about. The trade-off
is that hosted content and the control plane share a parent domain, so the
isolation guarantees below are what make that model safe.

**The `__Host-` guarantee (what is closed).** In production
(`KOTOJI_COOKIE_SECURE=true`, enforced — the server refuses to boot otherwise),
the control session and CSRF cookies carry the `__Host-` prefix. By browser rule
a `__Host-` cookie is **host-only**: it cannot carry a `Domain` attribute and is
keyed to the exact host, so a sibling subdomain (a hosted site) **cannot** set or
overwrite it on the shared parent. Just as important, the control plane **only
reads** the `__Host-`-prefixed cookie names in production — there is no fallback
to the bare names (`kotoji_session`, `kotoji_csrf`). Therefore a hosted site that
"tosses" a bare-name cookie onto the shared parent domain is **invisible** to the
control plane: a tossed cookie cannot shadow a real session, fixate a session id,
or fixate the CSRF double-submit token. None of kotoji's auth/CSRF logic depends
on a parent-domain (`Domain=.example.com`) cookie; every control cookie is
host-only. (In local dev over plain http the bare names are used, because
browsers reject `__Host-` without `Secure` — that path is not internet-exposed.)
These properties are pinned by regression tests in
`backend/internal/auth/cookie_tossing_test.go`.

**Documented residual (what remains).** Cross-*tenant* cookie injection on the
shared parent is inherent to any single-registrable-domain design: a hosted site
can still set bare-name cookies that other hosted sites (or the user's browser)
observe on the shared parent. This does **not** affect the control session/CSRF
(see above); its impact is confined to *hosted-content* behavior and is low —
hosted apps are static and are expected to be **cookie-independent**. Fully
closing cross-tenant cookie injection would require serving hosted content from a
**separate registrable domain** (a distinct "usercontent" domain, so the browser
treats it as a different site for cookie scoping). That split is **deliberately
deferred**: the single-domain model is kept as an operational advantage, and the
`__Host-` guarantee already removes the control-plane attack surface.

**Recommendation for hosted content.** Keep hosted apps cookie-independent: do
not rely on cookies for cross-site state when running under the shared parent
domain, and treat any cookie readable on the parent as untrusted. If you host
mutually-distrusting tenants that must use cookies, run kotoji behind a separate
usercontent domain in front of the data plane.

## Dependency reporting

Routine, non-exploitable dependency-bump notices (e.g. a Dependabot alert with no
reachable path) can be filed as a normal issue or PR. Anything with a plausible
exploit path against a running kotoji instance should go through the private
channel above.
