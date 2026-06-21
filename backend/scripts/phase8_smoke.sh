#!/usr/bin/env bash
# phase8_smoke.sh — focused boot smoke for the Phase 8 additions: the GitHub
# webhook receiver and the preview-grant -> data-plane-accept flow. Starts a
# throwaway postgres:18-alpine, migrates + seeds, boots kotojid RUN_MODE=all with
# the GitHub mirror ENABLED (so the webhook route mounts) + a webhook secret, then:
#
#   - webhook: bad signature            -> 401
#   - webhook: signed non-tracked push  -> 200 (verify+parse+lookup path, no git fetch)
#   - webhook: signed unknown-repo push -> 200 (ignored)
#   - preview-grant: viewer issues grant -> data plane accepts (?kpt) -> serves draft
#
# Tears down the container + server on exit.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CONTAINER="kotoji-p8-pg"
PGPORT=55434
DSN="postgres://kotoji:kotoji@127.0.0.1:${PGPORT}/kotoji?sslmode=disable"
DATA_DIR="$(mktemp -d /tmp/kotoji-p8-data.XXXXXX)"
CONTROL_PORT=18090
SERVE_PORT=18091
HANDLE="p8-site"
BRANCH="feature-smoke"
WEBHOOK_SECRET="phase8-smoke-secret"
SERVER_PID=""
FAIL=0

log()  { printf '\n=== %s ===\n' "$*"; }
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; FAIL=1; }

cleanup() {
  log "cleanup"
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null; wait "$SERVER_PID" 2>/dev/null
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1 && echo "removed container $CONTAINER"
  rm -rf "$DATA_DIR"
}
trap cleanup EXIT

log "start postgres:18-alpine"
docker rm -f "$CONTAINER" >/dev/null 2>&1
docker run -d --name "$CONTAINER" \
  -e POSTGRES_USER=kotoji -e POSTGRES_PASSWORD=kotoji -e POSTGRES_DB=kotoji \
  -p ${PGPORT}:5432 postgres:18-alpine >/dev/null || { echo "docker run failed"; exit 1; }

log "wait for postgres ready"
for i in $(seq 1 60); do
  docker exec "$CONTAINER" pg_isready -U kotoji -d kotoji >/dev/null 2>&1 && { echo "ready after ${i}s"; break; }
  sleep 1; [[ $i -eq 60 ]] && { echo "postgres never ready"; exit 1; }
done

export KOTOJI_ENV=development
export KOTOJI_AUTH_MODE=none
export KOTOJI_DATABASE_URL="$DSN"
export KOTOJI_RUN_MODE=all
export KOTOJI_DATA_DIR="$DATA_DIR"
export KOTOJI_BASE_DOMAIN=localhost
export KOTOJI_CONTROL_BASE_URL="http://localhost:${CONTROL_PORT}"
export KOTOJI_CONTROL_ADDR=":${CONTROL_PORT}"
export KOTOJI_SERVE_ADDR=":${SERVE_PORT}"
export KOTOJI_LOG_LEVEL=info
export KOTOJI_LOG_FORMAT=text
# Enable the mirror so the webhook route mounts; provide a token + secret.
export KOTOJI_GITHUB_MIRROR_ENABLED=true
export KOTOJI_GITHUB_PAT="dummy-pat-for-smoke"
export KOTOJI_GITHUB_WEBHOOK_SECRET="$WEBHOOK_SECRET"
# Keep the reaper grace huge so the background scheduler never touches the smoke site.
export KOTOJI_SOFT_DELETE_GRACE=99999h

log "goose migrations up"
goose -dir internal/db/migrations postgres "$DSN" up || { echo "goose failed"; exit 1; }
log "seed dev admin"
go run ./cmd/seed || { echo "seed failed"; exit 1; }

log "boot kotojid RUN_MODE=all (mirror enabled)"
go build -o "$DATA_DIR/kotojid" ./cmd/kotojid || { echo "build failed"; exit 1; }
"$DATA_DIR/kotojid" >"$DATA_DIR/server.log" 2>&1 &
SERVER_PID=$!

for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${CONTROL_PORT}/healthz" 2>/dev/null)
  [[ "$code" == "200" ]] && { echo "control up after ${i}s"; break; }
  kill -0 "$SERVER_PID" 2>/dev/null || { echo "server died:"; cat "$DATA_DIR/server.log"; exit 1; }
  sleep 1; [[ $i -eq 30 ]] && { echo "control never up:"; cat "$DATA_DIR/server.log"; exit 1; }
done

CTRL="http://127.0.0.1:${CONTROL_PORT}"
SERVE="http://127.0.0.1:${SERVE_PORT}"
JAR="$DATA_DIR/cookies.txt"

# ---- session + csrf ----
curl -s -c "$JAR" -o /dev/null "${CTRL}/auth/login"
curl -s -c "$JAR" -b "$JAR" -o /dev/null "${CTRL}/api/me"
CSRF=$(awk '/kotoji_csrf/ {print $7}' "$JAR" | tail -1)

# ---- create a site WITH a github_repo so the webhook can map it ----
log "create site ${HANDLE} (githubRepo=owner/${HANDLE})"
create_code=$(curl -s -c "$JAR" -b "$JAR" -H "X-CSRF-Token: ${CSRF}" -H "Content-Type: application/json" \
  -d "{\"handle\":\"${HANDLE}\",\"visibility\":\"private\",\"githubRepo\":\"owner/${HANDLE}\"}" \
  -o /dev/null -w '%{http_code}' "${CTRL}/api/sites")
[[ "$create_code" == "201" ]] && pass "create site 201" || fail "create site $create_code"

# Pull the draft tip + create a preview branch.
branches=$(curl -s -c "$JAR" -b "$JAR" "${CTRL}/api/sites/${HANDLE}/branches")
DRAFT_SHA=$(echo "$branches" | grep -o '"headSha":"[0-9a-f]*"' | head -1 | sed 's/.*"headSha":"//;s/"//')
log "create preview branch ${BRANCH} from draft@${DRAFT_SHA:0:7}"
br_code=$(curl -s -c "$JAR" -b "$JAR" -H "X-CSRF-Token: ${CSRF}" -H "Content-Type: application/json" \
  -d "{\"name\":\"${BRANCH}\",\"from\":\"draft\"}" \
  -o /dev/null -w '%{http_code}' "${CTRL}/api/sites/${HANDLE}/branches")
[[ "$br_code" == "201" || "$br_code" == "200" ]] && pass "create branch $br_code" || fail "create branch $br_code"

# ================= WEBHOOK =================
hmac() { printf '%s' "$2" | openssl dgst -sha256 -hmac "$1" | sed 's/^.*= //'; }

# 1. bad signature -> 401
log "webhook: bad signature -> 401"
BODY_PUB='{"ref":"refs/heads/published","repository":{"full_name":"owner/'"${HANDLE}"'"}}'
wh_bad=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${CTRL}/api/webhooks/github" \
  -H "X-GitHub-Event: push" -H "X-Hub-Signature-256: sha256=deadbeef" \
  -H "Content-Type: application/json" -d "$BODY_PUB")
[[ "$wh_bad" == "401" ]] && pass "webhook bad-sig 401" || fail "webhook bad-sig $wh_bad"

# 2. signed push to a NON-tracked branch (draft) -> 200 (verify+parse+lookup, no git fetch)
log "webhook: signed non-tracked (draft) push -> 200"
BODY_DRAFT='{"ref":"refs/heads/draft","repository":{"full_name":"owner/'"${HANDLE}"'"}}'
SIG_DRAFT="sha256=$(hmac "$WEBHOOK_SECRET" "$BODY_DRAFT")"
wh_draft=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${CTRL}/api/webhooks/github" \
  -H "X-GitHub-Event: push" -H "X-Hub-Signature-256: ${SIG_DRAFT}" \
  -H "Content-Type: application/json" -d "$BODY_DRAFT")
[[ "$wh_draft" == "200" ]] && pass "webhook signed non-tracked 200" || fail "webhook signed non-tracked $wh_draft"

# 3. signed push for an UNKNOWN repo -> 200 (ignored)
log "webhook: signed unknown-repo push -> 200 (ignored)"
BODY_UNK='{"ref":"refs/heads/published","repository":{"full_name":"someone/else"}}'
SIG_UNK="sha256=$(hmac "$WEBHOOK_SECRET" "$BODY_UNK")"
wh_unk=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${CTRL}/api/webhooks/github" \
  -H "X-GitHub-Event: push" -H "X-Hub-Signature-256: ${SIG_UNK}" \
  -H "Content-Type: application/json" -d "$BODY_UNK")
[[ "$wh_unk" == "200" ]] && pass "webhook unknown-repo 200" || fail "webhook unknown-repo $wh_unk"

# ================= PREVIEW GRANT =================
log "preview-grant: issue for ${HANDLE}--${BRANCH}"
pg_body=$(curl -s -X POST -c "$JAR" -b "$JAR" -H "X-CSRF-Token: ${CSRF}" -H "Content-Type: application/json" \
  -w '\n%{http_code}' "${CTRL}/api/sites/${HANDLE}/branches/${BRANCH}/preview-grant")
pg_code=$(echo "$pg_body" | tail -1)
pg_json=$(echo "$pg_body" | head -n -1)
echo "$pg_json"
[[ "$pg_code" == "200" ]] && pass "preview-grant 200" || fail "preview-grant $pg_code"

GRANT=$(echo "$pg_json" | grep -o '"grant":"[^"]*"' | sed 's/.*"grant":"//;s/"//')
echo "grant: ${GRANT:0:24}..."

# Data plane: first WITHOUT the grant -> preview is auth-gated -> 404 (no leak).
log "data plane: preview WITHOUT grant -> 404"
dp_nogrant=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Host: ${HANDLE}--${BRANCH}.localhost" "${SERVE}/")
[[ "$dp_nogrant" == "404" ]] && pass "preview no-grant 404 (gated)" || fail "preview no-grant $dp_nogrant"

# Data plane: WITH the one-time grant -> verifier accepts, sets cookie, 302 to strip kpt.
log "data plane: preview WITH grant -> accepted (302 set-cookie, then 200)"
PJAR="$DATA_DIR/preview-cookies.txt"
dp_grant=$(curl -s -o /dev/null -w '%{http_code}' -c "$PJAR" \
  -H "Host: ${HANDLE}--${BRANCH}.localhost" "${SERVE}/?kpt=${GRANT}")
# The handler 302s to strip the kpt param after setting the host-only cookie.
[[ "$dp_grant" == "302" || "$dp_grant" == "200" ]] && pass "preview grant accepted ($dp_grant)" || fail "preview grant $dp_grant"

# Follow up with the cookie (no grant param) -> served draft content (200).
log "data plane: preview with cookie -> 200"
dp_cookie=$(curl -s -o /dev/null -w '%{http_code}' -b "$PJAR" \
  -H "Host: ${HANDLE}--${BRANCH}.localhost" "${SERVE}/")
[[ "$dp_cookie" == "200" ]] && pass "preview cookie serves 200" || fail "preview cookie $dp_cookie"

# A grant for THIS site presented on a DIFFERENT (nonexistent) host must not leak -> 404.
log "data plane: grant on wrong host -> 404"
dp_wrong=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Host: other--${BRANCH}.localhost" "${SERVE}/?kpt=${GRANT}")
[[ "$dp_wrong" == "404" ]] && pass "wrong-host grant 404" || fail "wrong-host grant $dp_wrong"

log "server log tail"
tail -20 "$DATA_DIR/server.log"

log "RESULT"
if [[ $FAIL -eq 0 ]]; then echo "ALL PHASE-8 SMOKE CHECKS PASSED"; else echo "SOME PHASE-8 SMOKE CHECKS FAILED"; fi
exit $FAIL
