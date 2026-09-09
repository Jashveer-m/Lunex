#!/usr/bin/env bash
# End-to-end check for the Phase 3 retrieval pipeline and the Phase 4
# assistant.
#
# Boots the API against TEST_DATABASE_URL, registers a user, uploads a small
# text file, searches for a phrase from it, then asks the assistant two
# questions: one the document answers -- which must come back citing it -- and
# one nothing in the corpus answers, which must come back citing nothing.
# Nothing here is mocked: real Postgres, real pgvector, real Ollama, real
# generation.
#
#   ./scripts/e2e.sh
#
# Requires: a Postgres with the `vector` extension available, and Ollama
# serving nomic-embed-text and llama3.2:3b.
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
TAGS="$(curl -sf --max-time 5 "${OLLAMA}/api/tags")"
echo "${TAGS}" | grep -q nomic-embed-text \
  || fail "nomic-embed-text is not pulled. Run: ollama pull nomic-embed-text"
CHAT_MODEL="${CHAT_MODEL:-llama3.2:3b}"
echo "${TAGS}" | grep -q "${CHAT_MODEL}" \
  || fail "${CHAT_MODEL} is not pulled. Run: ollama pull ${CHAT_MODEL}"
ok "Ollama is up with nomic-embed-text and ${CHAT_MODEL}"

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
  CHAT_MODEL="${CHAT_MODEL}" \
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

# --- Phase 4: the assistant -------------------------------------------------
#
# The reply arrives as Server-Sent Events, so the assertions below are made
# over parsed frames rather than over raw text: `sse` pulls one fact out of a
# captured stream.
sse() { python3 - "$1" "$2" <<'PYEOF'
import sys, json
path, want = sys.argv[1], sys.argv[2]
events, name = [], None
for line in open(path):
    line = line.rstrip("\n")
    if line.startswith("event: "):
        name = line[7:]
    elif line.startswith("data: "):
        events.append((name, json.loads(line[6:])))
by = lambda n: next((d for k, d in events if k == n), None)
answer = "".join(d["text"] for k, d in events if k == "token")
done, err, srcs = by("done"), by("error"), by("sources")
if err:
    sys.exit("stream failed: %s" % err)
sources = (done or {}).get("message", {}).get("sources", [])
out = {
    "answer": answer,
    "source_count": len(sources),
    "sources_event_count": (srcs or {}).get("count", -1),
    "cited": ",".join(s["title"] for s in sources if s["cited"]),
    "titles": ",".join(s["title"] for s in sources),
    "first_event": events[0][0] if events else "",
    "model": (done or {}).get("model", ""),
}
print(out[want])
PYEOF
}

# ask <conversation-id> <question> <outfile>
ask() {
  curl -sN -X POST "${API}/api/v1/conversations/$1/messages" -H "${AUTH}" \
    -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys;print(json.dumps({"content":sys.argv[1]}))' "$2")" \
    > "$3"
}

step "Starting a conversation"
CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
[ -n "${CONV}" ] || fail "conversation was not created"
ok "conversation ${CONV}"

step "Asking a question the document answers"
GROUNDED="$(mktemp)"
ask "${CONV}" "What do my field notes say happened over the tundra?" "${GROUNDED}"

[ "$(sse "${GROUNDED}" first_event)" = "sources" ] \
  || fail "the stream did not lead with the retrieved sources: $(head -c 400 "${GROUNDED}")"
SRC_COUNT=$(sse "${GROUNDED}" source_count)
[ "${SRC_COUNT}" -ge 1 ] || fail "the assistant retrieved nothing for a question its own notes answer"
sse "${GROUNDED}" titles | grep -q "field-notes.txt" \
  || fail "the retrieved sources do not include the document: $(sse "${GROUNDED}" titles)"
sse "${GROUNDED}" cited | grep -q "field-notes.txt" \
  || fail "the answer did not cite the document it was given: $(sse "${GROUNDED}" answer)"
ANSWER="$(sse "${GROUNDED}" answer)"
echo "${ANSWER}" | grep -qi "aurora\|northern lights\|tundra" \
  || fail "the answer does not use the retrieved text: ${ANSWER}"
ok "cited field-notes.txt out of ${SRC_COUNT} retrieved source(s), model $(sse "${GROUNDED}" model)"
printf '     %s\n' "$(echo "${ANSWER}" | tr '\n' ' ' | head -c 260)"

step "Asking a question nothing in the corpus answers"
UNGROUNDED="$(mktemp)"
ask "${CONV}" "What is the current price of Brent crude oil on the futures market?" "${UNGROUNDED}"

# The similarity floor is what makes this hold: the chunk is far enough from
# the question that it is never shown to the model, so there is nothing to
# cite even by accident.
[ "$(sse "${UNGROUNDED}" sources_event_count)" = "0" ] \
  || fail "an unrelated question retrieved $(sse "${UNGROUNDED}" sources_event_count) source(s): $(sse "${UNGROUNDED}" titles)"
[ "$(sse "${UNGROUNDED}" source_count)" = "0" ] \
  || fail "an unrelated question recorded sources it should not have"
UNGROUNDED_ANSWER="$(sse "${UNGROUNDED}" answer)"
if echo "${UNGROUNDED_ANSWER}" | grep -q '\[S[0-9]'; then
  fail "the assistant invented a citation: ${UNGROUNDED_ANSWER}"
fi
if echo "${UNGROUNDED_ANSWER}" | grep -qi "field-notes\|your notes say\|according to your document"; then
  fail "the assistant claimed an answer came from the user's data: ${UNGROUNDED_ANSWER}"
fi
ok "nothing retrieved, no citation invented"
printf '     %s\n' "$(echo "${UNGROUNDED_ANSWER}" | tr '\n' ' ' | head -c 260)"

step "Reading the conversation back"
CONV_BODY=$(curl -s "${API}/api/v1/conversations/${CONV}" -H "${AUTH}")
MSGS=$(echo "${CONV_BODY}" | json 'len(d["messages"])')
[ "${MSGS}" = "4" ] || fail "the conversation holds ${MSGS} messages, want 4: ${CONV_BODY}"
echo "${CONV_BODY}" | json 'd["messages"][0]["role"]+"/"+d["messages"][1]["role"]' \
  | grep -q '^user/assistant$' || fail "the answer is stored before the question"
[ "$(echo "${CONV_BODY}" | json 'len(d["messages"][1]["sources"])')" != "0" ] \
  || fail "the grounded answer was stored without its sources"
[ "$(echo "${CONV_BODY}" | json 'len(d["messages"][3]["sources"])')" = "0" ] \
  || fail "the ungrounded answer was stored with sources"
echo "${CONV_BODY}" | json 'd["title"]' | grep -qi "field notes" \
  || fail "the conversation was not named from its first question"
ok "two turns stored in order, sources on the grounded answer only"

step "Deleting the document"
curl -s -o /dev/null -w '%{http_code}' -X DELETE "${API}/api/v1/documents/${DOC_ID}" -H "${AUTH}" \
  | grep -q 204 || fail "delete did not return 204"
LEFT=$(psql "${DB}" -tAc 'SELECT count(*) FROM document_chunks')
[ "${LEFT}" = "0" ] || fail "${LEFT} chunks survived the deleted document"
ok "document deleted, chunks cascaded"

printf '\n\033[32mPASS\033[0m upload -> extract -> chunk -> embed -> store -> retrieve -> ask -> cite\n'
