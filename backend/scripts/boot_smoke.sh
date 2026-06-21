#!/usr/bin/env bash
# boot_smoke.sh — end-to-end boot smoke test for kotojid against a throwaway
# Postgres. Starts postgres:18-alpine, runs goose migrations + the seed, boots
# kotojid in RUN_MODE=all (dev auth), then exercises the control plane (health,
# config, me, create+publish a site) and the data plane (serve the published site
# via a {handle}.localhost Host header). Tears the container + server down on exit.
#
# Usage: scripts/boot_smoke.sh   (run from the backend module root)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CONTAINER="kotoji-smoke-pg"
PGPORT=55433
DSN="postgres://kotoji:kotoji@127.0.0.1:${PGPORT}/kotoji?sslmode=disable"
DATA_DIR="$(mktemp -d /tmp/kotoji-smoke-data.XXXXXX)"
CONTROL_PORT=18080
SERVE_PORT=18081
HANDLE="smoke-site"
SERVER_PID=""
FAIL=0

log()  { printf '\n=== %s ===\n' "$*"; }
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; FAIL=1; }

cleanup() {
  log "cleanup"
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1 && echo "removed container $CONTAINER"
  rm -rf "$DATA_DIR"
}
trap cleanup EXIT

# ---- 1. throwaway postgres ----
log "start postgres:18-alpine"
docker rm -f "$CONTAINER" >/dev/null 2>&1
docker run -d --name "$CONTAINER" \
  -e POSTGRES_USER=kotoji -e POSTGRES_PASSWORD=kotoji -e POSTGRES_DB=kotoji \
  -p ${PGPORT}:5432 postgres:18-alpine >/dev/null || { echo "docker run failed"; exit 1; }

log "wait for postgres ready"
for i in $(seq 1 60); do
  if docker exec "$CONTAINER" pg_isready -U kotoji -d kotoji >/dev/null 2>&1; then
    echo "postgres ready after ${i}s"; break
  fi
  sleep 1
  [[ $i -eq 60 ]] && { echo "postgres never became ready"; exit 1; }
done

# ---- 2. shared env ----
export KOTOJI_ENV=development
export KOTOJI_AUTH_MODE=dev          # maps to AuthModeNone (no-auth dev login)
export KOTOJI_DATABASE_URL="$DSN"
export KOTOJI_RUN_MODE=all
export KOTOJI_DATA_DIR="$DATA_DIR"
export KOTOJI_BASE_DOMAIN=localhost
export KOTOJI_CONTROL_BASE_URL="http://localhost:${CONTROL_PORT}"
export KOTOJI_CONTROL_ADDR=":${CONTROL_PORT}"
export KOTOJI_SERVE_ADDR=":${SERVE_PORT}"
export KOTOJI_AUTH_ADMIN_EMAIL="admin@kotoji.local"
export KOTOJI_LOG_LEVEL=info
export KOTOJI_LOG_FORMAT=text
# AUTH_MODE accepts oidc|password|none; "dev" is not valid, use none for no-auth.
export KOTOJI_AUTH_MODE=none

# ---- 3. migrations + seed ----
log "goose migrations up"
goose -dir internal/db/migrations postgres "$DSN" up || { echo "goose failed"; exit 1; }

log "seed dev admin"
go run ./cmd/seed || { echo "seed failed"; exit 1; }

# ---- 4. boot kotojid ----
log "boot kotojid RUN_MODE=all"
go build -o "$DATA_DIR/kotojid" ./cmd/kotojid || { echo "build failed"; exit 1; }
"$DATA_DIR/kotojid" >"$DATA_DIR/server.log" 2>&1 &
SERVER_PID=$!

log "wait for control plane /healthz"
for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${CONTROL_PORT}/healthz" 2>/dev/null)
  [[ "$code" == "200" ]] && { echo "control plane up after ${i}s"; break; }
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then echo "server died:"; cat "$DATA_DIR/server.log"; exit 1; fi
  sleep 1
  [[ $i -eq 30 ]] && { echo "control plane never came up:"; cat "$DATA_DIR/server.log"; exit 1; }
done

CTRL="http://127.0.0.1:${CONTROL_PORT}"
SERVE="http://127.0.0.1:${SERVE_PORT}"
JAR="$DATA_DIR/cookies.txt"

# ---- 5. health probes ----
log "control /healthz + /readyz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${CTRL}/healthz");      [[ "$code" == "200" ]] && pass "control /healthz 200" || fail "control /healthz $code"
code=$(curl -s -o /dev/null -w '%{http_code}' "${CTRL}/readyz");       [[ "$code" == "200" ]] && pass "control /readyz 200"  || fail "control /readyz $code"
code=$(curl -s -o /dev/null -w '%{http_code}' "${SERVE}/healthz");     [[ "$code" == "200" ]] && pass "serve /healthz 200"    || fail "serve /healthz $code"
code=$(curl -s -o /dev/null -w '%{http_code}' "${SERVE}/readyz");      [[ "$code" == "200" ]] && pass "serve /readyz 200"     || fail "serve /readyz $code"

# ---- 6. public config ----
log "GET /api/config (public)"
cfg_body=$(curl -s -w '\n%{http_code}' "${CTRL}/api/config")
cfg_code=$(echo "$cfg_body" | tail -1)
echo "$cfg_body" | head -n -1
[[ "$cfg_code" == "200" ]] && pass "/api/config 200" || fail "/api/config $cfg_code"

# ---- 7. dev login -> session cookie ----
log "GET /auth/login (dev no-auth) -> session cookie"
login_code=$(curl -s -c "$JAR" -o /dev/null -w '%{http_code}' "${CTRL}/auth/login")
echo "login status: $login_code (302 expected)"
[[ "$login_code" == "302" ]] && pass "/auth/login 302" || fail "/auth/login $login_code"

# ---- 8. /api/me (authenticated) ----
log "GET /api/me (with session cookie)"
me_body=$(curl -s -c "$JAR" -b "$JAR" -w '\n%{http_code}' "${CTRL}/api/me")
me_code=$(echo "$me_body" | tail -1)
echo "$me_body" | head -n -1
[[ "$me_code" == "200" ]] && pass "/api/me 200" || fail "/api/me $me_code"

# Extract the CSRF token issued alongside /api/me (double-submit cookie).
CSRF=$(awk '/kotoji_csrf/ {print $7}' "$JAR" | tail -1)
echo "csrf token: ${CSRF:0:12}..."

# ---- 9. create a site (POST, CSRF-guarded) ----
log "POST /api/sites (create ${HANDLE})"
create_body=$(curl -s -c "$JAR" -b "$JAR" -H "X-CSRF-Token: ${CSRF}" \
  -H "Content-Type: application/json" \
  -d "{\"handle\":\"${HANDLE}\",\"visibility\":\"public\"}" \
  -w '\n%{http_code}' "${CTRL}/api/sites")
create_code=$(echo "$create_body" | tail -1)
echo "$create_body" | head -n -1
[[ "$create_code" == "201" ]] && pass "create site 201" || fail "create site $create_code"

# Pull the draft tip SHA (baseSha) needed to publish.
log "GET /api/sites/${HANDLE}/branches"
branches=$(curl -s -c "$JAR" -b "$JAR" "${CTRL}/api/sites/${HANDLE}/branches")
echo "$branches"
DRAFT_SHA=$(echo "$branches" | grep -o '"headSha":"[0-9a-f]*"' | head -1 | sed 's/.*"headSha":"//;s/"//')
echo "draft head sha: $DRAFT_SHA"

# ---- 10. publish draft -> published ----
log "POST /api/sites/${HANDLE}/publish"
pub_body=$(curl -s -c "$JAR" -b "$JAR" -H "X-CSRF-Token: ${CSRF}" \
  -H "Content-Type: application/json" \
  -d "{\"from\":\"draft\",\"baseSha\":\"${DRAFT_SHA}\"}" \
  -w '\n%{http_code}' "${CTRL}/api/sites/${HANDLE}/publish")
pub_code=$(echo "$pub_body" | tail -1)
echo "$pub_body" | head -n -1
[[ "$pub_code" == "200" || "$pub_code" == "201" ]] && pass "publish $pub_code" || fail "publish $pub_code"

# ---- 11. data plane: serve the published site via Host header ----
log "GET data plane with Host: ${HANDLE}.localhost"
serve_body=$(curl -s -H "Host: ${HANDLE}.localhost" -w '\n%{http_code}' "${SERVE}/")
serve_code=$(echo "$serve_body" | tail -1)
echo "$serve_body" | head -n -1 | head -5
[[ "$serve_code" == "200" ]] && pass "data-plane served ${HANDLE}.localhost 200" || fail "data-plane ${HANDLE}.localhost $serve_code"

# ---- 12. data plane: unknown host 404 ----
log "GET data plane with Host: nope.localhost (expect 404)"
nf_code=$(curl -s -o /dev/null -H "Host: nope.localhost" -w '%{http_code}' "${SERVE}/")
[[ "$nf_code" == "404" ]] && pass "unknown handle 404" || fail "unknown handle $nf_code"

log "server log tail"
tail -15 "$DATA_DIR/server.log"

log "RESULT"
if [[ $FAIL -eq 0 ]]; then echo "ALL SMOKE CHECKS PASSED"; else echo "SOME SMOKE CHECKS FAILED"; fi
exit $FAIL
