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
| Cross-user isolation over the whole stack, documents included | `internal/api/isolation_test.go` | **yes** |
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

None of the Go tests need Ollama. The document tests use a deterministic
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

A real run looks like this:

```
==> Checking what was extracted
  ok extracted 1 memory/memories
     preference  0.70/0.90  The user prefers studying in the early morning before class.

==> Asking about it in a different conversation
  ok retrieved 1 memory/memories in a fresh conversation and cited one
     According to [S1], you prefer studying in the early morning.
```

```sh
ollama serve &                       # if it is not already running
ollama pull nomic-embed-text         # once
ollama pull llama3.2:3b              # once
make test-e2e
```

A run against a cold model takes a few minutes: the first turn includes loading
llama3.2:3b into memory, and since Phase 5 each substantial turn makes two model
calls rather than one.

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
