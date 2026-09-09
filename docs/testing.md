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
| Config loading | `internal/config/config_test.go` | no |
| Migrations, constraints, cascades, atomic rotation | `internal/db/integration_test.go` | **yes** |
| Phase 2 SQL: filters, sorting, partial updates, dependency cycles | `internal/db/phase2_integration_test.go` | **yes** |
| Phase 3 SQL: pgvector round trip, cosine ordering, cascades | `internal/db/phase3_integration_test.go` | **yes** |
| Cross-user isolation over the whole stack, documents included | `internal/api/isolation_test.go` | **yes** |
| The whole pipeline against a real Ollama | `scripts/e2e.sh` | **yes**, plus Ollama |

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
documents and chunks all cascade from `users`.

The Postgres-backed tests need the `vector` extension available; migration
000003 runs `CREATE EXTENSION IF NOT EXISTS vector`, which fails on a server
where pgvector is not installed:

```sh
brew install pgvector        # or: apt install postgresql-16-pgvector
```

None of the Go tests need Ollama. The document tests use a deterministic
bag-of-words embedder (`internal/api/embedder_test.go`) so that what they
measure is the routing, the SQL scoping and the pipeline's own logic — not
whether a model understood a sentence.

## The end-to-end check

`scripts/e2e.sh` is the one that uses nothing fake. It boots the API against
`TEST_DATABASE_URL`, registers a user, uploads a small text file, searches for a
phrase from it, and asserts the chunk comes back with the right filename and a
similarity in `(0, 1]` — then deletes the document and checks the chunks went
with it.

```sh
ollama serve &                       # if it is not already running
ollama pull nomic-embed-text         # once
make test-e2e
```

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
