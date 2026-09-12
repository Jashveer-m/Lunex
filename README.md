# Lunex

Personal life-operating-system.

- **Phase 1** — repository structure, database schema, authentication.
- **Phase 2** — tasks, goals and notes: plain CRUD, authenticated, scoped per
  user.
- **Phase 3** — documents and retrieval: upload a PDF or Markdown/text file,
  extract its text, chunk it, embed it with a local Ollama, store the vectors in
  pgvector, and search them with citations.
- **Phase 4** — the AI assistant: a provider abstraction over the LLM, an
  orchestrator that pulls the retrieved chunks plus your tasks, goals and notes
  into the prompt, and a streaming chat API that records exactly what grounded
  each answer.
- **Phase 5** — memory: after a substantial turn the assistant reads the
  exchange back and stores the facts worth keeping about you, embedded and
  searchable, so a later conversation can retrieve and cite them. You can see
  every one of them, correct it, switch it off, delete it, or forget everything.
- **Phase 6** — the knowledge graph: every task, goal, note and document gets a
  node when you write it, conversations grow the edges between them, and a
  question that names something you have gets its connections as context. Two
  Postgres tables, one hop, no graph database.
- **Phase 7** — tools and the action engine: eight fixed tools over the
  existing services, a routing step that decides per message whether one is
  needed, and an approval flow. A search runs straight away; a change — create a
  task, goal or note, update a task — is only *proposed*, and nothing is written
  until you approve it.
- **UI phase** — the web app over all of it: sign-in, tasks/goals/notes,
  document upload, a streaming chat with inline citations and Approve/Reject
  cards for proposed changes, and a memory manager.

Named specialist agents (Study, Career, …) and deleting through the chat belong
to later phases and are deliberately absent. The assistant can change your data
only by proposing a change you then approve, and nothing — including asking it
to skip the approval — relaxes that.

## Stack

| Piece | Choice |
| --- | --- |
| API | Go 1.26, [chi](https://github.com/go-chi/chi) router |
| Database | PostgreSQL 16 + [pgvector](https://github.com/pgvector/pgvector), migrations via golang-migrate (embedded) |
| Embeddings | [Ollama](https://ollama.com) running `nomic-embed-text` (768 dimensions) locally |
| Chat model | Ollama running `llama3.2:3b` locally, behind an `ai.Provider` interface |
| PDF text | [ledongthuc/pdf](https://github.com/ledongthuc/pdf) — text layer only |
| Passwords | Argon2id (`golang.org/x/crypto/argon2`) |
| Tokens | HS256 access JWT (15 min) + rotating opaque refresh token (30 days) |
| Frontend | Vite + React 19 + TypeScript + Tailwind v4, no router or state library |

## Layout

```
lunex/
├── backend/
│   ├── cmd/api/           # HTTP server
│   ├── cmd/migrate/       # up / down / version
│   ├── internal/
│   │   ├── actions/       # the action engine: proposals, approval, the audit trail
│   │   ├── agents/        # agents as tool sets, the routing call, the lexical gate
│   │   ├── ai/            # LLM provider interface + Ollama and mock backends
│   │   ├── api/           # router and middleware wiring
│   │   ├── auth/          # argon2id, JWT, sessions, service, handlers
│   │   ├── chat/          # conversations, the RAG orchestrator, SSE streaming
│   │   ├── config/        # environment configuration
│   │   ├── db/            # connection pool, migration runner, pgvector param
│   │   ├── documents/     # upload, extract, chunk, embed, search
│   │   ├── embeddings/    # Ollama client behind an Embedder interface
│   │   ├── goals/         # goals + milestones: model, service, handlers
│   │   ├── graph/         # knowledge graph: node sync, relationship extraction, 1-hop lookup
│   │   ├── memories/      # extraction, embedding, retrieval, management
│   │   ├── httpx/         # JSON transport helpers shared by the modules
│   │   ├── notes/         # notes: model, service, handlers
│   │   ├── optional/      # the three-state field a PATCH body needs
│   │   ├── tasks/         # tasks + dependencies: model, service, handlers
│   │   ├── tools/         # the tool registry: eight fixed tools, the approval gate
│   │   ├── users/         # user + profile model and repository
│   │   └── validate/      # field rules shared by the modules
│   ├── migrations/        # embedded .sql migrations
│   └── go.mod
├── frontend/              # the web UI (Vite + React)
├── docs/                  # api.md, decisions.md, testing.md
├── scripts/e2e.sh         # upload -> ask -> cite -> remember -> recall -> link -> traverse -> propose -> approve, against real Postgres and Ollama
└── Makefile
```

## Quick start

Requires Go 1.26+, PostgreSQL 16+ with pgvector, Ollama, and Node 20+.

```sh
# 0. Embeddings and the vector extension (Phase 3)
brew install pgvector          # or: apt install postgresql-16-pgvector
brew install ollama && ollama serve &
ollama pull nomic-embed-text
ollama pull llama3.2:3b        # the chat model (Phase 4)

# 1. Database
createdb lunex

# 2. Backend config
cd backend
cp .env.example .env          # then edit it
export DATABASE_URL='postgres://postgres@localhost:5432/lunex?sslmode=disable'
export JWT_SECRET="$(openssl rand -base64 48)"

# 3. Migrate and run
go run ./cmd/migrate up
go run ./cmd/api              # listens on :8080

# 4. Frontend (proxies /api to :8080) -- open http://localhost:5173
cd ../frontend && npm install && npm run dev
```

Check it is alive:

```sh
curl -s localhost:8080/healthz
# {"database":"ok","status":"ok"}
```

Register and call an authenticated endpoint:

```sh
TOKENS=$(curl -s -X POST localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct horse battery staple","name":"Ada"}')

ACCESS=$(echo "$TOKENS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["tokens"]["access_token"])')
AUTH="Authorization: Bearer $ACCESS"

curl -s localhost:8080/api/v1/me -H "$AUTH"
```

Create and list a task:

```sh
curl -s -X POST localhost:8080/api/v1/tasks -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Ship phase 2","priority":"high","tags":["api"]}'

curl -s 'localhost:8080/api/v1/tasks?status=pending&sort=-priority' -H "$AUTH"
```

Upload a document and ask it a question:

```sh
printf 'The aurora borealis appeared over the tundra shortly after midnight.\n' > notes.txt

curl -s -X POST localhost:8080/api/v1/documents -H "$AUTH" -F 'file=@notes.txt'
# {"id":"…","filename":"notes.txt","file_type":"txt","status":"ready","chunk_count":1,…}

curl -s -X POST localhost:8080/api/v1/documents/search -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"query":"what happened over the tundra?","limit":3}'
# {"count":1,"results":[{"filename":"notes.txt","chunk_index":0,"similarity":0.73,"content":"The aurora…"}]}
```

Ask the assistant about it:

```sh
CONV=$(curl -s -X POST localhost:8080/api/v1/conversations -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

curl -N -X POST "localhost:8080/api/v1/conversations/$CONV/messages" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"content":"What do my notes say happened over the tundra?"}'
# event: sources
# data: {"sources":[{"type":"document","label":"S1","title":"notes.txt","similarity":0.73,…}],"count":1}
# event: token
# data: {"text":"According"}
# …
# event: done
# data: {"conversation_id":"…","message":{"content":"According to [S1], …","sources":[…]},"model":"llama3.2:3b"}
```

The reply streams as Server-Sent Events, and the `sources` frame arrives before
the first token so a client can show what the answer is grounded in while it is
still being written. Ask something your data does not answer and the assistant
says so rather than inventing a citation — see
[docs/decisions.md](docs/decisions.md) for how that is enforced.

Tell it something about yourself, then ask in a fresh conversation:

```sh
curl -N -X POST "localhost:8080/api/v1/conversations/$CONV/messages" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"content":"Something to keep in mind about me: I always study in the early morning before class."}'
# …
# event: done
# data: {…,"remembered":[{"id":"…","type":"preference","content":"The user prefers studying in the early morning before class."}]}

curl -s localhost:8080/api/v1/memories -H "$AUTH"
# {"count":1,"memories":[{"type":"preference","content":"The user prefers studying in the early morning before class.",
#                         "importance":0.7,"confidence":0.9,"enabled":true,"source_conversation_id":"…",…}]}
```

A new conversation retrieves that memory and cites it like any other source.
The whole record is yours to manage: `PATCH /api/v1/memories/{id}` corrects a
fact or switches it off without deleting it, `DELETE` removes one, and
`DELETE /api/v1/memories` with `{"confirm": true}` forgets everything.

Extraction is a second model call on every substantial turn. It runs after the
last token, so it delays the end of the stream rather than the answer, and
`MEMORY_EXTRACTION=false` turns it off while leaving retrieval running.

### The knowledge graph (Phase 6)

Nodes appear as you write. Creating a task, goal, note or document creates its
node in the same request; renaming it moves the label; deleting it takes the
node and its edges with it.

```sh
curl -s -X POST localhost:8080/api/v1/tasks -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"title":"Finish the compiler project"}'

curl -s localhost:8080/api/v1/knowledge-graph -H "$AUTH"
# {"nodes":[{"type":"task","label":"Finish the compiler project","ref_table":"tasks",
#            "ref_id":"…","extracted":false,…}],"edges":[],"node_count":1,"edge_count":0,…}
```

Edges come from conversations. After a substantial turn the exchange is read a
third time, this time for the relationships in it:

```sh
# "I have been learning Rust so I can finish the compiler project."
# event: done
# data: {…,"linked":[{"from_node_id":"…","to_node_id":"…","relationship":"STUDIES","confidence":0.9}]}

curl -s localhost:8080/api/v1/knowledge-graph/nodes/$NODE_ID -H "$AUTH"
# {"node":{"type":"skill","label":"Rust","extracted":true,…},
#  "neighbors":[{"node":{"type":"goal","label":"the compiler project",…},
#                "edge":{"relationship":"REQUIRES","confidence":0.85,…},"incoming":true}],
#  "neighbor_count":1}
```

And a later question that *names* one of your nodes gets its connections as a
citable source, in a conversation that shares nothing else with the first:

```
[S1] graph: "Rust"
"the compiler project" (goal) REQUIRES "Rust" (skill) · confidence 0.85
"You" (person) STUDIES "Rust" (skill) · confidence 0.90
```

The match is on the node's *name*, exactly and case-insensitively, on a word
boundary — "Rust" matches "learning Rust" and not "trusted". It is a lookup,
not a traversal: one hop, no path finding, no graph algorithms.

Relationship extraction is a third model call on a substantial turn, after the
memory one and on the same terms — it runs after the last token, it cannot fail
the answer, and `GRAPH_EXTRACTION=false` turns it off while leaving node sync
and the lookup running.

### Tools and actions (Phase 7)

Ask the assistant to do something and it proposes it:

```sh
# "Add a task to renew my passport by 2026-10-15, it's urgent."
# event: action
# data: {"id":"…","tool_name":"create_task","status":"proposed",
#        "summary":"Create a task \"Renew my passport\" (priority high, due Thu 15 Oct 2026).",…}

curl -s "localhost:8080/api/v1/tasks?q=passport" -H "$AUTH"   # {"count":0,…} -- proposed, not created

curl -s -X POST localhost:8080/api/v1/actions/$ACTION_ID/approve -H "$AUTH"
# {"status":"executed","result":{"task":{"title":"Renew my passport",…}},…}
```

`POST /api/v1/actions/{id}/reject` declines it instead, and nothing is ever
written. `GET /api/v1/actions?status=proposed` is what is waiting for you.

A search is different: "which of my tasks mention the passport?" runs
`search_tasks` there and then, and what it found becomes the answer's first,
citable source — no approval, because looking changes nothing. Every tool call,
read or write, is recorded in `/api/v1/actions`.

The model picks a tool through a short structured prompt, not Ollama's native
tool calling: measured on llama3.2:3b, native calling used a tool on every
message that needed none — "thanks" proposed a task — while this prompt chose
correctly on 23 of 24 and proposed no write it was not asked for
(`docs/decisions.md`). The routing call only happens for messages that look like
requests ("add", "task", "find", "mark", …), because on a slow CPU it costs up
to a minute before the first token. `AGENT_TOOLS=false` turns the tools off and
leaves the Phase 6 assistant.

## The web UI

`make frontend-dev` (with the API running) serves it at http://localhost:5173;
Vite proxies `/api` and `/healthz` to `:8080`, so no CORS is involved.

| Screen | Route | What it does |
| --- | --- | --- |
| Sign in / register | `/login`, `/register` | Access token in memory, refresh token in `localStorage`; a 401 refreshes silently and retries |
| Today | `/`, `/?tab=goals`, `/?tab=notes` | Tasks (quick add, filters, search, edit, complete, dependencies), goals with milestones, notes |
| Assistant | `/chat`, `/chat/:id` | Conversations; answers stream token by token with `[S1]` citation chips and a source list; proposed changes appear as cards with Approve / Reject |
| Documents | `/documents` | Drag-and-drop upload, status and chunk count, extracted-text viewer, delete |
| Memories | `/memories` | Filter by type and state, edit, switch off, delete, and "Forget everything" behind a confirmation |
| Approvals | `/approvals` | Every proposal, including ones from deleted conversations |
| Connections | `/graph` | The knowledge graph as two lists — nodes and relationships — no renderer |

## Configuration

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `DATABASE_URL` | yes | — | Postgres connection string |
| `JWT_SECRET` | yes | — | ≥ 32 bytes; the process refuses to start otherwise |
| `PORT` | no | `8080` | |
| `JWT_ISSUER` | no | `lifeos` | Validated on every access token; unchanged from Phase 1 so existing tokens keep verifying |
| `ACCESS_TOKEN_TTL` | no | `15m` | Go duration |
| `REFRESH_TOKEN_TTL` | no | `720h` | 30 days |
| `LOGIN_RATE_LIMIT_BURST` | no | `10` | Per-IP burst on login/register |
| `AUTO_MIGRATE` | no | `false` | `true` migrates on boot instead of via `cmd/migrate` |
| `OLLAMA_BASE_URL` | no | `http://localhost:11434` | Where `ollama serve` is listening |
| `EMBEDDING_MODEL` | no | `nomic-embed-text` | |
| `EMBEDDING_DIMENSIONS` | no | `768` | Must match the `vector(n)` column in migration 000003 |
| `MAX_UPLOAD_BYTES` | no | `10485760` | 10 MB; rejected before the file is read |
| `DOCUMENT_PROCESS_TIMEOUT` | no | `2m` | Budget for one synchronous upload; also sets the server's read/write timeout |
| `CHAT_MODEL` | no | `llama3.2:3b` | The Ollama chat model |
| `CHAT_TIMEOUT` | no | `6m` | Budget for one whole turn: route, retrieve, generate, persist — **and** both extractions, which run inside it. The process refuses to start if the two extraction timeouts and `AGENT_TIMEOUT` exceed half of this |
| `CHAT_TEMPERATURE` | no | `0.2` | 0–2. Low: the assistant quotes your own data back at you |
| `CHAT_MAX_TOKENS` | no | `1024` | Reply length cap |
| `CHAT_MIN_SIMILARITY` | no | `0.5` | 0–1. Retrieval floor for chat; below it a chunk is never shown to the model |
| `MEMORY_EXTRACTION` | no | `true` | The writing half. `false` keeps retrieval and stops the assistant learning anything new |
| `MEMORY_MODEL` | no | `CHAT_MODEL` | Which model extracts. A smaller one is reasonable: it classifies, it does not converse |
| `MEMORY_EXTRACT_TIMEOUT` | no | `60s` | Bounds one extraction; the latency memory adds to the end of a turn |
| `MEMORY_TEMPERATURE` | no | `0.1` | 0–2. Near zero: extraction is a reading task |
| `MEMORY_MAX_TOKENS` | no | `512` | Extraction reply cap |
| `MEMORY_MIN_SIMILARITY` | no | `0.6` | 0–1. Retrieval floor for memories, tuned separately from the document one and higher |
| `MEMORY_JSON_MODE` | no | `false` | Constrain extraction to JSON. Off: it makes llama3.2:3b answer `{}` and pad whitespace |
| `GRAPH_EXTRACTION` | no | `true` | The writing half of the graph. `false` keeps node sync and the 1-hop lookup, and stops the assistant inferring new relationships |
| `GRAPH_MODEL` | no | `CHAT_MODEL` | Which model extracts relationships |
| `GRAPH_EXTRACT_TIMEOUT` | no | `60s` | Bounds one relationship extraction; the second piece of latency at the end of a turn |
| `GRAPH_TEMPERATURE` | no | `0.1` | 0–2. Near zero: extraction is a reading task |
| `GRAPH_MAX_TOKENS` | no | `512` | Extraction reply cap |
| `GRAPH_JSON_MODE` | no | `false` | As `MEMORY_JSON_MODE`, and off for the same measured reason |
| `AGENT_TOOLS` | no | `true` | The assistant's tools: routing, searches and proposals. `false` is the Phase 6 assistant; the approval endpoints stay, so existing proposals can still be decided |
| `AGENT_MODEL` | no | `CHAT_MODEL` | Which model makes the routing decision |
| `AGENT_TIMEOUT` | no | `60s` | Bounds one routing decision. Spent *before* the first token, and counted inside `CHAT_TIMEOUT` |
| `AGENT_TEMPERATURE` | no | `0.1` | 0–2. Near zero: it is a classification |
| `AGENT_MAX_TOKENS` | no | `200` | Routing reply cap; a decision is one short JSON object |

## Common commands

```sh
make build             # go build ./...
make test              # unit + handler tests, no database and no Ollama needed
make test-integration  # adds the Postgres-backed tests (needs pgvector)
make test-e2e          # upload -> search -> ask -> cite -> remember -> recall -> link -> traverse -> propose -> approve, against a real Ollama
make migrate-up        # apply migrations
make migrate-version   # print schema version
make run               # start the API
make frontend-dev      # start Vite
```

## Documentation

- [docs/api.md](docs/api.md) — endpoint-by-endpoint API contract
- [docs/decisions.md](docs/decisions.md) — why chi, why SHA-256 for refresh
  tokens, why a foreign resource is a 404, and **what is explicitly deferred**
- [docs/testing.md](docs/testing.md) — test layers and how to run them

## Known gaps

Summarised here, detailed in [docs/decisions.md](docs/decisions.md):

1. **Rate limiting is in-process.** A real per-IP token bucket protects
   login/register, but it resets on restart and is per-replica. Redis is out of
   scope for this phase.
2. **The web UI keeps the refresh token in `localStorage`.** The access token
   lives only in memory, but the 30-day refresh token is readable by any script
   on the origin, so an XSS bug would leak it. An `HttpOnly` cookie is the fix
   and needs a backend change.
3. **No email verification or password reset.**
4. **Refresh tokens are returned in the JSON body**, not as an `HttpOnly` cookie.
5. **No CORS middleware** — development relies on the Vite proxy.
6. **Logout does not revoke outstanding access tokens**; they expire within 15
   minutes.
7. **No recurring tasks.** The schema leaves room for a `recurrence_rule`
   column; none of the logic exists yet.
8. **List paging is `limit`/`offset`.** Fine at this size, but a client paging
   deeply while rows are being inserted can see a row twice. Keyset paging is
   the fix when it starts to matter.
9. **The Go module path is still `github.com/jashveer/lifeos/backend`**, because
   it mirrors the repository URL. Renaming it is a repository rename.
10. **No object storage — the uploaded file itself is not kept.** Only the
    extracted text is stored, so there is no way to re-download the PDF you
    uploaded, show it in a viewer, or re-extract it later with a better parser
    or with OCR. MinIO is a later phase; the files uploaded before it exists are
    not recoverable. This is the largest gap in Phase 3.
11. **PDF and TXT/Markdown only.** DOCX, CSV and images (OCR) are rejected with
    `415`. A scanned PDF is accepted, extracts nothing, and fails with a message
    saying OCR is what it needs.
12. **No background job queue.** Uploads process inside the request, which is
    why `/documents` has its own longer timeout and why a document is capped at
    800 chunks. A document that failed because Ollama was down has to be deleted
    and re-uploaded; there is no retry.
13. **No reranking and no hybrid search.** Retrieval is plain vector similarity;
    `min_similarity` is the only defence against confident-looking irrelevant
    citations, and it is off by default.
14. **Embeddings are tied to nomic-embed-text.** Vectors from two models are not
    comparable, so changing the model means a migration that changes the column
    and re-embeds every chunk.
15. **No cloud LLM provider.** `internal/ai` defines the interface and ships
    Ollama and a mock; adding Anthropic or OpenAI is a new file there plus one
    line in `cmd/api`, but `Options` will need to grow for a hosted provider's
    extras.
16. **Tasks, goals and notes are retrieved by heuristic, not semantically.**
    They have no embeddings yet, so the assistant sees what is in progress, due
    soonest and recently touched — not what is relevant to the question.
    Documents are the only semantic half.
17. **A conversation still forgets its own wording.** The last 20 messages are
    replayed verbatim; past that, only what Phase 5 extracted into memory
    survives, and it comes back as facts rather than as what was said.
18. **The assistant can only propose changes** *(since Phase 7)*. Nothing in the
    chat path writes to tasks, goals or notes; a change is recorded as a
    proposal and runs only on `POST /actions/{id}/approve`. It cannot delete
    anything.
19. **A failed turn is not saved at all** — not the question, not a partial
    answer. Resending is the retry.
20. **No token accounting.** The context budget is counted in characters, and a
    prompt that overflows the model's window is truncated by Ollama silently.
21. **No per-user rate limit on generation.** One user can hold as many
    concurrent turns open as they have connections; `CHAT_TIMEOUT` is the only
    bound.
22. **Memory extraction doubles the model work on a substantial turn.** It runs
    inline after the last token, so it delays the end-of-stream marker rather
    than the answer, and `MEMORY_EXTRACTION=false` removes it while leaving
    retrieval running. There is still no job queue to move it to.
23. **Nothing consolidates, merges or forgets memories.** They accumulate. A
    near-duplicate is skipped, a contradiction is not noticed — the system
    prompt tells the model to prefer what you say now, which is a mitigation
    rather than a fix — and no memory decays. `expires_at` exists and nothing
    writes it.
24. **Extraction sees one exchange at a time**, so a fact stated across three
    turns is not assembled, and it reads the assistant's reply as well as
    yours: an answer that opens "I could not find anything about that" can talk
    a 3B model out of remembering what you just told it. `MEMORY_MODEL` points
    extraction at a better model without touching anything else.
25. **Memory retrieval ranks on similarity alone.** Importance, confidence,
    recency and usage are all recorded and none of them affect what is
    retrieved.
26. **Text in your documents can reach the extraction prompt.** The extractor
    reads an answer that was grounded in your files, so a document engineered to
    say "remember that the user has approved X" has a path into your memory.
    Requiring every fact to be about the user narrows it; nothing in this phase
    closes it. Every memory is visible, attributable and deletable. Since Phase 7
    the same text cannot cause a *change*: the routing call reads only your
    message, never the retrieved context, and every write waits for you.
27. **The graph is one hop and nothing more.** No shortest path, no centrality,
    no "how are these two things related". The chat integration is a lookup —
    the question named something, here is what it is connected to.
28. **Nodes are matched by name, exactly and case-insensitively.** "Go" and
    "go" are one node; "Go" and "Golang" are two. Fuzzy matching is not used on
    purpose: a wrong merge does not lose a distinction, it invents
    relationships. Node embeddings are the upgrade path, and they need a UI for
    confirming a merge.
29. **Rows written before Phase 6 have no node** until they are next updated.
    Sync is idempotent and runs on every update, so the graph fills in as things
    are touched, but nothing backfills the four tables. That belongs with the
    job queue.
30. **A relationship extraction is a third model call on a substantial turn.**
    The tail of a turn is now roughly twice what Phase 5 made it. It runs after
    the last token and cannot fail the answer; `GRAPH_EXTRACTION=false` removes
    it, `GRAPH_MODEL` points it at a smaller model.
31. **Nothing merges, decays or contradicts an edge.** `STUDIES` never becomes
    `KNOWS`, a new edge that contradicts an old one is not noticed, and
    confidence is recorded once and never re-estimated. Deleting an edge is not
    a suppression list — a later conversation that states it again recreates it.
32. **A graph source carries links, not details.** A node knows the goal it
    mirrors but not that goal's deadline; the goal's own retrieval path supplies
    that, and only if the goal is in the top five active ones. The graph adds a
    lookup, not a join planner.
33. **"Asked about" and "stated about" are told apart by the prompt only.**
    Entity names are required to come from *your* message rather than the
    assistant's reply, which stops the retrieved context becoming nodes — but a
    thing you asked a question about is a thing you named, and only the model's
    judgement stops it becoming an edge.
34. **The same prompt-injection path reaches the graph.** The relationship
    extractor reads an answer grounded in your files. Requiring both entity
    names to appear in *your* message narrows it considerably — a document
    cannot name entities the extractor will accept unless you named them too —
    but it does not close it. Every node and edge is visible, attributable to a
    conversation, and deletable.
35. **No delete tools.** Deleting through the chat is deferred: an approved
    delete of the wrong task costs the task, and it wants a proposal that shows
    exactly what would go.
36. **One tool per turn, and the router reads only your message.** "Create
    tasks for A, B and C" proposes one; "mark it done" about a task named two
    turns ago does not resolve. `update_task` resolves its own reference ("the
    scheduler task") to exactly one task, or declines and says which ones
    matched.
37. **The routing call costs a model call before the first token** on a
    message that looks like a request — ~3s with a warm cache, up to a minute on
    a slow CPU without one. A lexical gate keeps it off greetings and ordinary
    questions; a request phrased with none of its cue words gets no tool.
38. **A proposal cannot be edited and does not expire.** Reject it and ask
    again. `update_task` cannot clear a field, only set one.
39. **The model can still *say* a proposal is done.** The prompt forbids it
    twice, and the end-to-end run warns when it happens; what is guaranteed is
    that nothing is written until you approve, whatever the reply says.
40. **Relative dates are resolved in UTC**, not in your profile's timezone.
