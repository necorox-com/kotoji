# Operations (Day-2)

> Running kotoji in production: **backup, restore, upgrade, health/logs, and
> disaster recovery.** This complements [`deploy/README.md`](../deploy/README.md)
> (which covers first-time deployment + the three edge/TLS modes). Examples use
> `example.com` as the base domain — substitute your own.

kotoji has exactly **two pieces of durable state**, both held in Docker named
volumes by the base [`deploy/docker-compose.yml`](../deploy/docker-compose.yml):

| What | Volume (compose project `kotoji`) | Mounted at | Contents |
|------|-----------------------------------|------------|----------|
| **Site data** (critical) | `kotoji_kotojidata` | `/data` (`KOTOJI_DATA_DIR`) | `sites/<uuid>/.git` (per-site bare-ish repo), `sites/<uuid>/served/` (materialized read-only worktrees), `certmagic/` (issued TLS certs/keys in native-TLS mode), `backups/` (git bundles from the soft-delete reaper) |
| **Metadata** | `kotoji_pgdata` | `/var/lib/postgresql` | Postgres: users, sessions, site index, tokens, memberships, redirects |

**git is the source of truth.** Every hosted site is a git repository under
`/data/sites/<uuid>/`. Postgres is a *directory/index* over those repos
(handle → uuid, ownership, published SHA, tokens). If you could keep only one
artifact, keep the **`kotoji_kotojidata` volume** — Postgres rows largely
re-derive from it, but the repos cannot be reconstructed from Postgres.

> The compose project name is `kotoji` (the `name:` field in
> `docker-compose.yml`), so Docker prefixes the volumes as `kotoji_kotojidata`
> and `kotoji_pgdata`. If you run with a different `-p`, adjust the names (the
> backup/restore scripts derive them from `-p`).

---

## Backup

Use [`deploy/backup.sh`](../deploy/backup.sh). It is read-only against the running
stack (it never stops containers, never deletes), and writes a timestamped
snapshot containing both artifacts plus a manifest:

```bash
# from the repo root; writes to deploy/backups/<UTC timestamp>/
deploy/backup.sh

# or point it somewhere durable (a mounted backup disk, etc.)
deploy/backup.sh -d /srv/kotoji-backups
```

Each run produces `deploy/backups/<YYYYMMDD_HHMMSS>/` with:

- `kotoji-db-<stamp>.dump` — `pg_dump -Fc` (custom format) of the kotoji DB, taken
  via `docker compose exec -T postgres pg_dump`.
- `kotoji-data-<stamp>.tar.gz` — a `tar.gz` of the entire `/data` volume, taken by
  a throwaway `alpine` helper that mounts `kotoji_kotojidata` **read-only**. The
  per-repo writer is advisory-locked, so an online tar is crash-consistent.
- `MANIFEST.txt` — records the DB name, volume name, and filenames so
  `restore.sh` can put everything back without guessing.

Flags (all optional): `-d BACKUP_DIR`, `-f COMPOSE_FILE`, `-p PROJECT`,
`-e ENV_FILE`. Defaults match the base compose (`-p kotoji`,
`-f deploy/docker-compose.yml`, `-e deploy/.env`). DB credentials are read from
the env file (`POSTGRES_USER` / `POSTGRES_DB`), falling back to the compose
defaults.

**Schedule it.** A nightly cron is enough for most internal deployments:

```cron
# 03:17 UTC daily; keep backups on a separate disk and prune old ones yourself.
17 3 * * *  cd /opt/kotoji && deploy/backup.sh -d /srv/kotoji-backups >> /var/log/kotoji-backup.log 2>&1
```

**Off-box copies.** Whatever directory you back up into, replicate it off the host
(object storage, a second disk). If `KOTOJI_GITHUB_MIRROR_ENABLED=true`, each
site's `published` branch is also pushed to your GitHub org — an *additional*
off-box copy of the repo contents (see [Disaster recovery](#disaster-recovery)),
but it does **not** include Postgres metadata or unpublished branches, so it is a
supplement to `backup.sh`, not a replacement.

---

## Restore

Use [`deploy/restore.sh`](../deploy/restore.sh). It is the inverse of `backup.sh`
and is **destructive** — it overwrites the live DB and data volume — so it is
guarded by a `yes` confirmation (or `-y` for non-interactive use) and does nothing
by default.

```bash
# list available snapshots (safe; touches nothing)
deploy/restore.sh

# restore a specific snapshot into fresh/blank containers
deploy/restore.sh -s deploy/backups/20260101_031700
```

What it does, in order:

1. Stops `backend` + `frontend` (best effort) so nothing reads a half-written volume.
2. Wipes and re-extracts the `kotoji_kotojidata` volume from the tar (a throwaway
   `alpine` helper mounting the volume RW + the tar read-only).
3. Brings up `postgres`, waits for `pg_isready`, then
   `pg_restore --clean --if-exists --no-owner` into the kotoji DB.
4. `docker compose up -d` to start everything.

**Migrations self-heal.** On boot the backend runs the embedded, **forward-only**,
advisory-locked goose migrations (`internal/migrate`). So restoring an *older*
dump is safe: the next boot brings the schema current automatically — no manual
`migrate` step.

> **Preserve `KOTOJI_SECRET_KEY`.** Secrets stored in Postgres (the GitHub mirror
> PAT, the OIDC client secret) are encrypted at rest with `KOTOJI_SECRET_KEY`
> (AES-256-GCM). Restore the DB with the **same** key that was set when the backup
> was taken, or those secrets become undecryptable and must be re-entered in the
> UI. Keep `KOTOJI_SECRET_KEY` in your secret manager, not only in `.env`.

Restoring onto a fresh host: install Docker, `git clone` kotoji, copy your saved
`deploy/.env` (with the original `KOTOJI_SECRET_KEY`), drop the snapshot under
`deploy/backups/`, then run `restore.sh -s …`.

---

## Upgrade

Migrations are forward-only and advisory-locked, so a rolling restart is safe.

```bash
# 1. (recommended) take a backup first
deploy/backup.sh -d /srv/kotoji-backups

# 2. pull the new code, rebuild images, recreate containers
git pull
docker compose -f deploy/docker-compose.yml up -d --build
```

On recreate, `postgres` stays up (the `pgdata` volume persists), `backend`
re-applies any pending migrations under the advisory lock (concurrent/old boots
block rather than race), and `frontend` rolls. The data volume
(`kotoji_kotojidata`) is untouched.

**Carry `KOTOJI_SECRET_KEY` across upgrades.** It is the at-rest encryption key —
if it changes between versions, stored secrets become undecryptable. The same
applies to any TLS/edge overlay you compose on top; bring it up with the matching
overlay command from `deploy/README.md` (e.g. add `-f deploy/docker-compose.tls.yml`).

**Rollback.** If an upgrade misbehaves, redeploy the previous image tag/commit.
Because migrations are forward-only, a schema that advanced will not auto-revert;
restore the pre-upgrade snapshot with `restore.sh` if you need the old schema back.

---

## Health & logs

Both probes live on the **control plane** (`:8080`, routed under the bare host):

| Endpoint | Meaning |
|----------|---------|
| `GET /healthz` | **Liveness** — 200 whenever the process is up. (Used by the compose healthcheck.) |
| `GET /readyz`  | **Readiness** — 200 only when Postgres is reachable; 503 otherwise. |

```bash
curl -fsS http://localhost:8080/healthz && echo "  live"
curl -fsS http://localhost:8080/readyz  && echo "  ready"
# through your edge, same probes on the bare host:
curl -fsS https://example.com/healthz
```

Logs are **structured slog** (JSON by default, `KOTOJI_LOG_FORMAT`; level via
`KOTOJI_LOG_LEVEL`):

```bash
docker compose -f deploy/docker-compose.yml logs -f backend
docker compose -f deploy/docker-compose.yml logs -f postgres frontend
docker compose -f deploy/docker-compose.yml ps   # container + healthcheck status
```

Watch the boot lines `database migrations applied` / `database schema up to date`
to confirm migrations ran.

---

## Disaster recovery

In order of preference:

1. **Restore from a `backup.sh` snapshot** (above) — the complete, fastest path
   (repos + metadata + certs together).
2. **Postgres lost, data volume intact** — restore only the DB dump (step 3 of
   `restore.sh`, or `pg_restore` by hand). The repos in `kotoji_kotojidata` are
   the source of truth; the index re-points at them.
3. **Data volume lost AND the GitHub mirror is enabled** — worst case. If
   `KOTOJI_GITHUB_MIRROR_ENABLED=true`, each site's `published` branch was mirrored
   to `https://github.com/<KOTOJI_GITHUB_ORG>/<repo>`. Re-clone those repos to
   reconstruct site contents. Note this recovers **published content only** — not
   unpublished `draft`/preview branches, not Postgres metadata, and not issued TLS
   certs (which simply re-issue on demand). Treat the mirror as a last-resort
   off-box copy, and keep `backup.sh` snapshots as the primary recovery path.

> TLS certs are disposable: in native-TLS mode (`docker-compose.tls.yml`) CertMagic
> re-issues per-host certs on demand after a `certmagic/` loss; in the Traefik edge
> overlay the wildcard cert re-issues into `traefikacme`. No cert backup is required
> for correctness — only to avoid a brief re-issuance delay.
