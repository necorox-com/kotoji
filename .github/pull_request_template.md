<!--
Thanks for contributing to kotoji! Please read CONTRIBUTING.md.
Keep the title in Conventional Commit form, e.g. `feat(auth): ...`, `fix: ...`.
-->

## What & why

<!-- What does this change do, and what problem does it solve? Link issues with
"Closes #123". -->

## How tested

<!-- The exact commands you ran. See CONTRIBUTING.md for the full list. -->

- [ ] `go build ./...` / `go vet ./...` / `gofmt -l .` (clean)
- [ ] `go test ./...` (+ `-tags conformance`, and `-tags=integration` if DB-related)
- [ ] `npx tsc --noEmit` / `npm run lint` / `npm run build`
- [ ] `make gen` re-run and committed (if I touched the OpenAPI spec, sqlc queries, or migrations)
- [ ] Manually verified locally (`make up` / overlay) where relevant

## Checklist

- [ ] Conventional Commit title/messages
- [ ] Docs updated (`docs/`, `README*`, `deploy/README.md`) if user-visible
- [ ] `CHANGELOG.md` **Unreleased** updated if user-visible
- [ ] No secrets, real IPs/hostnames, or org-specific values added (necorox-agnostic; use `example.com`)
- [ ] This is not a security vulnerability (those go through SECURITY.md privately)

## Notes for reviewers

<!-- Anything that needs special attention, follow-ups, or known limitations. -->
