#!/usr/bin/env sh
# kotoji backup — captures the two stateful artifacts of a running stack:
#   1. the Postgres metadata DB  (pg_dump, custom format)
#   2. the data volume           (per-site git repos + served worktrees + certmagic)
#
# The data volume is the CRITICAL artifact: git is the source of truth for every
# hosted site, so its loss cannot be reconstructed from Postgres alone. The DB is
# a directory/index over those repos. (If the GitHub mirror is enabled, that is an
# additional off-box copy — see docs/operations.md.)
#
# Usage:
#   deploy/backup.sh [-d BACKUP_DIR] [-f COMPOSE_FILE] [-p PROJECT] [-e ENV_FILE]
#
# Safe by design: read-only against the running stack (pg_dump + a throwaway
# read-only tar helper). It never stops containers and never deletes anything.
#
# POSIX sh, no bashisms.
set -eu

# --- resolve script + repo locations (so it works from any CWD) --------------
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# --- defaults (override via flags or KOTOJI_* env) ---------------------------
# Compose project name — must match `name:` in docker-compose.yml so the volume
# names resolve (compose prefixes volumes with the project name).
PROJECT="${KOTOJI_PROJECT:-kotoji}"
COMPOSE_FILE="${KOTOJI_COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.yml}"
ENV_FILE="${KOTOJI_ENV_FILE:-$SCRIPT_DIR/.env}"
BACKUP_DIR="${KOTOJI_BACKUP_DIR:-$SCRIPT_DIR/backups}"

# Volume + service names. These mirror docker-compose.yml; change only if you
# renamed them there. The data volume is mounted at /data inside the backend.
DATA_VOLUME_SUFFIX="kotojidata"
PG_SERVICE="postgres"

usage() {
	cat >&2 <<EOF
Usage: $0 [-d BACKUP_DIR] [-f COMPOSE_FILE] [-p PROJECT] [-e ENV_FILE]
  -d  Output directory for backup artifacts (default: $BACKUP_DIR)
  -f  Compose file                          (default: $COMPOSE_FILE)
  -p  Compose project name                  (default: $PROJECT)
  -e  Env file passed to compose            (default: $ENV_FILE)
EOF
	exit 2
}

while getopts ':d:f:p:e:h' opt; do
	case "$opt" in
	d) BACKUP_DIR="$OPTARG" ;;
	f) COMPOSE_FILE="$OPTARG" ;;
	p) PROJECT="$OPTARG" ;;
	e) ENV_FILE="$OPTARG" ;;
	h) usage ;;
	*) usage ;;
	esac
done

# --- preflight ---------------------------------------------------------------
command -v docker >/dev/null 2>&1 || { echo "error: docker not found in PATH" >&2; exit 1; }

# Build the compose invocation once. The env file is optional (compose still runs
# without it); only pass --env-file when the file actually exists.
set -- compose -p "$PROJECT" -f "$COMPOSE_FILE"
if [ -f "$ENV_FILE" ]; then
	set -- "$@" --env-file "$ENV_FILE"
fi
COMPOSE="docker $*"

# Read DB credentials from the env file when present, else fall back to the same
# compose defaults as docker-compose.yml. We avoid sourcing the env file blindly
# (it may contain values with spaces/quotes); grep the specific keys instead.
read_env() {
	# $1 = key, $2 = default. Last assignment wins; strip optional surrounding quotes.
	_val=""
	if [ -f "$ENV_FILE" ]; then
		_val=$(grep -E "^${1}=" "$ENV_FILE" 2>/dev/null | tail -n1 | cut -d= -f2- || true)
		_val=$(printf '%s' "$_val" | sed -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'$//")
	fi
	[ -n "$_val" ] && printf '%s' "$_val" || printf '%s' "$2"
}

PG_USER=$(read_env POSTGRES_USER kotoji)
PG_DB=$(read_env POSTGRES_DB kotoji)

DATA_VOLUME="${PROJECT}_${DATA_VOLUME_SUFFIX}"
STAMP=$(date -u +%Y%m%d_%H%M%S)
OUT_DIR="$BACKUP_DIR/$STAMP"
mkdir -p "$OUT_DIR"

echo ">> kotoji backup -> $OUT_DIR"
echo "   project=$PROJECT  db=$PG_DB  volume=$DATA_VOLUME"

# --- 1) Postgres dump --------------------------------------------------------
# Custom format (-Fc): compressed + restorable with pg_restore (parallel, selective).
DB_DUMP="$OUT_DIR/kotoji-db-${STAMP}.dump"
echo ">> dumping Postgres ($PG_DB) ..."
# -T disables docker's pseudo-TTY so the binary dump streams cleanly to a file.
$COMPOSE exec -T "$PG_SERVICE" pg_dump -U "$PG_USER" -d "$PG_DB" -Fc >"$DB_DUMP"
echo "   wrote $DB_DUMP ($(wc -c <"$DB_DUMP") bytes)"

# --- 2) Data volume tar ------------------------------------------------------
# A throwaway alpine container mounts the named volume read-only and tars it to
# stdout, which we capture on the host. No dependency on the backend image and no
# need to stop the running stack — git repos under /data are crash-consistent
# (the writer is advisory-locked per repo), so an online tar is a sound snapshot.
DATA_TAR="$OUT_DIR/kotoji-data-${STAMP}.tar.gz"
echo ">> archiving data volume ($DATA_VOLUME) ..."
docker run --rm \
	-v "${DATA_VOLUME}:/data:ro" \
	alpine:3 \
	tar -czf - -C /data . >"$DATA_TAR"
echo "   wrote $DATA_TAR ($(wc -c <"$DATA_TAR") bytes)"

# --- manifest ----------------------------------------------------------------
# A tiny manifest records exactly what restore.sh needs to put things back.
cat >"$OUT_DIR/MANIFEST.txt" <<EOF
kotoji backup
created_utc=$STAMP
project=$PROJECT
db_dump=$(basename "$DB_DUMP")
data_tar=$(basename "$DATA_TAR")
postgres_db=$PG_DB
postgres_user=$PG_USER
data_volume=$DATA_VOLUME
EOF

echo ">> done. Artifacts in $OUT_DIR:"
ls -la "$OUT_DIR"
