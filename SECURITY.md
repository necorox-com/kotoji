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

## Dependency reporting

Routine, non-exploitable dependency-bump notices (e.g. a Dependabot alert with no
reachable path) can be filed as a normal issue or PR. Anything with a plausible
exploit path against a running kotoji instance should go through the private
channel above.
