# Testing

## Layers

| Layer | Location | Needs a database |
| --- | --- | --- |
| Argon2id, JWT, refresh tokens, validation, rate limiter | `internal/auth/*_test.go` | no |
| Auth service use cases (in-memory fakes) | `internal/auth/service_test.go` | no |
| HTTP handlers and middleware | `internal/auth/handlers_test.go` | no |
| Full route tree over the real service | `internal/api/router_test.go` | no |
| Three-state PATCH fields | `internal/optional/optional_test.go` | no |
| Shared field rules | `internal/validate/validate_test.go` | no |
| Task / goal / note validation and use cases (fakes) | `internal/{tasks,goals,notes}/*_test.go` | no |
| Chunking, text extraction, filename and query rules | `internal/documents/{chunk,extract,validate}_test.go` | no |
| Document pipeline use cases (fake store + fake embedder) | `internal/documents/service_test.go` | no |
| Ollama client: batching, widths, outages | `internal/embeddings/ollama_test.go` | no |
| Chat provider: streaming, refusals, truncated streams | `internal/ai/ollama_test.go` | no |
| The mock provider's own contract | `internal/ai/mock_test.go` | no |
| Orchestrator: grounding, citations, failure modes (fakes + MockProvider) | `internal/chat/service_test.go` | no |
| Orchestrator: memory retrieval and extraction hand-off (fakes + MockProvider) | `internal/chat/memory_test.go` | no |
| Extraction parsing, the length threshold, the prompt | `internal/memories/extract_test.go` | no |
| Memory use cases: extraction quality, filters, dedup, re-embedding (fakes + MockProvider) | `internal/memories/service_test.go` | no |
| Memory validation: filters, patches, search bounds | `internal/memories/validate_test.go` | no |
| Prompt assembly, citation extraction, context budget | `internal/chat/prompt_test.go` | no |
| SSE framing, status codes, in-band errors | `internal/chat/handlers_test.go` | no |
| Config loading | `internal/config/config_test.go` | no |
| Migrations, constraints, cascades, atomic rotation | `internal/db/integration_test.go` | **yes** |
| Phase 2 SQL: filters, sorting, partial updates, dependency cycles | `internal/db/phase2_integration_test.go` | **yes** |
| Phase 3 SQL: pgvector round trip, cosine ordering, cascades | `internal/db/phase3_integration_test.go` | **yes** |
| Phase 4 SQL: jsonb sources, turn atomicity, message ordering, cascades | `internal/db/phase4_integration_test.go` | **yes** |
| Phase 5 SQL: memory vectors, the enabled/expired/unembedded filters, constraints, cascades | `internal/db/phase5_integration_test.go` | **yes** |
| Tool registry: the approval gate, canonical inputs, reference resolution, dates (fakes) | `internal/tools/*_test.go` | no |
| Routing: the gate, the prompt, the parser, the router's failure modes (MockProvider) | `internal/agents/agents_test.go` | no |
| Routing quality against a real model (opt-in: `LUNEX_ROUTING_EVAL=1`) | `internal/agents/ollama_eval_test.go` | no, but Ollama |
| Memory extraction against a real model: non-work preferences, proposals, restated documents (opt-in: `LUNEX_MEMORY_EVAL=1`) | `internal/memories/ollama_eval_test.go` | no, but Ollama |
| Action engine: approve/reject, single use, sanitized failures, strict bodies (in-memory store) | `internal/actions/*_test.go` | no |
| Orchestrator: proposals, reads as sources, the ACTIONS section, rule 8 (fakes + MockProvider) | `internal/chat/tools_test.go` | no |
| Phase 6 SQL: graph constraints, upserts, 1-hop queries, the delete trigger | `internal/db/phase6_integration_test.go` | **yes** |
| Phase 7 SQL: the approval gate, constraints, turn atomicity, cascades, the `q` filter | `internal/db/phase7_integration_test.go` | **yes** |
| Cross-user isolation over the whole stack, documents included | `internal/api/isolation_test.go` | **yes** |
| Phase 7 over HTTP: propose → approve/reject, concurrency, isolation | `internal/api/actions_isolation_test.go` | **yes** |
| The whole pipeline and the assistant against a real Ollama | `scripts/e2e.sh` | **yes**, plus Ollama |

## Running

```sh
# Everything that does not need Postgres:
cd backend && go test ./...

# Including the database-backed tests:
createdb lunex_test
cd backend && TEST_DATABASE_URL='postgres://postgres@localhost:5432/lunex_test?sslmode=disable' \
  go test ./... -count=1 -p 1
```

`-p 1` is required rather than merely tidy: `internal/db` and `internal/api`
both `TRUNCATE users` on the one test database, so letting their packages run
concurrently makes each delete the other's rows. `make test-integration` passes
it for you.

Or `make test` and `make test-integration` from the repository root.

Without `TEST_DATABASE_URL` the database-backed tests skip rather than fail, so
`go test ./...` is always green on a machine with no Postgres.

The integration tests migrate up and `TRUNCATE users CASCADE` before each test,
so point `TEST_DATABASE_URL` at a throwaway database — **never** at one holding
data you care about. One truncate is enough: tasks, goals, milestones, notes,
documents, chunks, conversations, messages and memories all cascade from
`users`.

The Postgres-backed tests need the `vector` extension available; migration
000003 runs `CREATE EXTENSION IF NOT EXISTS vector`, which fails on a server
where pgvector is not installed:

```sh
brew install pgvector        # or: apt install postgresql-16-pgvector
```

None of the Go tests need Ollama, except two that ask for it by name. The first,
`LUNEX_ROUTING_EVAL=1 go test ./internal/agents -run Ollama -v -timeout 30m`
runs the production routing gate, prompt and parser against a real model over
24 messages and fails on a write proposed for a message that asked for none, or
on accuracy under 85%. It is the reproducible form of the measurement in
`docs/decisions.md`. The second,
`LUNEX_MEMORY_EVAL=1 go test ./internal/memories -run Ollama -v -timeout 60m`,
runs the memory extraction prompt, parser and filters over non-work
preferences, a proposed change and a restated document, each
`LUNEX_MEMORY_EVAL_RUNS` times (default 3). The document tests use a deterministic
bag-of-words embedder (`internal/api/embedder_test.go`) and the chat and memory
tests use `ai.Mock`, so that what they measure is the routing, the SQL scoping
and the pipeline's own logic — not whether a model understood a sentence.
`ai.Mock` records every prompt it was given, which is how the grounding
assertions are made: whether the retrieved chunk reached the model, whether the
system prompt carried the rules, and whose data was in the context.

That boundary matters most in Phase 5. `internal/memories/service_test.go` runs
several example conversations through the extraction pipeline with a scripted
model, which pins *the pipeline's* judgement — the length threshold, the score
floors, the "is this about the user" check, the duplicate check, the provenance
— and pins nothing at all about whether llama3.2:3b picks good facts. That
question is only answerable against the real model, and it is asked in
`scripts/e2e.sh`.

The memory floor is likewise not tested against `hashingEmbedder`'s scores:
`internal/api/router_test.go` sets an explicit floor for those tests, because
a bag of words and nomic-embed-text do not produce numbers on the same scale,
and testing the production constant against the wrong scale would be testing
nothing. The floor itself is pinned in `internal/chat/memory_test.go` and
`internal/db/phase5_integration_test.go`, against numbers those tests choose.

## The end-to-end check

`scripts/e2e.sh` is the one that uses nothing fake. It boots the API against
`TEST_DATABASE_URL`, registers a user, uploads a small text file, searches for a
phrase from it, and asserts the chunk comes back with the right filename and a
similarity in `(0, 1]` — then deletes the document and checks the chunks went
with it.

Since Phase 4 it also asks the assistant two questions against a real
llama3.2:3b, which is the check that phase turned on:

- **a question the document answers** — the stream must lead with a `sources`
  frame naming `field-notes.txt`, the stored answer must mark that source
  `cited`, and the text must use what was retrieved;
- **a question nothing in the corpus answers** — nothing may be retrieved (the
  similarity floor), the answer may contain no `[S…]` citation, and it may not
  claim to have found anything in the user's documents.

It then reads the conversation back and checks both turns are stored in order,
with sources on the grounded answer only.

Since Phase 5 it goes on to the memory check, which is the one that cannot be
faked — nothing is shared between the two conversations below except the memory
itself:

- **a conversation that states a durable fact** — the user says they study in
  the early morning and are writing this term's coursework in Rust. Afterwards
  `GET /memories` must hold at least one memory, its content must be about what
  was said, and every memory must carry a type from the closed set, scores
  inside `[0, 1]`, `enabled: true` and the conversation it came from;
- **a different conversation asking about it** — a brand new conversation, so
  anything the assistant knows here came out of the memory store. It must
  retrieve at least one `memory` source, mark it `cited`, and use it in the
  answer;
- **switching the memories off** — a third conversation must retrieve none of
  them, while `GET /memories` still lists them and `?enabled=true` returns
  nothing: disabled is not deleted;
- **forgetting everything** — an unconfirmed `DELETE /memories` must be a `400`
  that deletes nothing, and a confirmed one must return the count and leave the
  list empty.

Since Phase 6 it ends with the knowledge-graph check, in the order the phase
builds it:

- **a task, and its node** — creating a task through the API must leave exactly
  one node keyed to it, typed `task`, labelled with its title and reporting
  `extracted: false`; the document uploaded earlier must have one too, because
  sync is not a task-only path. Renaming the task must move the label and not
  make a second node;
- **a conversation that states a relationship** — the user says they are
  learning Rust to finish the compiler project and that Priya is helping.
  Afterwards `GET /knowledge-graph` must hold at least one edge, and every node
  and edge is checked against what the schema and the quality gate promise: a
  relationship from the closed set, a confidence in `[0, 1]` and above the 0.5
  floor, provenance recorded, both endpoints real nodes of this user's, no self
  edge, no half reference, no extracted node longer than eight words;
- **entity matching** — a goal called "the compiler project" is created *before*
  the conversation, and afterwards exactly one node may carry that label: the
  extracted entity must have resolved onto the existing goal rather than
  creating a parallel idea of it;
- **a different conversation asking about it** — a brand new conversation, so
  any connection the assistant knows here came out of the graph. It must
  retrieve at least one `graph` source, the source must be the node the question
  named, and its excerpt must carry a rendered relationship;
- **one node's neighbourhood** — every neighbour's `incoming` must agree with
  the edge it describes, the far end must actually be on that edge, and no node
  may be its own neighbour;
- **the delete rules** — a mirrored node answers `409 node_is_backed`, an
  extracted one answers `204` and takes its edges, and deleting the task removes
  its node. Then a `DELETE FROM goals` issued straight to the database must
  remove the goal nodes too, which is the trigger doing the work no Go code is
  on the path for.

A real run looks like this:

```
==> Checking what was extracted
  ok extracted 1 memory/memories
     preference  0.70/0.90  The user prefers studying in the early morning before class.

==> Asking about it in a different conversation
  ok retrieved 1 memory/memories in a fresh conversation and cited one
     According to [S1], you prefer studying in the early morning.

==> Checking what was extracted
  ok extracted 2 relationship(s) over 4 node(s)
     You (person)             STUDIES        Rust (skill)              0.95
     the compiler pro (goal)  REQUIRES       Rust (skill)              0.85

==> Asking about it in a different conversation
  ok retrieved 1 graph source(s) in a fresh conversation, 1 cited
     "You" (person) STUDIES "Rust" (skill) · confidence 0.95 ; …
```

```sh
ollama serve &                       # if it is not already running
ollama pull nomic-embed-text         # once
ollama pull llama3.2:3b              # once
make test-e2e
```

Since Phase 7 it ends with the action check, which is the brief's:

- **asking the assistant to create a task** — "Add a task to renew my passport
  by 2026-10-15, it's urgent." The stream must carry exactly one `action` frame,
  after the last token and before `done`, for a `create_task` proposal whose
  title is about the passport; `done` must repeat it;
- **proposed, not created** — `GET /tasks?q=passport` must be empty and
  `GET /actions?status=proposed` must list the proposal;
- **approving it** — `executed`, and now exactly one passport task exists, due
  2026-10-15, the same task the action's `result` names. A second approval is
  `409` and creates nothing;
- **another, rejected** — a dentist task is proposed and rejected; no dentist
  task exists, and approving it afterwards is `409` and still creates nothing;
- **a read** — "Which of my tasks mention the passport?" must run `search_tasks`
  without any approval, surface the task as a `tool` source, and be recorded as
  an executed read;
- **the log** — the conversation's actions are one executed create, one
  rejected create and one executed read, and nothing is pending.

The script also **warns** (without failing) when an answer describes a proposal
as done. What a 3B model writes is wording; that nothing was created is asserted
against the database.

`E2E_ONLY=actions ./scripts/e2e.sh` runs only the preflight, registration and
this check.

The script builds the API and runs the binary directly, and refuses to start if
anything already answers on its port (`E2E_PORT`, default 8099). Before Phase 7
it started the server with `go run` and killed the `go run` process on exit,
which left the compiled server itself listening; the next run's server then
failed to bind, silently, and every assertion was made against the previous
run's build. It was found because an answer quoted wording the code no longer
contained.

A run against a cold model takes several minutes: the first turn includes
loading llama3.2:3b into memory, and since Phase 6 each substantial turn makes
*three* model calls rather than one (four since Phase 7, when the message looks
like it asks for a tool) — the answer, the memory extraction and the
relationship extraction, in that order and in sequence, because they all queue
behind the same resident model.

### When the extraction steps come back empty

This is the failure mode to know about, because it does not look like a
failure. An extraction that misses its deadline is logged and dropped — that is
the whole point of it not being able to break a turn — so a machine too slow
for `MEMORY_EXTRACT_TIMEOUT` produces a green chat run and then an empty
`/memories`, with nothing in the output saying why. The script now greps the
API log and prints a hint when that is what happened.

The defaults are 60 seconds each. One memory extraction against llama3.2:3b has
been measured at **79 seconds** on a 2019 Intel Mac — producing perfectly good
output, just not inside the budget. Both timeouts and the turn budget are
passed through to the API, so raise all three together:

```sh
MEMORY_EXTRACT_TIMEOUT=240s GRAPH_EXTRACT_TIMEOUT=240s AGENT_TIMEOUT=240s CHAT_TIMEOUT=24m \
  ./scripts/e2e.sh
```

`CHAT_TIMEOUT` has to cover the whole turn *including* both extractions and the
routing call, and `config.Load` refuses to start a process where they would take
more than half of it — so raising one without the others is a boot error rather than
a mystery.

One other thing got slower rather than merely longer: the run holds a single
access token from registration to the last assertion, and that span now
routinely exceeds the 15-minute default `ACCESS_TOKEN_TTL`. The symptom is not
a `401` in the output — it is a step failing to parse a response, because the
script asked for `edge_count` and got an error body. The API is started with a
two-hour TTL for the run (`E2E_ACCESS_TOKEN_TTL`), which is a property of the
harness rather than of the product.

The two extraction steps are also the ones that vary run to run, because what
they assert is what a 3B model chose to write down. The memory step has been
seen to fail when llama3.2:3b paraphrases the assistant's reply instead of the
user's statement — storing "the user values productivity and time management"
where the retrieval question needs "studies in the early morning", which then
scores ~0.55 against a 0.6 floor and retrieves nothing. When one of these steps
fails, read what `GET /memories` or `GET /knowledge-graph` actually holds before
looking anywhere else: an extraction that stored the wrong thing and a pipeline
that stored nothing look identical from the assertion.

## What the tests assert

Beyond the happy paths, the suite pins the security-relevant behaviour.

Phase 1:

- passwords are never stored or returned in plaintext, and every hash is salted;
- malformed, truncated, bcrypt and wrong-version hashes are rejected rather than
  verified;
- access tokens signed with another key, from another issuer, expired, or using
  `alg: none` are all refused;
- a refresh token is rotated on use and its predecessor is rejected on replay;
- logout invalidates exactly one session and is idempotent;
- an unknown email and a wrong password produce the same error;
- a refresh token cannot be used as an access token;
- rate limiting ignores a spoofed `X-Forwarded-For`;
- `ON DELETE CASCADE` removes profiles and sessions with their user.

Phase 2:

- **cross-user isolation**, end to end and against real SQL: user B gets `404`
  — not `403` — on GET, PATCH and DELETE of every one of user A's tasks, goals,
  notes and milestones; B's lists never contain A's rows; and A's data is
  verified unchanged afterwards, so a wrong status code cannot hide a write that
  actually landed (`internal/api/isolation_test.go`);
- a task cannot be parented by, or made to depend on, another user's task —
  both answer `404`, so neither confirms the foreign id exists;
- every Phase 2 route rejects a request with no access token;
- the authenticated user id, and nothing from the request body, is what reaches
  the store on every service call;
- `sort` only ever selects a fragment from a fixed map, so no request text
  reaches the `ORDER BY`, and an unknown value is a 400 rather than a silent
  fallback;
- a PATCH touches only the columns it names: an omitted key leaves the column
  alone, `null` clears a nullable one, empties a NOT NULL one, and is rejected
  where the column is required;
- an empty PATCH does not fire the `updated_at` trigger;
- a dependency cycle is refused, both directly (`a → b`, `b → a`) and
  transitively (`a → b → c`, `c → a`), and a self-dependency is refused by the
  service *and* by the table's CHECK constraint;
- `text[]` survives the driver round trip in order, and a task with no tags
  reads back as `[]` rather than NULL;
- deleting a user, a parent task or a goal cascades to everything hanging off it.

Phase 3:

- **cross-user isolation for documents**, end to end and against real SQL: user
  B gets `404` — not `403` — on GET and DELETE of A's document, B's list holds
  only B's rows, and, the part unique to this phase, B *searching for the exact
  words in A's file* gets none of A's chunks back. Naming A's document id in
  `document_ids` also returns nothing, so a foreign id is indistinguishable from
  an invented one;
- a control that makes those assertions non-vacuous: the owner searching the
  same corpus does get the chunk, with the filename, chunk index and a
  similarity score;
- deleting a document removes it from search, not merely from the list — the
  chunks are gone from the table;
- a file the phase cannot read (DOCX, CSV, PNG, or a `.pdf` that is not a PDF)
  is rejected with `415` **before** a row is created, so four rejected uploads
  leave zero documents;
- every document route rejects a request with no access token;
- the authenticated user id, and nothing from the request, is what reaches the
  store on every service call — including `Search`;
- a pipeline failure always leaves a document in `failed` with a reason, never
  in `processing`: unreadable PDF, no extractable text, embedding outage, an
  expired processing deadline, and a write failure after the embeddings arrived
  are each pinned separately;
- the stored `error_message` never contains the underlying error — a driver
  error naming a host and port does not reach the API;
- if even the "mark it failed" write fails, `Upload` returns an error saying the
  document is stuck, rather than a document that looks fine;
- a document too large to process synchronously fails as a whole and stores no
  chunks;
- chunk indexes are dense and ordered, so a citation can name one;
- chunking loses no words, overlaps by exactly `OverlapWords`, preserves the
  original line structure, never splits a rune, and terminates on input that has
  no whitespace at all;
- the Ollama client batches (one request for a document's chunks, not one per
  chunk), preserves input order across batch boundaries — which is what ties
  each vector to its chunk — refuses a model returning the wrong number of
  dimensions, and distinguishes a cancelled caller from a real outage;
- a 768-float `vector` survives the driver round trip exactly: a vector searched
  against itself scores 1, not merely close to it;
- cosine ordering is what the API reports — an identical vector scores 1, a 0.8
  blend scores 0.8, an orthogonal one scores 0 — and `min_similarity` filters on
  the same metric;
- only `ready` documents are searched;
- re-processing a document replaces its chunks rather than duplicating them;
- the `status` CHECK constraint rejects anything outside
  `processing`/`ready`/`failed`;
- deleting a document, or a user, cascades to every chunk.

Phase 4:

- **cross-user isolation for conversations**, end to end and against real SQL:
  user B gets `404` — not `403` — on GET, DELETE and *sending a message to* A's
  conversation, B's list holds only B's rows, and A's conversation is verified
  unchanged afterwards, so a wrong status code cannot hide a message that
  actually landed;
- the part unique to this phase: B asking the assistant *the exact words in A's
  document* retrieves nothing and gets an answer that says nothing was found —
  the retrieval path could leak another user's text without ever naming an id;
- the control that makes those non-vacuous: A asking the same question does
  retrieve the document, and the answer cites it with the right filename, id
  and label;
- an unrelated question retrieves nothing above the similarity floor and
  produces no citation — the grounding rule, end to end;
- `Cited` is read off the generated answer: a source that was offered and not
  referenced is recorded as uncited, and a label the model never saw is not
  matched;
- a bracketed group that is not a citation — a Markdown link, a bare `S1` in
  prose — does not mark anything as cited;
- every dependency (document search, task/goal/note lists, the conversation
  store) is called with the authenticated user id and nothing from the request;
- a failed turn persists nothing: a refused model call, a stream cut off
  partway, an empty reply, a failed retrieval and a client that hung up are
  each pinned separately, and each leaves the conversation exactly as it was;
- everything that fails before the first token is an ordinary JSON error with a
  real status code, and only a mid-generation failure is an in-band `error`
  event on an already-committed `200`;
- the streamed tokens reassemble into exactly the stored message content,
  newlines included;
- the model stream is a stream: a three-frame reply arrives as three pieces,
  and a body that ends without `done: true` is reported as a truncated answer
  rather than stored as a finished one;
- a cancelled caller is distinguished from a model outage, so an abandoned
  request is not logged as a 503;
- the retrieval floor and the top-k are applied to the document search, and the
  task/goal/note heuristic is the stated one;
- a turn is atomic: an invalid role on the second message rolls back the first
  *and* the conversation rename;
- the answer is never stored before its question, and a long conversation
  returns its most recent window in order;
- the `role` CHECK rejects anything outside `user`/`assistant`/`system`;
- `sources` survives the jsonb round trip whole, pointers included, and a user
  message reads back as NULL rather than `[]`;
- deleting a conversation, or a user, cascades to every message.

Phase 5:

- **cross-user isolation for memories**, end to end and against real SQL: user
  B gets `404` — not `403` — on PATCH, disable and DELETE of A's memory, B's
  list holds only B's rows, and B clearing *everything* deletes none of A's;
  A's memory is verified unchanged and still enabled afterwards, so a wrong
  status code cannot hide a write that landed;
- the part unique to this phase: B asking the assistant the question that
  retrieves A's memory gets nothing back and an answer that says so — the
  retrieval path could leak a fact about another user without ever naming an id;
- the control that makes those non-vacuous, and the property the phase exists
  for: a fact stated in one conversation is extracted, and a **different**
  conversation retrieves it, cites it, and the recorded source points at the
  stored memory by id;
- extraction never fails a turn: a failing extractor leaves the answer
  generated, streamed and stored, and the turn returns normally;
- nothing is extracted from a turn that was not persisted — a refused model call
  and a failed write are pinned separately;
- the length threshold is applied before the model is called at all, so a
  greeting costs no second round trip, and whitespace padding does not buy one;
- the four filters between the model and the table each drop what they are for:
  a low-confidence reading, a fact the model itself scored unimportant, a
  sentence copied out of the retrieved context rather than about the user, and a
  fact the user already has — while another user's identical memory is not
  treated as a duplicate of this one;
- the extraction parser accepts a bare array, a wrapper object, a single object,
  a fenced or prose-prefixed one and the documented line fallback, caps the
  count, clamps every score into the column's CHECK range, keeps the type inside
  the allow-list, and invents nothing from prose;
- JSON mode is off by default and sent when asked for, so the measured default
  cannot change by accident;
- retrieval uses the memory floor rather than the document floor, and the memory
  floor is the higher of the two;
- a disabled memory is invisible to retrieval and still on the list; an expired
  one and one that was never embedded are invisible too;
- editing a memory's text re-embeds it, so it becomes retrievable by what it now
  says and stops being retrievable by what it used to — verified through the API
  and again at the SQL layer — while toggling `enabled` costs no embedding call
  and does not lose the vector;
- clearing every memory is refused without `{"confirm": true}`, at the service
  as well as at the handler, and reports what it deleted;
- deleting a conversation keeps the memories learned in it, with a NULL
  provenance; deleting a user takes them.

Phase 6:

- **cross-user isolation for the knowledge graph**, end to end and against real
  SQL: user B gets `404` — not `403`, and not the `409` a mirrored node
  normally earns — on reading and deleting every one of A's nodes and edges;
  B's graph is empty; and A's node and edge counts are verified unchanged
  afterwards, so a wrong status code cannot hide a delete that landed;
- the part unique to this phase: B asking the assistant the question that
  matches A's node label gets no graph source back — the mention scan is a
  lookup over labels, and an unscoped one would hand B the shape of A's life
  without ever naming an id;
- the control that makes those non-vacuous, and the property the phase exists
  for: a relationship stated in one conversation is extracted, and a
  **different** conversation that names one end of it retrieves the
  neighbourhood, cites it, and the excerpt carries the rendered triple;
- **sync on write**, through the API and against real SQL: creating a task,
  goal, note or document creates exactly one node, keyed to the row and
  labelled with its title; renaming re-syncs rather than duplicating; a
  rejected write syncs nothing; and a service built without the option behaves
  exactly as it did before Phase 6;
- **the delete trigger**, which is the one piece of this phase with no Go on its
  path: deleting a parent task cascades to its subtasks in SQL, and *both*
  nodes and the edges between them go — verified by a delete that the task
  service is called once for and that removes two rows, and again by a
  `DELETE FROM goals` issued straight to the database;
- both unique indexes, as database properties rather than lucky interleavings:
  syncing the same task twice is one node whose label follows the title, `"Go"`
  and `"go"` are one node while a `person` and a `skill` both called Go are two,
  and the same triple stated in two conversations is one edge that keeps the
  higher confidence and the *first* conversation's provenance;
- every CHECK constraint pinned separately: an unknown node type, an unknown
  relationship, an unknown `ref_table`, half a reference (a table with no id or
  an id with no table), a confidence outside `[0, 1]`, and a node related to
  itself are all write errors rather than rows no client can render;
- the 1-hop query answers in both directions with the far end joined in, orders
  by confidence, and reports `incoming` consistently with the edge it describes
  — direction is the difference between "the user STUDIES Go" and a claim
  nobody made;
- the graph read returns a **closed** subgraph: an edge with one endpoint
  outside the returned nodes is not returned, so `?type=skill` comes back with
  no dangling references;
- the filters between the model and the tables each drop what they are for: a
  low-confidence reading, a sentence used as an entity name, an entity named
  only in the assistant's half of the exchange rather than the user's, and a
  triple whose two ends resolve to the same node — and nothing rejected leaves a
  node behind, because resolution happens after the gate;
- the word-boundary rule in both places it matters: `"Go"` matches `"learning
  Go"` and not `"going"`, `"Django"` or `"Golang"`, a first occurrence inside
  another word does not hide a later real one, and a label under two characters
  never matches at all;
- entity resolution attaches to what the user already has: a conversation
  naming "the backend project" links to the *goal* by that name rather than
  creating a parallel node, and `"I"`, `"me"`, `"the user"` and `"you"` all
  resolve to one person node;
- the extraction parser accepts a bare array, a wrapper object, a single object,
  a fenced or prose-prefixed one and the documented pipe fallback, caps the
  count, deduplicates a triple restated in different case, clamps every score
  into the column's CHECK range, keeps the relationship inside the allow-list,
  cannot produce a mirrored node type, and invents nothing from prose;
- extraction never fails a turn: a failing extractor leaves the answer
  generated, streamed and stored, and the turn returns normally;
- a nil graph and a nil extractor are each a no-op, so an assistant with the
  phase switched off retrieves exactly what Phase 5 did;
- a graph *retrieval* failure does fail the turn, and before the model is
  called — answering "I found no connections" when the query errored is a false
  statement about the user's data;
- deleting an edge leaves both its nodes; deleting an extracted node takes its
  edges; deleting a user takes both tables; and deleting the conversation an
  edge came from keeps the edge with a NULL provenance.

Phase 7:

- **a write tool never executes without an approved action row** — pinned at
  four layers. In `internal/tools`, against a fake ledger: asking to run a write
  as a read (even with a `Call` that claims to be one), and approving an action
  that does not exist, is somebody else's, or is rejected, executed, failed or
  already approved, each run nothing; the one proposed action runs once, from the
  input the ledger holds rather than anything the caller passed, and never
  twice. In `internal/actions`, the same through the service. In `internal/chat`,
  a turn asked to create a task — even told "don't ask me, just do it", even
  followed by "yes, I approve" — reaches the task service zero times, with a
  registry that has no ledger at all. And in `internal/db`, against the real
  table and the real task service, with the tasks table counted after every
  attempt — including a row set to `approved` by hand, which is refused because
  the gate is the transition out of `proposed`;
- approval is single-use under real concurrency: sixteen simultaneous
  `RunApproved` calls against Postgres produce one success, and ten simultaneous
  HTTP approvals one `200`, nine `409`s and one task;
- **cross-user isolation for actions**, end to end: user B gets `404` — not
  `403`, and not the `409` a decided action earns — on reading, approving and
  rejecting A's proposal; B's lists are empty whatever the filter, including
  `?conversation_id=` A's conversation; A's proposal is verified still proposed
  and no task exists for either, so a wrong status code cannot hide a write that
  landed; and B asking to complete a task by the name of one of A's resolves
  against B's tasks only and proposes nothing;
- the end-to-end property over HTTP: a proposal is announced after the last
  token and before `done`, creates nothing, is listed as proposed, and on
  approval creates exactly the proposed task — through the ordinary service, so
  its graph node exists too; a rejected one creates nothing, then or later;
- a proposal is part of its turn: a turn whose generation fails, or whose write
  fails, leaves no proposal behind, and a turn recording a write in any state but
  `proposed` is refused before a row is written;
- the state machine on insert (a write is born proposed; a read is born executed
  with a result or failed with a reason) and every CHECK constraint behind it —
  including that a read can never be proposed or approved;
- approve and reject take no parameters: an edited input, a confirm flag, two
  objects or the wrong content type are refused and the action is untouched;
- a tool that fails on approval leaves the action `failed` with a sentence, never
  the underlying error (a DSN in the error does not reach the response), and a
  client that hangs up the moment it approves does not strand the row;
- routing: a model naming a tool outside its agent, a tool that does not exist,
  or `run_sql` gets no tool; the router's own deadline and a refused call are a
  `503` before the stream starts, while a caller that hung up is not an outage;
  the parser accepts the shapes models write (flattened arguments, a native-style
  `name`/`parameters`, OpenAI's string arguments, a fence, a list of one) and
  invents nothing from prose;
- the gate passes every tool request of the routing measurement and none of its
  conversational messages, and a message it skips costs no routing call;
- canonical inputs: relative dates resolved against a fixed clock (a weekday is
  the next one, "next friday" and "friday" agree, 31 September is refused rather
  than rolled into October), priority and status synonyms mapped, placeholders
  treated as absent, the service's own validation applied at proposal time, and
  no key stored that the schema does not declare;
- `update_task` resolves a reference to exactly one of the caller's tasks — by
  exact title, phrase, then word — declines an ambiguous or unmatched one with
  the reason, refuses another user's task id as "no such task", and declines a
  change to what the task already has;
- reads: results become the turn's first sources marked with `tool`,
  deduplicated against the heuristic, cited when used, recorded as executed
  reads; a read that fails fails the turn;
- the model is told what became of earlier proposals — done, rejected, failed
  with the reason — and never about another user's;
- an identical pending proposal is reused rather than queued twice;
- without the Phase 7 dependencies (or with only some of them) the assistant is
  the Phase 6 assistant: the read-only rule, one model call, nothing recorded;
- the `q` filter is case-insensitive, owner-scoped and literal — `50%` finds the
  task containing it, `_` matches nothing it should not.
