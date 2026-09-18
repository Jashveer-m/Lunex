#!/usr/bin/env bash
# End-to-end check for the Phase 3 retrieval pipeline, the Phase 4 assistant,
# the Phase 5 memory system, the Phase 6 knowledge graph and the Phase 7 action
# engine.
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
# Then the graph check: create a task and confirm its node appeared; state a
# relationship between two things in one conversation and confirm the edge was
# extracted; ask about one end of it in a *new* conversation and confirm the
# graph context surfaces; and confirm the delete rules -- a mirrored node is
# refused, an extracted one is not, and deleting the task takes its node.
#
# Then the action check: ask the assistant to create a task and confirm it was
# proposed, not created; approve it and confirm the task now exists; ask for
# another and reject it, and confirm nothing was created; and ask it to find a
# task, which must run the search without asking anybody.
#
# Then the calendar check, which is the same rule for Phase 8: ask the
# assistant to schedule something and confirm it was proposed and the calendar
# is still empty; approve it and confirm the event is in GET /calendar with the
# time that was proposed; confirm its graph node appeared; and ask what is on
# that day, which must run search_calendar and cite the event.
#
# Then the finance check, which is the same rule for Phase 9 with one addition:
# ask the assistant to log an expense and confirm it was proposed and nothing
# was recorded; approve it and confirm it is in GET /expenses to the penny;
# confirm its graph node appeared; ask how much has been spent this month and
# confirm analyze_spending ran and its total is the one the API reports; and
# ask what to do with the money, which must be answered without advising.
#
# E2E_ONLY=actions runs the preflight, registration and the action check and
# nothing else -- a few minutes rather than most of an hour on a slow machine.
# E2E_ONLY=calendar does the same for the calendar check, and E2E_ONLY=finance
# for the finance one.
#
# Nothing here is mocked: real Postgres, real pgvector, real Ollama, real
# generation, real extraction.
#
#   ./scripts/e2e.sh
#
# Requires: a Postgres with the `vector` extension available, and Ollama
# serving nomic-embed-text and llama3.2:3b.
#
# On a slow machine the two extractions are what fails first, and they fail
# silently -- an extraction that misses its deadline is logged and dropped, so
# the symptom is an empty /memories or an empty graph rather than an error. One
# extraction against llama3.2:3b has been measured at 79s on a 2019 Intel Mac,
# against a 60s default. Raise the budgets if the extraction steps come back
# empty; they are passed through to the API:
#
#   MEMORY_EXTRACT_TIMEOUT=180s GRAPH_EXTRACT_TIMEOUT=180s AGENT_TIMEOUT=180s \
#     CHAT_TIMEOUT=18m ./scripts/e2e.sh
#
# CHAT_TIMEOUT has to cover the whole turn including both extractions and the
# routing call, and the API refuses to start if it does not leave them room --
# so raise them together or none.
#
# The run also holds one access token from registration to the last assertion,
# and since Phase 6 that span routinely exceeds the 15-minute default TTL --
# which shows up as a step failing to parse a response that is in fact a clean
# 401. The API is started with a 2h TTL for that reason, overridable with
# E2E_ACCESS_TOKEN_TTL. Refreshing mid-run would be the other fix and would
# make every later assertion depend on the refresh path working.
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

# expect <what> <body> <expression> -- read one field out of a JSON body, and
# fail with the body itself when it is not there.
#
# Without this, a response that is valid JSON of the wrong shape -- a 401 after
# a long run, a 500, a validation error -- surfaces as a Python KeyError
# traceback with the body nowhere in the output, which says nothing about what
# actually went wrong. Every assertion below that reaches into a response goes
# through it.
expect() {
  local what="$1" body="$2" expr="$3" out
  if ! out=$(printf '%s' "${body}" | python3 -c "import sys,json;d=json.load(sys.stdin);print(${expr})" 2>/dev/null); then
    fail "${what}: the response was not what this expects -- ${body:-<empty>}"
  fi
  printf '%s' "${out}"
}

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
# Something already listening on the port would answer healthz in place of the
# API being tested -- which is exactly what a server left behind by an earlier
# run did, silently, until this check existed.
if curl -s --max-time 2 -o /dev/null "${API}/healthz"; then
  fail "something is already listening on :${PORT} -- a server from an earlier run? Stop it, or set E2E_PORT"
fi
LOG="$(mktemp)"
# Built first and run directly, so API_PID is the server itself. With `go run`
# it was the go tool, and killing that on exit left the compiled server
# listening -- to be mistaken for the next run's.
BIN="$(mktemp -d)/api"
(cd "${ROOT}/backend" && go build -o "${BIN}" ./cmd/api) || fail "the API does not build"
(cd "${ROOT}/backend" && \
  DATABASE_URL="${DB}" \
  JWT_SECRET="e2e-secret-that-is-comfortably-over-32-bytes-long" \
  PORT="${PORT}" \
  ACCESS_TOKEN_TTL="${E2E_ACCESS_TOKEN_TTL:-2h}" \
  OLLAMA_BASE_URL="${OLLAMA}" \
  CHAT_MODEL="${CHAT_MODEL}" \
  CHAT_TIMEOUT="${CHAT_TIMEOUT:-}" \
  MEMORY_EXTRACT_TIMEOUT="${MEMORY_EXTRACT_TIMEOUT:-}" \
  GRAPH_EXTRACT_TIMEOUT="${GRAPH_EXTRACT_TIMEOUT:-}" \
  AGENT_TIMEOUT="${AGENT_TIMEOUT:-}" \
  exec "${BIN}" >"${LOG}" 2>&1) &
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

# --- the stream helpers ---------------------------------------------------------
#
# The reply arrives as Server-Sent Events, so the assertions below are made
# over parsed frames rather than over raw text: `sse` pulls one fact out of a
# captured stream.
sse() { python3 - "$1" "$2" <<'PYEOF'
import sys, json
path, want = sys.argv[1], sys.argv[2]
events, name = [], None
raw = open(path).read()
for line in raw.split("\n"):
    line = line.rstrip("\r")
    if line.startswith("event: "):
        name = line[7:]
    elif line.startswith("data: "):
        events.append((name, json.loads(line[6:])))
if not events:
    # Not a stream at all: the request was refused before the first frame, so
    # the body is an ordinary JSON error and it is the only thing that says
    # why. Without this the caller sees an empty answer and a -1 source count
    # and has nothing to go on.
    sys.exit("the request produced no stream: %s" % (raw.strip()[:400] or "<empty response>"))
by = lambda n: next((d for k, d in events if k == n), None)
answer = "".join(d["text"] for k, d in events if k == "token")
done, err, srcs = by("done"), by("error"), by("sources")
if err:
    sys.exit("stream failed: %s" % err)
sources = (done or {}).get("message", {}).get("sources", [])
memories = [s for s in sources if s["type"] == "memory"]
graph = [s for s in sources if s["type"] == "graph"]
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
    # Phase 6: what was retrieved from the graph, what of it the answer used,
    # and what the turn itself added to the graph.
    "graph_count": len(graph),
    "graph": " | ".join(s["excerpt"].replace("\n", " ; ") for s in graph),
    "graph_titles": ",".join(s["title"] for s in graph),
    "graph_cited": len([s for s in graph if s["cited"]]),
    "linked": " | ".join("%s -%s-> %s" % (l["from_node_id"][:8], l["relationship"], l["to_node_id"][:8])
                         for l in (done or {}).get("linked", [])),
    "linked_count": len((done or {}).get("linked", [])),
    # Phase 7: the action frames, in stream order, and what done repeated;
    # and the sources a read tool found.
    "action_frames": len([d for k, d in events if k == "action"]),
    "action": json.dumps(next((d for k, d in events if k == "action"), {})),
    "action_after_tokens": all(i > max([j for j, (k, _) in enumerate(events) if k == "token"] or [-1])
                               for i, (k, _) in enumerate(events) if k == "action"),
    "done_actions": len((done or {}).get("actions", [])),
    "tool_sources": ",".join("%s:%s" % (s.get("tool"), s["title"]) for s in sources if s.get("tool")),
    "tool_cited": len([s for s in sources if s.get("tool") and s["cited"]]),
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

# E2E_ONLY=actions, =calendar and =finance skip straight to their own check.
if [ -z "${E2E_ONLY:-}" ]; then

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
MEM_COUNT=$(expect "listing memories" "${MEMS}" 'd["count"]')
[ "${MEM_COUNT}" -ge 1 ] || {
  grep -q "extraction failed" "${LOG}" && \
    printf '  \033[33mhint\033[0m the extraction missed its deadline; raise MEMORY_EXTRACT_TIMEOUT (see the header of this script)\n' >&2
  fail "nothing was extracted from an exchange stating a durable fact: ${MEMS}"
}

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
# Re-read the list first. The recall turn above was itself a substantial
# exchange, so extraction ran on it too and may have stored another memory --
# and disabling the snapshot taken before it would leave that one live, which
# looks exactly like a disabled memory still being retrieved.
MEMS=$(curl -s "${API}/api/v1/memories" -H "${AUTH}")
MEM_COUNT=$(echo "${MEMS}" | json 'd["count"]')

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

# --- Phase 6: the knowledge graph -------------------------------------------
#
# Three properties, in the order the brief names them: a created task has a
# node; a conversation that states a relationship grows an edge; and a *new*
# conversation that names one end of that edge is handed the connection.

step "Creating a task and checking its node exists"
TASK_ID=$(curl -s -X POST "${API}/api/v1/tasks" -H "${AUTH}" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Finish the compiler project","priority":"high","status":"in_progress"}' \
  | json 'd["id"]')
[ -n "${TASK_ID}" ] || fail "the task was not created"

GRAPH=$(curl -s "${API}/api/v1/knowledge-graph" -H "${AUTH}")
expect "reading the graph" "${GRAPH}" 'd["node_count"]' >/dev/null
echo "${GRAPH}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
task_id = sys.argv[1]
nodes = [n for n in d["nodes"] if n["ref_table"] == "tasks" and n["ref_id"] == task_id]
assert len(nodes) == 1, "the task has %d nodes, want 1: %s" % (len(nodes), d["nodes"])
n = nodes[0]
assert n["type"] == "task", "node type is %r" % n["type"]
assert n["label"] == "Finish the compiler project", "node label is %r" % n["label"]
assert n["extracted"] is False, "a synced node reports itself as extracted"
' "${TASK_ID}" || fail "the task did not get a graph node: ${GRAPH}"

# The document uploaded earlier is mirrored too -- sync is not a task-only path.
echo "${GRAPH}" | json '",".join(n["ref_table"] or "" for n in d["nodes"])' | grep -q documents \
  || fail "the uploaded document has no node: ${GRAPH}"
ok "the task and the document each have exactly one node"

step "Renaming the task and checking the node follows"
curl -s -o /dev/null -X PATCH "${API}/api/v1/tasks/${TASK_ID}" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{"title":"Finish the Lunex compiler project"}'
GRAPH=$(curl -s "${API}/api/v1/knowledge-graph" -H "${AUTH}")
expect "reading the graph after the rename" "${GRAPH}" 'd["node_count"]' >/dev/null
echo "${GRAPH}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
task_id = sys.argv[1]
nodes = [n for n in d["nodes"] if n["ref_id"] == task_id]
assert len(nodes) == 1, "renaming made %d nodes" % len(nodes)
assert nodes[0]["label"] == "Finish the Lunex compiler project", "label is %r" % nodes[0]["label"]
' "${TASK_ID}" || fail "the node did not follow the rename: ${GRAPH}"
ok "one node, carrying the new title"

step "Stating a relationship in a conversation"
# A goal by the name the conversation will use, so the extracted entity has an
# existing node to resolve onto rather than making a parallel one. This is the
# entity-matching property, checked below.
GOAL_ID=$(curl -s -X POST "${API}/api/v1/goals" -H "${AUTH}" \
  -H 'Content-Type: application/json' \
  -d '{"title":"the compiler project","type":"project"}' | json 'd["id"]')
[ -n "${GOAL_ID}" ] || fail "the goal was not created"

REL_CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
STATING_REL="$(mktemp)"
ask "${REL_CONV}" "Some background on what I am doing: I have been learning Rust this term specifically so that I can finish the compiler project, and my flatmate Priya has been helping me with the borrow checker." "${STATING_REL}"
sse "${STATING_REL}" answer >/dev/null || fail "the stating turn failed"
ok "stated it, assistant answered ($(sse "${STATING_REL}" linked_count) relationship(s) reported on the turn)"
printf '     %s\n' "$(sse "${STATING_REL}" answer | tr '\n' ' ' | head -c 200)"

step "Checking what was extracted"
GRAPH=$(curl -s "${API}/api/v1/knowledge-graph" -H "${AUTH}")
EDGE_COUNT=$(expect "reading the graph" "${GRAPH}" 'd["edge_count"]')
[ "${EDGE_COUNT}" -ge 1 ] || {
  grep -q "relationship extraction failed" "${LOG}" && \
    printf '  \033[33mhint\033[0m the extraction missed its deadline; raise GRAPH_EXTRACT_TIMEOUT (see the header of this script)\n' >&2
  fail "nothing was extracted from an exchange stating a relationship: ${GRAPH}"
}

# Every edge is well formed: a relationship from the closed set, a confidence in
# range and above the floor, provenance recorded, and both ends real nodes of
# this user's -- the columns the CHECK constraints and the quality gate guard.
echo "${GRAPH}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
rels = {"RELATED_TO","REQUIRES","DEPENDS_ON","WORKS_ON","KNOWS","INTERESTED_IN","STUDIES","COMPLETED","GOAL_OF"}
kinds = {"task","goal","note","document","skill","person","project"}
ids = {n["id"] for n in d["nodes"]}
for n in d["nodes"]:
    assert n["type"] in kinds, "unknown node type %r" % n["type"]
    assert n["label"].strip(), "a node has no label: %r" % n
    assert len(n["label"].split()) <= 8 or not n["extracted"], "an extracted node is a sentence: %r" % n["label"]
    assert (n["ref_table"] is None) == (n["ref_id"] is None), "half a reference: %r" % n
    assert n["extracted"] == (n["ref_table"] is None), "extracted disagrees with ref_table: %r" % n
for e in d["edges"]:
    assert e["relationship"] in rels, "unknown relationship %r" % e["relationship"]
    assert 0.5 <= e["confidence"] <= 1, "confidence out of range or under the floor: %r" % e["confidence"]
    assert e["source_conversation_id"], "edge has no provenance: %r" % e
    assert e["from_node_id"] in ids and e["to_node_id"] in ids, "dangling edge: %r" % e
    assert e["from_node_id"] != e["to_node_id"], "self edge: %r" % e
' || fail "an extracted node or edge is malformed: ${GRAPH}"

# The entity match: "the compiler project" resolved onto the goal that already
# had that name rather than creating a second, parallel node for it.
echo "${GRAPH}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
goal_id = sys.argv[1]
same = [n for n in d["nodes"] if n["label"].lower() == "the compiler project"]
assert len(same) == 1, "%d nodes are called the compiler project: %s" % (len(same), same)
' "${GOAL_ID}" || fail "the extracted entity did not resolve onto the existing goal: ${GRAPH}"

ok "extracted ${EDGE_COUNT} relationship(s) over $(echo "${GRAPH}" | json 'd["node_count"]') node(s)"
echo "${GRAPH}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
by = {n["id"]: n for n in d["nodes"]}
for e in d["edges"]:
    f, t = by[e["from_node_id"]], by[e["to_node_id"]]
    print("     %-24s %-14s %-24s  %.2f" % (
        "%s (%s)" % (f["label"][:16], f["type"]), e["relationship"],
        "%s (%s)" % (t["label"][:16], t["type"]), e["confidence"]))
'

step "Asking about it in a different conversation"
# A brand new conversation: no shared history, so any connection the assistant
# knows here came out of the graph. The question names an entity by the label a
# node carries, which is what the mention scan matches on.
NAMED=$(echo "${GRAPH}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
# The self node is never matched by a mention -- in a message the user wrote,
# "you" means the assistant (docs/decisions.md) -- so it is not a node a
# question can name. Picking it, which happens whenever the model attaches most
# of its edges to the user, would make this step unpassable by design.
self_ids = {n["id"] for n in d["nodes"] if n["extracted"] and n["type"] == "person" and n["label"] == "You"}
linked = ({e["from_node_id"] for e in d["edges"]} | {e["to_node_id"] for e in d["edges"]}) - self_ids
# The node with the most edges, so the neighbourhood is worth showing.
counts = {i: 0 for i in linked}
for e in d["edges"]:
    for end in (e["from_node_id"], e["to_node_id"]):
        if end in counts:
            counts[end] += 1
best = max(counts, key=counts.get)
print(next(n["label"] for n in d["nodes"] if n["id"] == best))
')
[ -n "${NAMED}" ] || fail "no connected node to ask about: ${GRAPH}"

GRAPH_CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
GRECALL="$(mktemp)"
ask "${GRAPH_CONV}" "Remind me how ${NAMED} fits in with everything else I have going on." "${GRECALL}"

GRAPH_RETRIEVED=$(sse "${GRECALL}" graph_count)
[ "${GRAPH_RETRIEVED}" -ge 1 ] \
  || fail "the new conversation retrieved no graph context for ${NAMED}: types=$(sse "${GRECALL}" types)"
sse "${GRECALL}" graph_titles | grep -qi "${NAMED}" \
  || fail "the graph source is not the node the question named: $(sse "${GRECALL}" graph_titles)"
# The excerpt is the node's links, one whole triple per line.
sse "${GRECALL}" graph | grep -qE '\) (RELATED_TO|REQUIRES|DEPENDS_ON|WORKS_ON|KNOWS|INTERESTED_IN|STUDIES|COMPLETED|GOAL_OF) ' \
  || fail "the graph source carries no rendered relationship: $(sse "${GRECALL}" graph)"
ok "retrieved ${GRAPH_RETRIEVED} graph source(s) in a fresh conversation, $(sse "${GRECALL}" graph_cited) cited"
printf '     %s\n' "$(sse "${GRECALL}" graph | head -c 240)"
printf '     %s\n' "$(sse "${GRECALL}" answer | tr '\n' ' ' | head -c 260)"

step "Reading one node's neighbourhood"
NODE_ID=$(echo "${GRAPH}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
label = sys.argv[1]
print(next(n["id"] for n in d["nodes"] if n["label"] == label))
' "${NAMED}")
HOOD=$(curl -s "${API}/api/v1/knowledge-graph/nodes/${NODE_ID}" -H "${AUTH}")
[ "$(echo "${HOOD}" | json 'd["neighbor_count"]')" -ge 1 ] \
  || fail "the node has no neighbours: ${HOOD}"
echo "${HOOD}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
me = d["node"]["id"]
for nb in d["neighbors"]:
    e = nb["edge"]
    # `incoming` has to agree with the edge it describes, or the direction the
    # model is shown is backwards.
    assert nb["incoming"] == (e["to_node_id"] == me), "incoming disagrees with the edge: %r" % nb
    assert nb["node"]["id"] in (e["from_node_id"], e["to_node_id"]), "the far end is not on the edge"
    assert nb["node"]["id"] != me, "a node is its own neighbour"
' || fail "a neighbour is malformed: ${HOOD}"
ok "${NAMED} has $(echo "${HOOD}" | json 'd["neighbor_count"]') neighbour(s), each with a consistent direction"

step "Checking the delete rules"
# A node that mirrors a record is refused, and says what to delete instead.
TASK_NODE=$(echo "${GRAPH}" | python3 -c '
import sys, json
print(next(n["id"] for n in json.load(sys.stdin)["nodes"] if n["ref_table"] == "tasks"))
')
CODE=$(curl -s -o /tmp/e2e-node-delete.json -w '%{http_code}' \
  -X DELETE "${API}/api/v1/knowledge-graph/nodes/${TASK_NODE}" -H "${AUTH}")
[ "${CODE}" = "409" ] || fail "deleting a mirrored node returned ${CODE}, want 409: $(cat /tmp/e2e-node-delete.json)"
grep -q node_is_backed /tmp/e2e-node-delete.json \
  || fail "the refusal does not name the reason: $(cat /tmp/e2e-node-delete.json)"

# An extracted node is the user's to remove, and its edges go with it.
EXTRACTED=$(echo "${GRAPH}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
linked = {e["from_node_id"] for e in d["edges"]} | {e["to_node_id"] for e in d["edges"]}
print(next(n["id"] for n in d["nodes"] if n["extracted"] and n["id"] in linked))
')
BEFORE_EDGES=$(echo "${GRAPH}" | json 'd["edge_count"]')
CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  -X DELETE "${API}/api/v1/knowledge-graph/nodes/${EXTRACTED}" -H "${AUTH}")
[ "${CODE}" = "204" ] || fail "deleting an extracted node returned ${CODE}, want 204"
AFTER=$(curl -s "${API}/api/v1/knowledge-graph" -H "${AUTH}")
[ "$(echo "${AFTER}" | json 'd["edge_count"]')" -lt "${BEFORE_EDGES}" ] \
  || fail "the deleted node's edges survived it: ${AFTER}"
ok "a mirrored node is refused with 409, an extracted one deletes with its edges"

step "Deleting the task and watching its node go"
curl -s -o /dev/null -w '%{http_code}' -X DELETE "${API}/api/v1/tasks/${TASK_ID}" -H "${AUTH}" \
  | grep -q 204 || fail "deleting the task did not return 204"
LEFT=$(psql "${DB}" -tAc "SELECT count(*) FROM knowledge_nodes WHERE ref_table = 'tasks' AND ref_id = '${TASK_ID}'")
[ "${LEFT}" = "0" ] || fail "${LEFT} nodes survived the deleted task"
# And the trigger fires on a cascade too, which is the case no Go code is on.
psql "${DB}" -q -c 'DELETE FROM goals' >/dev/null
LEFT=$(psql "${DB}" -tAc "SELECT count(*) FROM knowledge_nodes WHERE ref_table = 'goals'")
[ "${LEFT}" = "0" ] || fail "${LEFT} goal nodes survived a delete the API never saw"
ok "the node went with the task, and with a goal deleted straight from SQL"

step "Deleting the document"
curl -s -o /dev/null -w '%{http_code}' -X DELETE "${API}/api/v1/documents/${DOC_ID}" -H "${AUTH}" \
  | grep -q 204 || fail "delete did not return 204"
LEFT=$(psql "${DB}" -tAc 'SELECT count(*) FROM document_chunks')
[ "${LEFT}" = "0" ] || fail "${LEFT} chunks survived the deleted document"
ok "document deleted, chunks cascaded"

fi # E2E_ONLY

# --- Phase 7: tools and the action engine ---------------------------------------
#
# The brief's check, in its order: ask the assistant to create a task and
# confirm it is proposed, not created; approve it and confirm the task exists;
# ask for another and reject it, and confirm nothing was created. Then a read
# tool, which runs without asking.
#
# The task titles are chosen so no earlier step has created anything that
# matches them: the counts below are `?q=` searches for them, and mean exactly
# what they say.

# claims_done <answer> -- warn when the model says a proposal was carried out.
# It is a warning rather than a failure: what a 3B model writes is a matter of
# wording, and the property that matters -- nothing was created -- is asserted
# separately against the database. But it is exactly the lie rule 9 exists to
# prevent, so a run that shows it says so. Both the action check and the
# calendar check use it, so it is defined outside them.
# gives_advice <answer> -- warn when the answer tells the user what to do with
# their money, which rule 9 of the system prompt forbids.
#
# A warning rather than a failure, for the same reason claims_done is one: what
# a 3B model writes is a matter of wording, and the property that is actually
# enforced -- that the rule is in the prompt the model was given -- is asserted
# in internal/chat/finance_test.go. But it is exactly the sentence the rule
# exists to prevent, so a run that shows it says so.
gives_advice() {
  if echo "$1" | grep -qiE "(you should (invest|save|put|move|cut|reduce|spend|consider)|I('d| would) (recommend|suggest|advise)|my (advice|recommendation)|as (your|a) financial (adviser|advisor|planner)|you ought to (invest|save|cut))"; then
    printf '  \033[33mwarn\033[0m the answer reads as financial advice: %s\n' "$(echo "$1" | tr '\n' ' ' | head -c 200)"
  fi
}

claims_done() {
  if echo "$1" | grep -qiE "(I('ve| have) (created|added|scheduled|set up|booked)|has been (created|added|scheduled|booked)|is now (done|created|added|scheduled)|(it|that) is done|I created|I added|I scheduled|I booked)"; then
    printf '  \033[33mwarn\033[0m the answer describes the proposal as done: %s\n' "$(echo "$1" | tr '\n' ' ' | head -c 200)"
  fi
}

if [ -z "${E2E_ONLY:-}" ] || [ "${E2E_ONLY:-}" = "actions" ]; then

# task_count <q> -- how many of the user's tasks match a search.
task_count() {
  local body
  body=$(curl -s "${API}/api/v1/tasks?q=$1" -H "${AUTH}")
  expect "listing tasks matching $1" "${body}" 'd["count"]'
}

step "Asking the assistant to create a task"
ACT_CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
[ -n "${ACT_CONV}" ] || fail "conversation was not created"
[ "$(task_count passport)" = "0" ] || fail "a passport task exists before anything was asked for"

PROPOSE="$(mktemp)"
ask "${ACT_CONV}" "Add a task to renew my passport by 2026-10-15, it's urgent." "${PROPOSE}"
sse "${PROPOSE}" answer >/dev/null || fail "the proposing turn failed"
[ "$(sse "${PROPOSE}" action_frames)" = "1" ] || {
  grep -q "routing" "${LOG}" && tail -5 "${LOG}" >&2
  fail "the turn announced $(sse "${PROPOSE}" action_frames) action frame(s), want 1: $(sse "${PROPOSE}" answer | head -c 300)"
}
[ "$(sse "${PROPOSE}" action_after_tokens)" = "True" ] \
  || fail "the action frame arrived before the answer finished"
[ "$(sse "${PROPOSE}" done_actions)" = "1" ] || fail "done did not repeat the action"
ACTION="$(sse "${PROPOSE}" action)"
ACTION_ID=$(expect "reading the action frame" "${ACTION}" 'd["id"]')
echo "${ACTION}" | python3 -c '
import sys, json
a = json.load(sys.stdin)
assert a["tool_name"] == "create_task", "tool is %r" % a["tool_name"]
assert a["status"] == "proposed", "status is %r" % a["status"]
assert a["permission_level"] == "write", "permission is %r" % a["permission_level"]
assert "passport" in a["input"]["title"].lower(), "title is %r" % a["input"]["title"]
assert a["result"] is None and a["error_message"] is None, "a proposal has an outcome: %r" % a
' || fail "the proposal is not the task that was asked for: ${ACTION}"
ok "proposed: $(echo "${ACTION}" | json 'd["summary"]')"
printf '     %s\n' "$(sse "${PROPOSE}" answer | tr '\n' ' ' | head -c 260)"
claims_done "$(sse "${PROPOSE}" answer)"

step "Checking it was proposed, not created"
[ "$(task_count passport)" = "0" ] || fail "the task exists before it was approved"
PENDING=$(curl -s "${API}/api/v1/actions?status=proposed" -H "${AUTH}")
echo "${PENDING}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
assert sys.argv[1] in [a["id"] for a in d["actions"]], "the proposal is not listed as proposed"
' "${ACTION_ID}" || fail "GET /actions?status=proposed does not list it: ${PENDING}"
ok "no task yet; the action is listed as proposed"

step "Approving it"
APPROVED=$(curl -s -X POST "${API}/api/v1/actions/${ACTION_ID}/approve" -H "${AUTH}")
[ "$(expect "approving" "${APPROVED}" 'd["status"]')" = "executed" ] \
  || fail "approval did not execute: ${APPROVED}"
[ "$(task_count passport)" = "1" ] || fail "after approval there are $(task_count passport) passport tasks, want 1"
TASKS=$(curl -s "${API}/api/v1/tasks?q=passport" -H "${AUTH}")
echo "${TASKS}" | python3 -c '
import sys, json
t = json.load(sys.stdin)["tasks"][0]
a = json.loads(sys.argv[1])
assert t["id"] == a["result"]["task"]["id"], "the action names %r, the task is %r" % (a["result"]["task"]["id"], t["id"])
assert t["deadline"] and t["deadline"].startswith("2026-10-15"), "deadline is %r" % t["deadline"]
' "${APPROVED}" || fail "the created task is not the one approved: ${TASKS}"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${API}/api/v1/actions/${ACTION_ID}/approve" -H "${AUTH}")
[ "${CODE}" = "409" ] || fail "a second approval returned ${CODE}, want 409"
[ "$(task_count passport)" = "1" ] || fail "approving twice made a second task"
ok "the task exists: $(echo "${TASKS}" | json 'd["tasks"][0]["title"]') (due $(echo "${TASKS}" | json 'd["tasks"][0]["deadline"][:10]')); a second approval is 409"

step "Asking for another task and rejecting it"
[ "$(task_count dentist)" = "0" ] || fail "a dentist task exists before anything was asked for"
PROPOSE2="$(mktemp)"
ask "${ACT_CONV}" "Please create a task to book a dentist appointment." "${PROPOSE2}"
sse "${PROPOSE2}" answer >/dev/null || fail "the second proposing turn failed"
[ "$(sse "${PROPOSE2}" action_frames)" = "1" ] \
  || fail "the second turn announced $(sse "${PROPOSE2}" action_frames) action frame(s), want 1: $(sse "${PROPOSE2}" answer | head -c 300)"
ACTION2="$(sse "${PROPOSE2}" action)"
ACTION2_ID=$(expect "reading the second action frame" "${ACTION2}" 'd["id"]')
echo "${ACTION2}" | json 'd["input"]["title"]' | grep -qi dentist \
  || fail "the second proposal is not the dentist task: ${ACTION2}"
ok "proposed: $(echo "${ACTION2}" | json 'd["summary"]')"
claims_done "$(sse "${PROPOSE2}" answer)"

REJECTED=$(curl -s -X POST "${API}/api/v1/actions/${ACTION2_ID}/reject" -H "${AUTH}")
[ "$(expect "rejecting" "${REJECTED}" 'd["status"]')" = "rejected" ] || fail "rejection failed: ${REJECTED}"
[ "$(task_count dentist)" = "0" ] || fail "a rejected proposal created a task"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${API}/api/v1/actions/${ACTION2_ID}/approve" -H "${AUTH}")
[ "${CODE}" = "409" ] || fail "approving a rejected action returned ${CODE}, want 409"
[ "$(task_count dentist)" = "0" ] || fail "approving after rejecting created a task"
ok "rejected, and no dentist task exists -- not even after trying to approve it afterwards"

step "Asking it to find a task"
FIND="$(mktemp)"
ask "${ACT_CONV}" "Which of my tasks mention the passport?" "${FIND}"
sse "${FIND}" answer >/dev/null || fail "the searching turn failed"
sse "${FIND}" tool_sources | grep -qi "search_tasks:.*passport" \
  || fail "the search did not run or did not find the task: sources=$(sse "${FIND}" tool_sources) types=$(sse "${FIND}" types)"
[ "$(sse "${FIND}" action_frames)" = "1" ] || fail "the read was not announced"
echo "$(sse "${FIND}" action)" | python3 -c '
import sys, json
a = json.load(sys.stdin)
assert a["permission_level"] == "read" and a["status"] == "executed", "the read is %r" % a
' || fail "the read was not recorded as an executed read: $(sse "${FIND}" action)"
[ "$(task_count passport)" = "1" ] || fail "a read changed the tasks"
ok "search_tasks ran without approval and found $(sse "${FIND}" tool_sources); $(sse "${FIND}" tool_cited) cited"
printf '     %s\n' "$(sse "${FIND}" answer | tr '\n' ' ' | head -c 260)"

step "Reading the action log"
LOGGED=$(curl -s "${API}/api/v1/actions?conversation_id=${ACT_CONV}" -H "${AUTH}")
expect "listing the conversation's actions" "${LOGGED}" 'd["count"]' >/dev/null
echo "${LOGGED}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
by = sorted((a["tool_name"], a["status"]) for a in d["actions"])
assert ("create_task", "executed") in by, by
assert ("create_task", "rejected") in by, by
assert ("search_tasks", "executed") in by, by
assert not [a for a in d["actions"] if a["status"] in ("proposed", "approved")], "something is still pending: %r" % by
for a in d["actions"]:
    print("     %-12s %-9s %s" % (a["tool_name"], a["status"], a["summary"]))
' || fail "the action log is not what happened: ${LOGGED}"
ok "one executed create, one rejected create, one executed read, nothing pending"

fi # E2E_ONLY: actions only

# --- Phase 8: the calendar -------------------------------------------------------
#
# The brief's end-to-end check: ask the assistant to schedule something and
# confirm it was proposed rather than added, approve it, and confirm it is on
# the calendar the API returns. Then the read half: ask what is on that day and
# confirm search_calendar ran and the answer cites the event.
#
# The date is fixed and far enough out that no earlier step could have put
# anything on it, so the counts below mean exactly what they say.

if [ -z "${E2E_ONLY:-}" ] || [ "${E2E_ONLY:-}" = "calendar" ]; then

CAL_DAY="2026-11-19"   # a Thursday
CAL_WINDOW="start=${CAL_DAY}&end=2026-11-20"

# event_count -- how many events the user has on CAL_DAY.
event_count() {
  local body
  body=$(curl -s "${API}/api/v1/calendar?${CAL_WINDOW}" -H "${AUTH}")
  expect "listing the calendar for ${CAL_DAY}" "${body}" 'd["count"]'
}

step "Asking the assistant to schedule something"
CAL_CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
[ -n "${CAL_CONV}" ] || fail "conversation was not created"
[ "$(event_count)" = "0" ] || fail "something is already on ${CAL_DAY}"

SCHEDULE="$(mktemp)"
ask "${CAL_CONV}" "Add a dentist appointment to my calendar on ${CAL_DAY} at 3pm." "${SCHEDULE}"
sse "${SCHEDULE}" answer >/dev/null || fail "the scheduling turn failed"
[ "$(sse "${SCHEDULE}" action_frames)" = "1" ] || {
  fail "the turn announced $(sse "${SCHEDULE}" action_frames) action frame(s), want 1: $(sse "${SCHEDULE}" answer | head -c 300)"
}
CAL_ACTION="$(sse "${SCHEDULE}" action)"
CAL_ACTION_ID=$(expect "reading the action frame" "${CAL_ACTION}" 'd["id"]')
echo "${CAL_ACTION}" | python3 -c '
import sys, json
a = json.load(sys.stdin)
assert a["tool_name"] == "create_calendar_event", "tool is %r" % a["tool_name"]
assert a["status"] == "proposed", "status is %r" % a["status"]
assert a["permission_level"] == "write", "permission is %r" % a["permission_level"]
assert "dentist" in a["input"]["title"].lower(), "title is %r" % a["input"]["title"]
assert a["input"]["start"].startswith(sys.argv[1]), "start is %r" % a["input"]["start"]
assert a["result"] is None, "a proposal has a result: %r" % a
' "${CAL_DAY}" || fail "the proposal is not the event that was asked for: ${CAL_ACTION}"
ok "proposed: $(echo "${CAL_ACTION}" | json 'd["summary"]')"
printf '     %s\n' "$(sse "${SCHEDULE}" answer | tr '\n' ' ' | head -c 260)"
claims_done "$(sse "${SCHEDULE}" answer)"

step "Checking it was proposed, not added to the calendar"
[ "$(event_count)" = "0" ] || fail "the event is on the calendar before it was approved"
ok "nothing on ${CAL_DAY} yet"

step "Approving it"
CAL_APPROVED=$(curl -s -X POST "${API}/api/v1/actions/${CAL_ACTION_ID}/approve" -H "${AUTH}")
[ "$(expect "approving" "${CAL_APPROVED}" 'd["status"]')" = "executed" ] \
  || fail "approval did not execute: ${CAL_APPROVED}"
[ "$(event_count)" = "1" ] || fail "after approval there are $(event_count) events on ${CAL_DAY}, want 1"
CAL_EVENTS=$(curl -s "${API}/api/v1/calendar?${CAL_WINDOW}" -H "${AUTH}")
echo "${CAL_EVENTS}" | python3 -c '
import sys, json
e = json.load(sys.stdin)["events"][0]
a = json.loads(sys.argv[1])
assert e["id"] == a["result"]["event"]["id"], "the action names %r, the event is %r" % (a["result"]["event"]["id"], e["id"])
assert e["start_time"] == a["input"]["start"].replace("Z", ".000Z"), \
    "the event starts %r, the proposal said %r" % (e["start_time"], a["input"]["start"])
assert e["end_time"] > e["start_time"] or e["all_day"], "the event has no length: %r" % e
' "${CAL_APPROVED}" || fail "the created event is not the one approved: ${CAL_EVENTS}"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${API}/api/v1/actions/${CAL_ACTION_ID}/approve" -H "${AUTH}")
[ "${CODE}" = "409" ] || fail "a second approval returned ${CODE}, want 409"
[ "$(event_count)" = "1" ] || fail "approving twice made a second event"
ok "on the calendar: $(echo "${CAL_EVENTS}" | json 'd["events"][0]["title"]') at $(echo "${CAL_EVENTS}" | json 'd["events"][0]["start_time"]'); a second approval is 409"

step "Checking the event's graph node"
CAL_NODES=$(curl -s "${API}/api/v1/knowledge-graph?type=event" -H "${AUTH}")
echo "${CAL_NODES}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
events = [n for n in d["nodes"] if n["ref_table"] == "calendar_events"]
assert len(events) == 1, "%d event nodes, want 1" % len(events)
n = events[0]
assert n["ref_id"] == json.loads(sys.argv[1])["events"][0]["id"], "the node mirrors %r" % n["ref_id"]
print("     %s (%s)" % (n["label"], n["type"]))
' "${CAL_EVENTS}" || fail "the approved event has no graph node: ${CAL_NODES}"
ok "the event has a node, like every other mirrored row"

step "Asking what is on that day"
ONDAY="$(mktemp)"
ask "${CAL_CONV}" "What is on my calendar on ${CAL_DAY}?" "${ONDAY}"
sse "${ONDAY}" answer >/dev/null || fail "the calendar-reading turn failed"
sse "${ONDAY}" tool_sources | grep -qi "search_calendar:.*dentist" \
  || fail "the search did not run or did not find the event: sources=$(sse "${ONDAY}" tool_sources) types=$(sse "${ONDAY}" types)"
[ "$(sse "${ONDAY}" action_frames)" = "1" ] || fail "the read was not announced"
echo "$(sse "${ONDAY}" action)" | python3 -c '
import sys, json
a = json.load(sys.stdin)
assert a["permission_level"] == "read" and a["status"] == "executed", "the read is %r" % a
assert a["result"]["start"].startswith(sys.argv[1]), "it looked at %r, not the day asked about" % a["result"]["start"]
' "${CAL_DAY}" || fail "the read was not recorded as an executed read over that day: $(sse "${ONDAY}" action)"
[ "$(event_count)" = "1" ] || fail "a read changed the calendar"
ok "search_calendar ran without approval and found $(sse "${ONDAY}" tool_sources); $(sse "${ONDAY}" tool_cited) cited"
printf '     %s\n' "$(sse "${ONDAY}" answer | tr '\n' ' ' | head -c 260)"

fi # E2E_ONLY: calendar only

# --- Phase 9: the finance module -------------------------------------------------
#
# The brief's end-to-end check: ask the assistant to log an expense and confirm
# it was proposed rather than recorded; approve it and confirm it is in GET
# /expenses. Then the read half: ask how much has been spent this month and
# confirm analyze_spending ran, the answer is grounded in the real total, and
# the framing is informational rather than advice.
#
# The description is distinctive so the `?q=` counts below mean exactly what
# they say.

if [ -z "${E2E_ONLY:-}" ] || [ "${E2E_ONLY:-}" = "finance" ]; then

FIN_AMOUNT="1450.50"
FIN_WHAT="printer cartridges"

# expense_count -- how many of the user's expenses mention the description.
expense_count() {
  local body
  body=$(curl -s "${API}/api/v1/expenses?q=cartridges" -H "${AUTH}")
  expect "listing expenses matching cartridges" "${body}" 'd["count"]'
}

# spent_total -- the INR total the summary endpoint reports, as a string.
spent_total() {
  local body
  body=$(curl -s "${API}/api/v1/expenses/summary" -H "${AUTH}")
  echo "${body}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
inr = [c for c in d["currencies"] if c["currency"] == "INR"]
print("%.2f" % inr[0]["total"] if inr else "0.00")
'
}

step "Checking the default expense categories exist"
CATS=$(curl -s "${API}/api/v1/expense-categories" -H "${AUTH}")
echo "${CATS}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
names = sorted(c["name"] for c in d["categories"])
for want in ("Food", "Transport", "Housing", "Utilities", "Other"):
    assert want in names, "%s is missing from %r" % (want, names)
print("     %s" % ", ".join(names))
' || fail "registration did not seed the default categories: ${CATS}"
ok "a new user starts with the five default categories"

step "Asking the assistant to log an expense"
FIN_CONV=$(curl -s -X POST "${API}/api/v1/conversations" -H "${AUTH}" \
  -H 'Content-Type: application/json' -d '{}' | json 'd["id"]')
[ -n "${FIN_CONV}" ] || fail "conversation was not created"
[ "$(expense_count)" = "0" ] || fail "a cartridges expense exists before anything was asked for"
BEFORE_TOTAL="$(spent_total)"

FIN_LOG="$(mktemp)"
ask "${FIN_CONV}" "I spent ${FIN_AMOUNT} on ${FIN_WHAT} today, log it." "${FIN_LOG}"
sse "${FIN_LOG}" answer >/dev/null || fail "the expense-logging turn failed"
[ "$(sse "${FIN_LOG}" action_frames)" = "1" ] || {
  fail "the turn announced $(sse "${FIN_LOG}" action_frames) action frame(s), want 1: $(sse "${FIN_LOG}" answer | head -c 300)"
}
FIN_ACTION="$(sse "${FIN_LOG}" action)"
FIN_ACTION_ID=$(expect "reading the action frame" "${FIN_ACTION}" 'd["id"]')
echo "${FIN_ACTION}" | python3 -c '
import sys, json
a = json.load(sys.stdin)
assert a["tool_name"] == "create_expense", "tool is %r" % a["tool_name"]
assert a["status"] == "proposed", "status is %r" % a["status"]
assert a["permission_level"] == "write", "permission is %r" % a["permission_level"]
# The amount is exact in the stored input -- not 1450.4999999.
assert abs(a["input"]["amount"] - float(sys.argv[1])) < 1e-9, "amount is %r" % a["input"]["amount"]
assert "cartridge" in a["input"].get("description", "").lower(), "description is %r" % a["input"].get("description")
assert a["result"] is None, "a proposal has a result: %r" % a
' "${FIN_AMOUNT}" || fail "the proposal is not the expense that was asked for: ${FIN_ACTION}"
ok "proposed: $(echo "${FIN_ACTION}" | json 'd["summary"]')"
printf '     %s\n' "$(sse "${FIN_LOG}" answer | tr '\n' ' ' | head -c 260)"
claims_done "$(sse "${FIN_LOG}" answer)"

step "Checking it was proposed, not recorded"
[ "$(expense_count)" = "0" ] || fail "the expense exists before it was approved"
[ "$(spent_total)" = "${BEFORE_TOTAL}" ] \
  || fail "the total moved from ${BEFORE_TOTAL} to $(spent_total) before approval"
ok "nothing recorded yet; the total is unchanged at ${BEFORE_TOTAL}"

step "Approving it"
FIN_APPROVED=$(curl -s -X POST "${API}/api/v1/actions/${FIN_ACTION_ID}/approve" -H "${AUTH}")
[ "$(expect "approving" "${FIN_APPROVED}" 'd["status"]')" = "executed" ] \
  || fail "approval did not execute: ${FIN_APPROVED}"
[ "$(expense_count)" = "1" ] || fail "after approval there are $(expense_count) matching expenses, want 1"
FIN_EXPENSES=$(curl -s "${API}/api/v1/expenses?q=cartridges" -H "${AUTH}")
echo "${FIN_EXPENSES}" | python3 -c '
import sys, json
e = json.load(sys.stdin)["expenses"][0]
a = json.loads(sys.argv[1])
assert e["id"] == a["result"]["expense"]["id"], "the action names %r, the expense is %r" % (a["result"]["expense"]["id"], e["id"])
# Exact to the penny, through JSON, through numeric(12,2), and back.
assert "%.2f" % e["amount"] == sys.argv[2], "amount is %r, want %s" % (e["amount"], sys.argv[2])
assert e["currency"] == "INR", "currency is %r" % e["currency"]
' "${FIN_APPROVED}" "${FIN_AMOUNT}" || fail "the recorded expense is not the one approved: ${FIN_EXPENSES}"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${API}/api/v1/actions/${FIN_ACTION_ID}/approve" -H "${AUTH}")
[ "${CODE}" = "409" ] || fail "a second approval returned ${CODE}, want 409"
[ "$(expense_count)" = "1" ] || fail "approving twice recorded a second expense"
ok "recorded: $(echo "${FIN_EXPENSES}" | json 'd["expenses"][0]["description"]') at INR $(echo "${FIN_EXPENSES}" | json '"%.2f" % d["expenses"][0]["amount"]'); a second approval is 409"

step "Checking the expense's graph node"
FIN_NODES=$(curl -s "${API}/api/v1/knowledge-graph?type=expense" -H "${AUTH}")
echo "${FIN_NODES}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
nodes = [n for n in d["nodes"] if n["ref_table"] == "expenses"]
assert len(nodes) == 1, "%d expense nodes, want 1" % len(nodes)
n = nodes[0]
assert n["ref_id"] == json.loads(sys.argv[1])["expenses"][0]["id"], "the node mirrors %r" % n["ref_id"]
assert n["label"].strip().lower() != "expense", "the node label is the bare word %r" % n["label"]
print("     %s (%s)" % (n["label"], n["type"]))
' "${FIN_EXPENSES}" || fail "the approved expense has no graph node: ${FIN_NODES}"
ok "the expense has a node, like every other mirrored row"

step "Asking how much has been spent this month"
SPEND="$(mktemp)"
ask "${FIN_CONV}" "How much have I spent this month?" "${SPEND}"
sse "${SPEND}" answer >/dev/null || fail "the spending-analysis turn failed"
sse "${SPEND}" tool_sources | grep -qi "analyze_spending" \
  || fail "the analysis did not run: sources=$(sse "${SPEND}" tool_sources) types=$(sse "${SPEND}" types)"
[ "$(sse "${SPEND}" action_frames)" = "1" ] || fail "the read was not announced"
# The answer is grounded in the real total: the figure the endpoint reports is
# the figure the tool computed, and the model was handed it rather than adding
# anything up itself.
TOTAL="$(spent_total)"
echo "$(sse "${SPEND}" action)" | python3 -c '
import sys, json
a = json.load(sys.stdin)
assert a["permission_level"] == "read" and a["status"] == "executed", "the read is %r" % a
inr = [c for c in a["result"]["currencies"] if c["currency"] == "INR"]
assert inr, "the analysis reports no INR total: %r" % a["result"]
assert "%.2f" % inr[0]["total"] == sys.argv[1], "the tool totalled %r, the API says %s" % (inr[0]["total"], sys.argv[1])
assert "total" not in a["result"], "the analysis reports a total across currencies"
' "${TOTAL}" || fail "the analysis is not grounded in the recorded expenses: $(sse "${SPEND}" action)"
[ "$(expense_count)" = "1" ] || fail "a read changed the expenses"
ANSWER="$(sse "${SPEND}" answer)"
# The figure has to appear in the answer, in one of the ways a model writes it.
echo "${ANSWER}" | grep -qE "$(echo "${TOTAL}" | sed 's/[.]/[.]/g')|$(echo "${TOTAL}" | cut -d. -f1)" \
  || printf '  \033[33mwarn\033[0m the answer does not quote the total (%s): %s\n' \
       "${TOTAL}" "$(echo "${ANSWER}" | tr '\n' ' ' | head -c 200)"
# And it must not read as advice. This is a warning rather than a failure for
# the reason claims_done is: what a 3B model writes is a matter of wording, and
# the property that is actually enforced -- the rule is in the prompt -- is
# asserted in internal/chat/finance_test.go.
gives_advice "${ANSWER}"
ok "analyze_spending ran without approval over the real total (INR ${TOTAL}); $(sse "${SPEND}" tool_cited) cited"
printf '     %s\n' "$(echo "${ANSWER}" | tr '\n' ' ' | head -c 300)"

step "Asking what should be done with the money"
ADVICE="$(mktemp)"
ask "${FIN_CONV}" "Should I move my savings into an index fund?" "${ADVICE}"
sse "${ADVICE}" answer >/dev/null || fail "the advice turn failed"
ADVICE_ANSWER="$(sse "${ADVICE}" answer)"
gives_advice "${ADVICE_ANSWER}"
ok "answered without recommending what to do with the money"
printf '     %s\n' "$(echo "${ADVICE_ANSWER}" | tr '\n' ' ' | head -c 300)"

fi # E2E_ONLY: finance only

case "${E2E_ONLY:-}" in
  actions)  printf '\n\033[32mPASS\033[0m ask -> propose -> approve -> created ; ask -> propose -> reject -> nothing ; find\n' ;;
  calendar) printf '\n\033[32mPASS\033[0m ask -> propose -> approve -> on the calendar -> node -> read back\n' ;;
  finance)  printf '\n\033[32mPASS\033[0m ask -> propose -> approve -> recorded -> node -> totalled -> not advised\n' ;;
  *)        printf '\n\033[32mPASS\033[0m upload -> chunk -> embed -> retrieve -> ask -> cite -> remember -> recall -> link -> traverse -> propose -> approve -> reject -> schedule -> spend -> total\n' ;;
esac
