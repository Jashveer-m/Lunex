#!/usr/bin/env bash
# End-to-end check for the Phase 3 retrieval pipeline, the Phase 4 assistant
# and the Phase 5 memory system.
#
# Boots the API against TEST_DATABASE_URL, registers a user, uploads a small
# text file, searches for a phrase from it, then asks the assistant two
# questions: one the document answers -- which must come back citing it -- and
# one nothing in the corpus answers, which must come back citing nothing.
#
# Then the memory check: a conversation that states a durable fact about the
# user, a look at what was extracted from it, and a *separate* conversation
# whose answer must retrieve and cite that memory -- followed by switching the
# memory off and watching the assistant stop using it.
#
# Nothing here is mocked: real Postgres, real pgvector, real Ollama, real
# generation, real extraction.
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
memories = [s for s in sources if s["type"] == "memory"]
out = {
    "answer": answer,
    "source_count": len(sources),
    "sources_event_count": (srcs or {}).get("count", -1),
    "cited": ",".join(s["title"] for s in sources if s["cited"]),
    "titles": ",".join(s["title"] for s in sources),
    "types": ",".join(s["type"] for s in sources),
    "first_event": events[0][0] if events else "",
    "model": (done or {}).get("model", ""),
    # Phase 5: what was retrieved from memory, what of it the answer used, and
    # what the turn itself put into memory.
    "memory_count": len(memories),
    "memories": " | ".join(s["excerpt"] for s in memories),
    "memories_cited": len([s for s in memories if s["cited"]]),
    "remembered": " | ".join(m["content"] for m in (done or {}).get("remembered", [])),
    "remembered_count": len((done or {}).get("remembered", [])),
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

# --- Phase 5: the memory system ---------------------------------------------
#
# The check the phase turns on: a fact stated in one conversation has to reach
# the prompt of a different one, be used, and be cited -- with nothing shared
# between the two conversations but the memory itself.

step "Stating a durable fact in a new conversation"
MEM_CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
[ -n "${MEM_CONV}" ] || fail "conversation was not created"

STATING="$(mktemp)"
# A plain statement, not a question about the user's own data. Asking one --
# "plan my week around this" -- makes the assistant answer "I could not find
# anything about your schedule", and llama3.2:3b then reads that sentence as
# evidence that the exchange contained nothing worth keeping. See
# docs/decisions.md.
ask "${MEM_CONV}" "Something to keep in mind about me: I always study in the early morning before class, and this term's systems programming coursework is in Rust." "${STATING}"
sse "${STATING}" answer >/dev/null || fail "the stating turn failed"
ok "stated the fact, assistant answered"
printf '     %s\n' "$(sse "${STATING}" answer | tr '\n' ' ' | head -c 200)"

step "Checking what was extracted"
MEMS=$(curl -s "${API}/api/v1/memories" -H "${AUTH}")
MEM_COUNT=$(echo "${MEMS}" | json 'd["count"]')
[ "${MEM_COUNT}" -ge 1 ] \
  || fail "nothing was extracted from an exchange stating a durable fact: ${MEMS}"

echo "${MEMS}" | json '" | ".join(m["content"] for m in d["memories"])' | grep -qi "morning\|rust\|stud" \
  || fail "the extracted memories are not about what was said: $(echo "${MEMS}" | json 'd["memories"]')"

# Every memory carries the conversation it came from, a type from the closed
# set, and scores inside [0, 1] -- the columns the CHECK constraints guard.
echo "${MEMS}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
kinds = {"episodic", "semantic", "preference", "project", "goal"}
for m in d["memories"]:
    assert m["type"] in kinds, "unknown type %r" % m["type"]
    assert 0 <= m["importance"] <= 1, "importance out of range: %r" % m["importance"]
    assert 0 <= m["confidence"] <= 1, "confidence out of range: %r" % m["confidence"]
    assert m["source_conversation_id"], "memory has no provenance: %r" % m
    assert m["enabled"] is True, "a new memory is not enabled: %r" % m
' || fail "an extracted memory is malformed: ${MEMS}"

ok "extracted ${MEM_COUNT} memory/memories"
echo "${MEMS}" | python3 -c '
import sys, json
for m in json.load(sys.stdin)["memories"]:
    print("     %-11s %.2f/%.2f  %s" % (m["type"], m["importance"], m["confidence"], m["content"]))
'

step "Asking about it in a different conversation"
# A brand new conversation: no shared history, so anything the assistant knows
# here came out of the memory store.
RECALL_CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
RECALL="$(mktemp)"
ask "${RECALL_CONV}" "What time of day do I prefer to study, and what language am I using for my coursework?" "${RECALL}"

MEM_RETRIEVED=$(sse "${RECALL}" memory_count)
[ "${MEM_RETRIEVED}" -ge 1 ] \
  || fail "the new conversation retrieved no memories: types=$(sse "${RECALL}" types)"
[ "$(sse "${RECALL}" memories_cited)" -ge 1 ] \
  || fail "the answer did not cite the memory it was given: $(sse "${RECALL}" answer)"
RECALL_ANSWER="$(sse "${RECALL}" answer)"
echo "${RECALL_ANSWER}" | grep -qi "morning\|rust" \
  || fail "the answer does not use what was remembered: ${RECALL_ANSWER}"
ok "retrieved ${MEM_RETRIEVED} memory/memories in a fresh conversation and cited one"
printf '     %s\n' "$(sse "${RECALL}" memories | head -c 200)"
printf '     %s\n' "$(echo "${RECALL_ANSWER}" | tr '\n' ' ' | head -c 260)"

step "Switching the memories off"
# All of them, so the assertion below can be "nothing was retrieved" rather
# than "the one I disabled was missing from a list of several".
for id in $(echo "${MEMS}" | json '" ".join(m["id"] for m in d["memories"])'); do
  curl -s -o /dev/null -w '%{http_code}' -X PATCH "${API}/api/v1/memories/${id}" -H "${AUTH}" \
    -H 'Content-Type: application/json' -d '{"enabled":false}' | grep -q 200 \
    || fail "PATCH /memories/${id} did not return 200"
done

OFF_CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
OFF="$(mktemp)"
ask "${OFF_CONV}" "What time of day do I prefer to study, and what language am I using for my coursework?" "${OFF}"
[ "$(sse "${OFF}" memory_count)" = "0" ] \
  || fail "a disabled memory was still retrieved: $(sse "${OFF}" memories)"

# Switched off, not deleted: still listed, and listed as disabled.
[ "$(curl -s "${API}/api/v1/memories" -H "${AUTH}" | json 'd["count"]')" = "${MEM_COUNT}" ] \
  || fail "disabling a memory removed it from the list"
[ "$(curl -s "${API}/api/v1/memories?enabled=true" -H "${AUTH}" | json 'd["count"]')" = "0" ] \
  || fail "a disabled memory is still listed as enabled"
ok "disabled memories are out of retrieval and still on the list"

step "Forgetting everything"
curl -s -o /dev/null -w '%{http_code}' -X DELETE "${API}/api/v1/memories" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{"confirm":false}' | grep -q 400 \
  || fail "an unconfirmed clear was not rejected"
[ "$(curl -s "${API}/api/v1/memories" -H "${AUTH}" | json 'd["count"]')" = "${MEM_COUNT}" ] \
  || fail "an unconfirmed clear deleted memories"

DELETED=$(curl -s -X DELETE "${API}/api/v1/memories" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{"confirm":true}' | json 'd["deleted"]')
[ "${DELETED}" = "${MEM_COUNT}" ] || fail "cleared ${DELETED} memories, want ${MEM_COUNT}"
[ "$(curl -s "${API}/api/v1/memories" -H "${AUTH}" | json 'd["count"]')" = "0" ] \
  || fail "memories survived a confirmed clear"
ok "confirmed clear removed all ${DELETED}"

step "Deleting the document"
curl -s -o /dev/null -w '%{http_code}' -X DELETE "${API}/api/v1/documents/${DOC_ID}" -H "${AUTH}" \
  | grep -q 204 || fail "delete did not return 204"
LEFT=$(psql "${DB}" -tAc 'SELECT count(*) FROM document_chunks')
[ "${LEFT}" = "0" ] || fail "${LEFT} chunks survived the deleted document"
ok "document deleted, chunks cascaded"

printf '\n\033[32mPASS\033[0m upload -> extract -> chunk -> embed -> store -> retrieve -> ask -> cite -> remember -> recall\n'
