#!/usr/bin/env sh
# kotoji restore — the inverse of backup.sh. Restores BOTH artifacts into the
# running stack:
#   1. the Postgres metadata DB  (pg_restore --clean into the existing DB)
#   2. the data volume           (extract the tar over the named volume)
#
# DESTRUCTIVE: this OVERWRITES the live database and the live data volume. It is
# guarded behind an explicit confirmation (type `yes`) or the -y flag, and does
# nothing by default beyond printing what it would do.
#
# Schema self-heals: the backend runs forward-only, advisory-locked goose
# migrations on boot (internal/migrate), so after restoring an older dump the
# next `up` brings the schema current automatically. KOTOJI_SECRET_KEY in your
# .env MUST match the one used when the backup was taken, or stored secrets
# (GitHub PAT / OIDC client secret) become undecryptable.
#
# Usage:
#   deploy/restore.sh -s SNAPSHOT_DIR [-f COMPOSE_FILE] [-p PROJECT] [-e ENV_FILE] [-y]
#   deploy/restore.sh                       # no -s => lists available snapshots
#
# POSIX sh, no bashisms.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

PROJECT="${KOTOJI_PROJECT:-kotoji}"
COMPOSE_FILE="${KOTOJI_COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.yml}"
ENV_FILE="${KOTOJI_ENV_FILE:-$SCRIPT_DIR/.env}"
BACKUP_DIR="${KOTOJI_BACKUP_DIR:-$SCRIPT_DIR/backups}"

DATA_VOLUME_SUFFIX="kotojidata"
PG_SERVICE="postgres"

SNAPSHOT=""
ASSUME_YES=0

usage() {
	cat >&2 <<EOF
Usage: $0 -s SNAPSHOT_DIR [-f COMPOSE_FILE] [-p PROJECT] [-e ENV_FILE] [-y]
  -s  Snapshot directory produced by backup.sh (e.g. $BACKUP_DIR/20260101_000000)
  -f  Compose file              (default: $COMPOSE_FILE)
  -p  Compose project name      (default: $PROJECT)
  -e  Env file passed to compose(default: $ENV_FILE)
  -y  Skip the confirmation prompt (non-interactive)

With no -s, lists the snapshots found under $BACKUP_DIR.
EOF
	exit 2
}

while getopts ':s:f:p:e:yh' opt; do
	case "$opt" in
	s) SNAPSHOT="$OPTARG" ;;
	f) COMPOSE_FILE="$OPTARG" ;;
	p) PROJECT="$OPTARG" ;;
	e) ENV_FILE="$OPTARG" ;;
	y) ASSUME_YES=1 ;;
	h) usage ;;
	*) usage ;;
	esac
done

command -v docker >/dev/null 2>&1 || { echo "error: docker not found in PATH" >&2; exit 1; }

# No snapshot chosen: help the operator by listing what's available, then exit
# WITHOUT touching anything (safe default).
if [ -z "$SNAPSHOT" ]; then
	echo "No -s SNAPSHOT_DIR given. Available snapshots under $BACKUP_DIR:" >&2
	if [ -d "$BACKUP_DIR" ]; then
		ls -1 "$BACKUP_DIR" >&2 || true
	else
		echo "  (none — $BACKUP_DIR does not exist)" >&2
	fi
	usage
fi

[ -d "$SNAPSHOT" ] || { echo "error: snapshot dir not found: $SNAPSHOT" >&2; exit 1; }

# Resolve the artifact files. Prefer the MANIFEST written by backup.sh; fall back
# to globbing so a hand-assembled snapshot dir still works.
DB_DUMP=""
DATA_TAR=""
if [ -f "$SNAPSHOT/MANIFEST.txt" ]; then
	_db=$(grep -E '^db_dump=' "$SNAPSHOT/MANIFEST.txt" | cut -d= -f2- || true)
	_tar=$(grep -E '^data_tar=' "$SNAPSHOT/MANIFEST.txt" | cut -d= -f2- || true)
	[ -n "$_db" ] && DB_DUMP="$SNAPSHOT/$_db"
	[ -n "$_tar" ] && DATA_TAR="$SNAPSHOT/$_tar"
fi
# Fallback glob (set -e tolerant: the `|| true` keeps an empty match from aborting).
[ -n "$DB_DUMP" ] || DB_DUMP=$(ls "$SNAPSHOT"/*.dump 2>/dev/null | head -n1 || true)
[ -n "$DATA_TAR" ] || DATA_TAR=$(ls "$SNAPSHOT"/*.tar.gz 2>/dev/null | head -n1 || true)

[ -n "$DB_DUMP" ] && [ -f "$DB_DUMP" ] || { echo "error: no DB dump (*.dump) in $SNAPSHOT" >&2; exit 1; }
[ -n "$DATA_TAR" ] && [ -f "$DATA_TAR" ] || { echo "error: no data tar (*.tar.gz) in $SNAPSHOT" >&2; exit 1; }

# Compose invocation (mirrors backup.sh).
set -- compose -p "$PROJECT" -f "$COMPOSE_FILE"
if [ -f "$ENV_FILE" ]; then
	set -- "$@" --env-file "$ENV_FILE"
fi
COMPOSE="docker $*"

read_env() {
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

cat <<EOF
About to RESTORE (this OVERWRITES live data):
  snapshot   = $SNAPSHOT
  db dump    = $DB_DUMP  ->  database "$PG_DB" (user "$PG_USER")
  data tar   = $DATA_TAR  ->  volume "$DATA_VOLUME"
  project    = $PROJECT
EOF

# Confirmation gate. Default is to ABORT unless the operator explicitly opts in.
if [ "$ASSUME_YES" -ne 1 ]; then
	printf 'Type "yes" to proceed: '
	read -r REPLY
	[ "$REPLY" = "yes" ] || { echo "aborted."; exit 1; }
fi

# --- 1) restore the data volume ---------------------------------------------
# We restore the volume FIRST so the backend, when it (re)starts, sees the repos.
# The postgres service should be up for the DB step; stop the backend/frontend so
# nothing reads a half-written volume. (Best-effort: ignore if not running.)
echo ">> stopping backend/frontend (best effort) ..."
$COMPOSE stop backend frontend 2>/dev/null || true

echo ">> wiping + extracting data volume ($DATA_VOLUME) ..."
# A throwaway container mounts the volume RW. It clears existing contents (mindful
# of dotfiles) then untars the snapshot into /data. The DATA_TAR is bind-mounted
# read-only as /snapshot.tar.gz.
docker run --rm \
	-v "${DATA_VOLUME}:/data" \
	-v "$DATA_TAR:/snapshot.tar.gz:ro" \
	alpine:3 \
	sh -c 'set -e; find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} +; tar -xzf /snapshot.tar.gz -C /data'
echo "   data volume restored."

# --- 2) restore the database -------------------------------------------------
# Make sure postgres is up, then pg_restore with --clean --if-exists so existing
# objects are dropped and recreated from the dump (idempotent re-restore).
echo ">> ensuring postgres is up ..."
$COMPOSE up -d "$PG_SERVICE"

# Wait for readiness so pg_restore doesn't race a still-booting server.
echo ">> waiting for postgres to accept connections ..."
i=0
while [ "$i" -lt 30 ]; do
	if $COMPOSE exec -T "$PG_SERVICE" pg_isready -U "$PG_USER" -d "$PG_DB" >/dev/null 2>&1; then
		break
	fi
	i=$((i + 1))
	sleep 2
done

echo ">> restoring Postgres ($PG_DB) ..."
# --clean --if-exists: drop then recreate objects; --no-owner: don't require the
# dump's role to exist. Stream the dump file into the container over stdin.
$COMPOSE exec -T "$PG_SERVICE" pg_restore --clean --if-exists --no-owner -U "$PG_USER" -d "$PG_DB" <"$DB_DUMP"
echo "   database restored."

# --- 3) bring the app back ---------------------------------------------------
# On boot the backend runs forward-only goose migrations (advisory-locked), so an
# older dump's schema is brought current automatically here.
echo ">> starting backend/frontend ..."
$COMPOSE up -d

echo ">> restore complete. Verify: curl -fsS http://localhost:8080/healthz && echo OK"
