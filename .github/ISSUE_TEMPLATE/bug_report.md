---
name: Bug report
about: Report something that isn't working as expected
title: "bug: "
labels: bug
assignees: ""
---

<!--
Before filing: this is NOT for security vulnerabilities. For those, use the
private channel in SECURITY.md (do not open a public issue).
Please search existing issues first.
-->

## What happened

A clear, concise description of the bug.

## Expected behavior

What you expected to happen instead.

## Steps to reproduce

1. ...
2. ...
3. See the error

## Environment

- kotoji version / commit: <!-- a release tag like v0.1.0, or `git rev-parse --short HEAD` -->
- Deployment mode: <!-- behind your proxy / Traefik edge overlay / kotoji-native TLS / local *.localhost -->
- Images: <!-- built from source, or GHCR prebuilt (which tag?) -->
- Host OS & Docker / Compose version:
- Browser (for UI issues):

## Logs / output

<details>
<summary>Relevant logs (redact any secrets, tokens, or real hostnames)</summary>

```
paste here
```

</details>

## Additional context

Screenshots, config snippets (with secrets removed), anything else that helps.
