#!/usr/bin/env bash
# End-to-end check for the Phase 3 pipeline.
#
# Boots the API against TEST_DATABASE_URL, registers a user, uploads a small
# text file, and searches for a phrase from it -- asserting that the chunk
# comes back with the right filename and a sane similarity score. Nothing here
# is mocked: real Postgres, real pgvector, real Ollama.
#
#   ./scripts/e2e.sh
#
# Requires: a Postgres with the `vector` extension available, and Ollama
# serving nomic-embed-text.
set -euo pipefail

DB="${TEST_DATABASE_URL:-postgres://postgres@localhost:5432/lunex_test?sslmode=disable}"
OLLAMA="${OLLAMA_BASE_URL:-http://localhost:11434}"
PORT="${E2E_PORT:-8099}"
API="http://localhost:${PORT}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() { printf '\n\033[31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }
step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
ok()   { printf '  \033[32mok\033[0m %s\n' "$1"; }

# jq is not assumed; python3 is already required by the README's examples.
json() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

step "Preflight"
curl -sf --max-time 5 "${OLLAMA}/api/version" >/dev/null \
  || fail "Ollama is not answering at ${OLLAMA}. Start it with: ollama serve"
curl -sf --max-time 5 "${OLLAMA}/api/tags" | grep -q nomic-embed-text \
  || fail "nomic-embed-text is not pulled. Run: ollama pull nomic-embed-text"
ok "Ollama is up with nomic-embed-text"

step "Migrating ${DB}"
(cd "${ROOT}/backend" && DATABASE_URL="${DB}" go run ./cmd/migrate up >/dev/null)
psql "${DB}" -tAc "SELECT extversion FROM pg_extension WHERE extname='vector'" | grep -q . \
  || fail "the vector extension is not installed in the test database"
ok "schema is up, pgvector $(psql "${DB}" -tAc "SELECT extversion FROM pg_extension WHERE extname='vector'")"

# A clean slate, so counts below mean what they say.
psql "${DB}" -q -c 'TRUNCATE users CASCADE'

step "Starting the API on :${PORT}"
LOG="$(mktemp)"
(cd "${ROOT}/backend" && \
  DATABASE_URL="${DB}" \
  JWT_SECRET="e2e-secret-that-is-comfortably-over-32-bytes-long" \
  PORT="${PORT}" \
  OLLAMA_BASE_URL="${OLLAMA}" \
  go run ./cmd/api >"${LOG}" 2>&1) &
API_PID=$!
trap 'kill "${API_PID}" 2>/dev/null || true; wait "${API_PID}" 2>/dev/null || true' EXIT

for _ in $(seq 1 60); do
  curl -sf --max-time 2 "${API}/healthz" >/dev/null && break
  sleep 1
done
curl -sf --max-time 2 "${API}/healthz" >/dev/null || { cat "${LOG}"; fail "the API never became healthy"; }
ok "healthz answers"

step "Registering a user"
ACCESS=$(curl -s -X POST "${API}/api/v1/auth/register" \
  -H 'Content-Type: application/json' \
  -d '{"email":"e2e@example.com","password":"correct horse battery staple","name":"E2E"}' \
  | json 'd["tokens"]["access_token"]')
[ -n "${ACCESS}" ] || fail "registration returned no access token"
AUTH="Authorization: Bearer ${ACCESS}"
ok "registered"

step "Uploading a text file"
FILE="$(mktemp -d)/field-notes.txt"
cat > "${FILE}" <<'TXT'
Field notes, 14 March.

The aurora borealis appeared over the tundra shortly after midnight and lasted
about forty minutes. The dogs slept through the whole thing.

Separately: the generator needs a new fuel filter before the next resupply run,
and the shortwave antenna guy-line on the north side has gone slack again.
TXT

DOC=$(curl -s -X POST "${API}/api/v1/documents" -H "${AUTH}" -F "file=@${FILE}")
STATUS=$(echo "${DOC}" | json 'd["status"]')
DOC_ID=$(echo "${DOC}" | json 'd["id"]')
[ "${STATUS}" = "ready" ] || fail "document status is '${STATUS}', want 'ready': ${DOC}"
CHUNKS=$(echo "${DOC}" | json 'd["chunk_count"]')
[ "${CHUNKS}" -ge 1 ] || fail "document has ${CHUNKS} chunks"
ok "uploaded ${DOC_ID}, status ready, ${CHUNKS} chunk(s)"

step "Searching for a phrase from the file"
FOUND=$(curl -s -X POST "${API}/api/v1/documents/search" -H "${AUTH}" \
  -H 'Content-Type: application/json' \
  -d '{"query":"what happened with the northern lights over the tundra?","limit":3}')

COUNT=$(echo "${FOUND}" | json 'd["count"]')
[ "${COUNT}" -ge 1 ] || fail "search returned no results: ${FOUND}"

echo "${FOUND}" | json 'd["results"][0]["content"]' | grep -qi "aurora borealis" \
  || fail "the top result does not contain the phrase: ${FOUND}"
echo "${FOUND}" | json 'd["results"][0]["filename"]' | grep -q "field-notes.txt" \
  || fail "the result carries the wrong filename: ${FOUND}"

SIM=$(echo "${FOUND}" | json 'd["results"][0]["similarity"]')
python3 -c "import sys; sys.exit(0 if 0 < ${SIM} <= 1.0000001 else 1)" \
  || fail "similarity ${SIM} is outside (0, 1]"
ok "top result cites field-notes.txt, similarity ${SIM}"

step "Deleting the document"
curl -s -o /dev/null -w '%{http_code}' -X DELETE "${API}/api/v1/documents/${DOC_ID}" -H "${AUTH}" \
  | grep -q 204 || fail "delete did not return 204"
LEFT=$(psql "${DB}" -tAc 'SELECT count(*) FROM document_chunks')
[ "${LEFT}" = "0" ] || fail "${LEFT} chunks survived the deleted document"
ok "document deleted, chunks cascaded"

printf '\n\033[32mPASS\033[0m upload -> extract -> chunk -> embed -> store -> retrieve\n'
