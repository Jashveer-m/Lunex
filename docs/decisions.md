# Phase 1 decisions

## Router: chi

`net/http`'s `ServeMux` can do method+path patterns since Go 1.22, but it has no
middleware chaining and no route groups. Phase 1 already needs three different
middleware stacks on one tree (global, rate-limited auth endpoints,
token-protected endpoints), and later phases add more. chi gives that with a
`http.Handler`-compatible API and no framework lock-in: every handler is still a
plain `http.HandlerFunc`, so dropping chi later is a routing-file change, not a
rewrite. It has one dependency (itself) and no reflection or codegen.

## Migrations: golang-migrate, as a library

Migration files live in `backend/migrations/` in golang-migrate's standard
`{version}_{name}.{up|down}.sql` format. They are embedded into the binary with
`go:embed`, and applied by `cmd/migrate` (or on boot with `AUTO_MIGRATE=true`).

Embedding rather than shelling out to the `migrate` CLI means a deploy carries
its own schema and there is no separate tool version to keep in sync — and, in
practice, it is why `make migrate-up` works on a machine that has never
installed golang-migrate.

## Password hashing: Argon2id

`golang.org/x/crypto/argon2` with the OWASP-recommended parameters (19 MiB,
t=2, p=1, 16-byte salt, 32-byte key). Hashes are stored in the standard PHC
string, which carries its own parameters:

```
$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
```

Because the parameters travel with each hash, raising the cost later does not
invalidate existing passwords: old hashes keep verifying with their recorded
parameters, and rehash-on-login can upgrade them.

## Refresh tokens: hashed with SHA-256, not Argon2id

Refresh tokens are 256 bits from `crypto/rand`. Unlike a password, there is no
dictionary to grind, so a memory-hard KDF buys nothing — and it would turn every
refresh into 19 MiB of work, which is a denial-of-service lever on an endpoint
that unauthenticated clients can call. SHA-256 is the right primitive for a
high-entropy secret. The database only ever sees the hash.

## Rotation is a single UPDATE

`SessionRepository.Rotate` swaps the hash in one `UPDATE ... WHERE
refresh_token_hash = $1 AND expires_at > now() RETURNING ...`. Two concurrent
uses of the same token cannot both succeed — the second matches zero rows — so
replay fails without needing a transaction or an advisory lock. Rotation keeps
the same session row, which preserves `device_info` and `created_at` across the
lifetime of a login.

## Logout does not revoke outstanding access tokens

Access tokens are stateless JWTs, so an already-issued one stays valid until it
expires — at most 15 minutes after logout. Checking a denylist on every request
would trade the main benefit of stateless tokens for a database round trip per
call. The 15-minute TTL is the mitigation. If Phase 2 needs immediate
revocation, the cheapest addition is a `token_version` column on `users`
included as a JWT claim and compared on parse.

## Uniform login failures

An unknown email and a wrong password return the same `401 invalid_credentials`.
The unknown-email path still runs a full Argon2id hash so the two cases are not
distinguishable by timing either.

## Email uniqueness is case-insensitive

The unique index is on `lower(email)` and the application lowercases before
writing, so `Ada@Example.com` and `ada@example.com` cannot both register.

---

# Phase 2 decisions

## Ownership lives in the WHERE clause, not in a check after the fact

Every repository method for tasks, goals and notes takes the owner id and puts
`user_id = $n` in the statement itself. Nothing loads a row and then compares
`row.UserID` to the caller.

The difference matters when someone forgets. A post-hoc check is a line that can
be omitted from a new method and still compile, still pass a happy-path test,
and still return the row. A `WHERE` clause is not optional in the same way: the
`Store` interfaces have no method that can be called without saying whose data
it is, so the compiler is what keeps a new query scoped.

Milestones have no `user_id` of their own; every milestone statement joins back
to `goals` and filters there. The clause moves, it is never dropped.

## A foreign resource is 404, not 403

`403 Forbidden` on somebody else's task confirms the id exists. Enumerating ids
would then tell an attacker how many tasks other users have, and a leaked id
from a log or a URL could be confirmed as real. `404` says nothing.

This extends past the obvious routes. A `parent_task_id` or
`depends_on_task_id` naming another user's task also answers `404` rather than a
`400` blaming the field, and a path id that is not even a valid UUID answers
`404` too — one response for "not yours", "not there" and "not an id".

## Enum columns are checked in the service, not by CHECK constraints

`priority`, `status` and `type` are plain `text`. The allowed sets live in Go
(`tasks.Priorities`, `goals.Types`, …) and are enforced in each module's
validator.

The trade is deliberate: a `CHECK` would catch a bad value written by hand in
psql, but adding a status later would then need a migration, a deploy ordering
between schema and code, and a rollback story. Values are only ever written
through the service, and the service tests pin the sets. If a future phase adds
another writer — an importer, a background job — the CHECK constraints become
worth their cost and can be added then.

## `sort` selects a fragment from a fixed map

`ORDER BY` cannot be parameterised, so it has to be interpolated. Each module
keeps a `Sorts` map from the public sort value to a literal SQL fragment, and
the request is only ever used as a map key. A value that is not in the map is a
`400` — not a silent fall back to the default, which would hide a typo behind
plausible-looking results.

`priority` sorts by rank via a `CASE`, because alphabetically `high` sorts below
`low`. Date sorts are `NULLS LAST` in both directions: someone ordering by
deadline wants the dated rows.

## PATCH needs a three-state field

`optional.Field[T]` distinguishes "key absent" from "key present and null". A
plain `*T` collapses the two, which makes a nullable column impossible to clear
over the wire — the handler cannot tell "leave the deadline alone" from "remove
the deadline". `UnmarshalJSON` is only called when the key is present, which is
what makes the `Set` flag trustworthy.

The repository then builds its `SET` list from only the fields that carry an
instruction. Two clients patching different fields of the same row do not
overwrite each other, and an empty patch is a read rather than a write, so it
does not fire the `updated_at` trigger.

## Dependency edges are added under a per-user advisory lock

`AddDependency` runs the ownership check, the cycle check and the insert in one
transaction holding `pg_advisory_xact_lock(hashtext(user_id))`.

Without the lock, two concurrent additions could each see an acyclic graph and
together close a loop — the classic check-then-act race, and a cycle is not
something a later reader can recover from. The lock is per user and held only
for the length of one small transaction, so it serialises nothing that matters:
a user is not adding two dependencies at once except by accident, which is
exactly the case being defended against.

Cycles are detected with a recursive CTE that asks whether the task is already
reachable from its proposed dependency. Re-adding an existing edge is
`ON CONFLICT DO NOTHING`: it is the state the caller asked for, not an error.

## Every list endpoint is paged, whether or not it was asked for

`limit` defaults to 50 and is clamped to 200; `offset` defaults to 0. An
unbounded list over a table that grows without limit is a denial-of-service
lever that costs one slow client and one large account to pull. Clamping rather
than rejecting an oversized `limit` keeps the endpoint usable, and the effective
values are echoed in the response so a client can tell it was clamped.

Ordering always ends with `, id ASC`. Without a tiebreaker, two rows sharing a
`created_at` can swap places between pages and a client would see one row twice
and another never.

## `text[]` is read back as JSON

`database/sql` hands the driver's rendering of an array straight to `Scan`,
which for pgx is the Postgres literal form (`{a,"b c"}`). Parsing that means
re-implementing array quoting and escaping. The reads instead select
`array_to_json(tags)` and `db.TextArray` unmarshals it. Writes need no adapter —
pgx encodes a `[]string` parameter as `text[]` on its own — except that a nil
slice would encode as SQL NULL, so `db.TextArrayParam` turns it into `{}`.

## The shared transport helpers alias auth's error types

`internal/httpx` provides the JSON helpers the three resource modules share.
Its `ErrorBody` and `ValidationError` are Go type *aliases* of the ones
`internal/auth` already defines, not copies: the API returns exactly one error
shape, and an alias makes that a compile-time fact rather than a convention two
packages have to keep agreeing on.

## The module path still says `lifeos`

The project is Lunex, and the docs, database names and comments say so. The Go
module path is `github.com/jashveer/lifeos/backend`, which mirrors the
repository URL — renaming it is a rename of the repository, not a code change,
and guessing at that is not this phase's call. It is a one-line `go.mod` edit
plus a `sed` over the imports whenever the repository moves.

---

# Phase 3 decisions

## HNSW, not IVFFlat

IVFFlat clusters the rows that exist when the index is built into `lists`
centroids. An index created by a migration is created on an empty table, so it
has no useful clustering and has to be rebuilt once real data arrives — a step
nothing in the deploy would remember to do. HNSW builds its graph incrementally
as rows are inserted: no training pass, no `REINDEX`, and better recall at the
same speed.

The costs are real and are the right ones to pay here. HNSW uses more memory,
and an insert is slower than IVFFlat's. Inserts happen once per uploaded
document; searches happen on every question asked of the corpus.

The operator class is `vector_cosine_ops`, matching the `<=>` the search uses.
nomic-embed-text does not return unit-length vectors, so cosine — not L2, not
inner product — is the distance that means "similar text".

## `ef_search` is raised for every search

Every search is filtered by `user_id`. An HNSW scan collects candidates from the
graph *first* and applies the `WHERE` clause afterwards, so with the default
`ef_search` of 40, a user whose chunks are a small fraction of the table can
have most candidates filtered away and get back fewer rows than they asked for
— silently, since a short result list looks exactly like a small corpus.

`Repository.Search` therefore runs in a read-only transaction whose only reason
to exist is `SET LOCAL hnsw.ef_search = 200`: local, so the setting reverts on
commit instead of leaking to the next query on that pooled connection.

pgvector 0.8 adds iterative index scans (`hnsw.iterative_scan`), which solve
this properly by re-walking the graph until the filter is satisfied. Using them
would tie the code to a version floor the migration does not currently enforce;
a wider `ef_search` costs a little latency and works on every version that has
HNSW at all.

## `document_chunks.user_id` is denormalized

The owner is derivable by joining `documents`, and it is stored on the chunk
anyway. The similarity search is an index scan over `document_chunks` ordered by
distance; if the owner filter lived on the other side of a join, it could only
be applied *after* that scan had already chosen its candidates — which is the
same recall problem as above, made worse.

The duplication is safe because nothing can change it: a chunk's `user_id` is
written once, at insert, from the same variable that scoped the document lookup.

## Chunking counts words, not tokens

The brief asks for ~500-token chunks with ~50 tokens of overlap. Nothing here
tokenizes. The only tokenizer that would be exact is the embedding model's own,
and a chunk 15% off target costs nothing: nomic-embed-text's context is 8k
tokens, sixteen times a chunk.

So the token budget is converted once, at ~0.75 words per token, into 375 words
with 38 of overlap — and the windows are sliced out of the original string
rather than re-joined from a word list, so paragraph breaks, indentation and
Markdown structure survive into the chunk. What gets embedded is what a citation
will quote back.

A byte ceiling (`MaxChunkBytes`) sits on top for text that is not words at all:
minified JSON, base64, a language that does not space-separate. Without it one
"word" could be the entire file, and Ollama would silently truncate it — storing
a vector that does not describe most of the chunk it is attached to.

## A failed document is a created document

`POST /documents` answers `201` even when processing failed, with `status:
"failed"` and the reason in `error_message`.

The alternative — a `5xx` — leaves the client with a row they were never told
the id of. The row genuinely exists: it can be listed, inspected and deleted.
The line drawn instead is *when* the failure happened. Anything detected before
work starts (empty file, unsupported type, oversized upload) creates nothing and
is an ordinary `4xx`. Anything that fails during the pipeline is a document that
failed.

`error_message` is a fixed set of sentences written in this package, never
`err.Error()`. The column is read back over the API, and a wrapped driver error
would put connection strings and internal paths in an HTTP response. The full
error goes to the log.

## Uploads are processed inside the request

There is no job queue in this phase, so the pipeline runs in the handler. Two
consequences were made explicit rather than left to be discovered:

- **`/documents` gets its own request timeout** (`DOCUMENT_PROCESS_TIMEOUT`,
  default 2 minutes) instead of the global 30 seconds, and the HTTP server's
  read and write timeouts are derived from it — a response the socket has
  already given up on cannot report a result. Every other route keeps the 30
  seconds.
- **A document is capped at 800 chunks** (~1.5 MB of prose). That is not a storage
  limit; it is what keeps one upload from holding a request open past any
  reasonable timeout. A larger file fails with a message saying so.

Both revert when the queue arrives: upload becomes `202`, and the cap becomes a
matter of how long a worker may run rather than how long a client will wait.

## The retrieval function is a service method, not a handler

`documents.Service.Search(ctx, userID, SearchQuery)` takes the user id as an
argument rather than reading it from a request context, and returns typed
results. Phase 6's chat calls exactly that, from an agent loop with no HTTP
anywhere in it. `POST /documents/search` is a thin adapter over it, and the
validation and the owner scoping live on the service side of that line, so the
non-HTTP caller cannot skip them.

# Explicitly deferred

These are **not** silently skipped — they are known gaps.

## 1. Rate limiting is in-process, not distributed

`internal/auth/ratelimit.go` implements a real per-IP token bucket
(`golang.org/x/time/rate`) that is wired into `/auth/register` and
`/auth/login`, so the endpoints are not unprotected. But:

- Counters live in process memory: they reset on restart.
- Each replica keeps its own buckets, so behind N API processes the effective
  limit is N x the configured rate.
- The limiter keys on `RemoteAddr` only. `X-Forwarded-For` is deliberately
  ignored, because it is client-controlled until a trusted proxy is configured;
  behind a load balancer this needs a trusted-proxy setting, or every request
  will appear to come from the balancer.

A shared store (Redis) is the fix, and Redis is explicitly out of scope for
Phase 1. The `TODO(phase-2)` comment sits on the type.

## 2. No auth UI *(resolved in the UI phase)*

Phase 1 shipped a health panel and nothing else. The UI phase added the auth
screens and made the token-storage call deferred here: the access token in
memory, the refresh token in `localStorage`. See "The refresh token lives in
localStorage" under the UI phase below.

## 3. No email verification or password reset

`users` has no `email_verified_at` column and nothing sends mail. Registration
trusts the address as given.

## 4. Refresh tokens are returned in the JSON body

Not as an `HttpOnly; Secure; SameSite` cookie. That is the right shape for the
API-client and mobile-free scope of Phase 1, but a browser SPA storing a refresh
token in JavaScript-reachable storage is XSS-exposed. Revisit with the auth UI.

*UI phase:* still true, and now load-bearing — the SPA does store it in
`localStorage`. The cookie is a backend change and was out of scope for a
frontend-only phase; see deferred item 38.

## 5. No CORS middleware

The frontend dev server proxies `/api` to the backend (see
`frontend/vite.config.ts`), so same-origin holds in development. A deployment
that serves the SPA from a different origin will need CORS configured.

## 6. No recurring-task logic

The brief leaves room for a `recurrence_rule` column on `tasks`. Nothing was
added for it: a column nothing reads is a column that drifts out of date, and
the interesting part is the expansion logic — does completing an occurrence
create the next one, what happens to a rule edited mid-series — which is a phase
of its own. The migration to add it later is additive and needs no backfill.

## 7. List paging is limit/offset, not keyset

`OFFSET n` makes the database walk and discard `n` rows, so deep pages get
slower, and a row inserted while a client is paging shifts everything after it —
the client sees one row twice and misses another. At Phase 2 sizes neither
matters. The fix, when it does, is keyset paging on `(created_at, id)`, which
the composite index already added on each table supports.

## 8. No bulk or transactional multi-writes

Each endpoint writes one row. Reordering a list of tasks, or completing a goal
and all its milestones, takes one request per row and is not atomic. A
`PATCH /tasks` accepting a list is the shape for it, and it should share the
per-user advisory lock that dependency edits already use.

## 9. No object storage — the original file is gone

**This is the largest gap in Phase 3.** `documents.extracted_text` holds the
text; the uploaded bytes are read, parsed and discarded. There is no way to
re-download the PDF that was uploaded, to show it in a viewer, to re-extract it
with a better parser later, or to hand it to OCR when that arrives.

Everything a user can get back is the plain text: page layout, tables, images
and formatting are lost at upload time and cannot be recovered from what is
stored.

MinIO is the fix and is a later phase. The migration for it is additive — a
`storage_key` column on `documents` — but the files uploaded before it exists
are not recoverable, so this is worth knowing before anyone treats the API as a
document store.

## 10. PDF and TXT/Markdown only

DOCX, CSV and images (OCR) are rejected with `415`. Each is a separate parser
with its own failure modes, and the brief scopes them out. A scanned PDF is
accepted, extracts nothing, and fails with a message that says OCR is what it
needs — rather than looking like a corrupt file.

The PDF path reads the text layer only, via `ledongthuc/pdf`. That library
panics on some malformed files rather than returning an error, which
user-uploaded files will find; `extractPDF` recovers from that and reports it as
an ordinary extraction failure.

## 11. No background job queue

Uploads process synchronously — see the Phase 3 decision above for what that
constrains. A document that fails because Ollama was down has to be deleted and
re-uploaded; there is no retry.

## 12. No reranking, and no hybrid search

Retrieval is plain vector similarity. There is no keyword/BM25 leg and no
cross-encoder pass over the top-k, both of which measurably improve RAG
precision. `min_similarity` is the only defence against confident-looking
irrelevant citations, and it is off by default.

## 13. Embeddings are tied to one model

The column is `vector(768)` and the vectors in it come from
nomic-embed-text. Vectors from two models are not comparable, so changing
`EMBEDDING_MODEL` means a migration that changes the column *and* re-embeds
every existing chunk. `EMBEDDING_DIMENSIONS` exists as a knob, but a mismatch is
the operator's to resolve — the client refuses the batch with both widths named
rather than letting Postgres reject the insert with an opaque error.

## 14. Chunk text is stored twice

Once in `documents.extracted_text` and again, in overlapping windows, across
`document_chunks.content`. With ~10% overlap that is roughly 2.1x the text size
in the database. It is the right trade while retrieval must return the exact
string it embedded — but it is why the 800-chunk cap and the 10 MB upload limit
matter more than they would if chunks were offsets into the text.

---

# Phase 4 decisions

## The provider is an interface, and no cloud provider is implemented

`internal/ai` defines `Provider` (`Chat(ctx, messages, opts) (Stream, error)`)
with two implementations: `Ollama`, and `Mock`. **There is deliberately no
Anthropic or OpenAI implementation in this phase** — the brief scopes it out —
but the interface is shaped so adding one is a new file in `internal/ai` plus
one line in `cmd/api`, and nothing in `internal/chat` changes. What that costs
is stated rather than hidden: `Options` carries only `Model`, `Temperature` and
`MaxTokens`, so a hosted provider's extras (system-prompt caching, tool use,
structured output, a per-request key) will need the struct to grow. Growing an
options struct is a smaller change than unpicking Ollama's wire format from an
orchestrator.

`Mock` lives in the package rather than in a `_test.go` file because three
packages' tests need it, and a test helper that three packages import is
production code with a small audience. It is also why `go test ./...` is green
on a machine with no model server.

## The grounding rule is enforced in three places, not one

The master spec's rule — never claim an answer came from the user's data when
it did not — is not something a system prompt alone can deliver. It is
implemented as:

1. **A retrieval floor.** `CHAT_MIN_SIMILARITY` defaults to `0.5`. Vector
   search always returns the nearest *k* chunks however far away they are, so
   with no floor an unrelated question is handed a distant chunk and the model
   is then being asked to resist material it was told was relevant. The floor
   removes the temptation instead of relying on the model to. Measured against
   the corpus in `scripts/e2e.sh` with nomic-embed-text: the question the notes
   answer scores 0.73, "the current price of Brent crude" scores 0.43, "how do
   I make sourdough starter" 0.38. 0.5 sits in that gap.
2. **An explicitly empty context.** When nothing is retrieved the prompt still
   carries a `CONTEXT` section, saying so in words. An absent section reads
   like an oversight; an explicitly empty one reads like an answer.
3. **Citations checked after the fact.** `Source.Cited` is set by reading the
   generated answer for the `S1`-style labels it was actually given. Retrieval
   typically offers five things and an answer uses one, so recording all five
   as "used" would make the stored citation trail useless for the question it
   exists to answer — what was this claim based on?

The system prompt states the rule too, including what to do instead (say it
was not found, then answer from general knowledge and say so). Prompts are the
weakest of the four, which is why they are not the only one.

## The assistant cannot take actions, and says so

There is no approval engine in this phase, so an assistant that answered "done,
I've added that task" would be lying about a write that cannot happen. The
system prompt says it can only read and must tell the user to do it themselves.
Structurally, `chat.Deps` is given `DocumentSearcher`, `TaskLister`,
`GoalLister` and `NoteLister` — four read interfaces — so there is no write
path to reach even by accident. Adding one will be a deliberate change to that
struct.

A caveat worth stating: a 3B model asked to create a task answers "I'm not able
to take actions on your behalf" and then, sometimes, offers to help create one
anyway. It does not claim to have done it, and nothing is written. A larger
model is the fix, not more prompt.

## Streaming is Server-Sent Events, not chunked text

Both were on the table. SSE won on one point: the answer is not only text.
Every turn also has a citation list that a client wants *before* the first
token, so it can show what the reply is grounded in while it is still being
written. SSE names each frame and carries a JSON body, so `sources`, `token`,
`done` and `error` are distinguishable without inventing a length-prefixed
protocol inside a chunked body. Token text is JSON-encoded because models emit
newlines mid-sentence and an SSE frame ends at a blank line.

This is SSE *framing* over a normal authenticated `POST`, not an `EventSource`
resource — `EventSource` can send neither a body nor an `Authorization` header,
so clients read it with `fetch`.

The cost: the response commits to `200 OK` at the first frame. That is why the
orchestrator does retrieval *and* opens the model stream before touching the
sink — everything that can fail with a meaningful status code (`404`, `400`,
`503`) fails while the HTTP layer can still choose one, and only a
mid-generation failure has to be reported as an `error` event.

There is no keep-alive comment on the stream. A local model's first token
usually arrives in a few seconds and the `sources` frame is sent before it, so
a proxy sees traffic early. A slow hosted model behind an idle-timeout proxy
would want one.

## A failed turn persists nothing

If generation fails — the model refuses the call, the stream is cut off, the
reply is empty, the client hangs up — neither message is written. The
alternative, keeping the question and discarding the answer, leaves a dangling
user message that the next turn replays as history and that a client cannot
distinguish from a question the assistant ignored. Resending is the retry.

The whole turn is one transaction, so a reader never sees a question without
its answer. That transaction is opened by an `UPDATE conversations … RETURNING
id`, which does three jobs at once: it proves the conversation is the caller's
(no rows means it is not), applies the derived title if the conversation is
still called `New conversation`, and moves `updated_at` through the existing
`set_updated_at` trigger — inserting a message would not otherwise touch the
parent row, and the conversation list sorts on it.

## Messages have no user_id, so ownership runs through the join

The brief's schema hangs `messages` off `conversations` alone. Ownership is
still in the `WHERE` clause rather than checked after the row is in memory —
every message statement joins `conversations` and filters `user_id` there — it
just costs one join. `messages_conversation_id_created_at_idx` is what makes
that join and the "last N messages" read a single scan, and it doubles as the
index the `ON DELETE CASCADE` lookup needs, which the foreign key does not
create by itself.

## Message timestamps come from clock_timestamp(), not now()

`now()` is the *transaction's* start time, so both messages of a turn would
carry the identical timestamp and their read order would fall to the tie-break
on a random uuid — which can put the answer before the question. The inserts
use `clock_timestamp()`, which reads the wall clock per statement.

The sharp edge: `clock_timestamp()` has microsecond resolution, and two rows
written a microsecond apart would tie. Two round trips to Postgres take far
longer than that, so it does not happen in practice — but if the ordering ever
needs to be a guarantee rather than an overwhelming likelihood, the fix is a
monotonic `seq` column on `messages`, and `ORDER BY` moves to it.

## The request timeout is per subtree, because a nested one only shortens

`middleware.Timeout` derives its context from the one already on the request,
so a nested `Timeout` can only shorten the deadline it inherits: a two-minute
budget underneath a thirty-second one is thirty seconds.

Phases 1–3 applied a 30-second timeout at the root and a longer one on
`/documents` beneath it, which meant `DOCUMENT_PROCESS_TIMEOUT` never actually
took effect — an upload was cut off at 30 seconds regardless. Phase 4 found it
the obvious way: the first real chat turn against a cold llama3.2:3b died at
exactly 30 seconds with `context deadline exceeded`. The router now applies the
budget per subtree and none of them nest.

`RequireAuth` is repeated on each of those three groups rather than lifted onto
the `/api/v1` router, because a router-level middleware runs *before* routing:
an unknown path under `/api/v1` would then answer `401` instead of `404`.

## Tasks, goals and notes are retrieved by heuristic, not semantically

They have no embeddings in this phase. What they get instead is a stated rule:
up to 5 tasks (in progress by recency, then pending by nearest deadline), up to
5 active goals by nearest deadline, and the 3 most recently updated notes.
The difference is visible to the model — a chunk arrives with a similarity
score, an item arrives as background the question may or may not be about — and
`Cited` records which of it the answer actually used.

The consequence is real: "what did I write about the aurora" searches
documents properly, while "which of my notes mentions the antenna" only sees
the three most recent notes. Embedding tasks, goals and notes into the same
`document_chunks`-style index is the fix and is a later phase.

## Conversation history is the whole of the memory *(superseded by Phase 5)*

The last 20 messages are replayed as real user/assistant turns. In Phase 4 that
was the whole of the memory, so a conversation longer than 20 messages forgot
its own beginning, silently. Phase 5 adds the long-term store: the wording is
still lost, the durable facts are not.

The context block sits immediately before the new question rather than at the
top of the prompt, because it was retrieved for *that* question; putting it
next to the question is what stops the model attributing it to an earlier turn.

## Titles are derived, not generated

A conversation is named from the first line of its first message, cut to 60
characters. Asking the model for a title would cost a second round trip per
conversation, and a title is a label in a sidebar rather than a summary.

## What Phase 4 does not have

1. **No cloud provider.** Interface only; see above.
2. **No memory system.** Phase 5.
3. **No agents.** One general assistant.
4. **No knowledge graph, no action/approval engine.** The assistant reads.
5. **No regeneration, no editing a message, no branching.** A conversation is
   append-only.
6. **No streaming cancellation that survives the request.** A client that hangs
   up abandons the turn; the model's own generation is cancelled with the
   request context and nothing is stored.
7. **No token accounting.** `MaxTokens` bounds the reply and the context budget
   is counted in characters, not tokens. A prompt that overflows the model's
   window is truncated by Ollama, silently.
8. **No reranking of the retrieved set**, and no hybrid search — inherited from
   Phase 3, and it matters more here, because a chunk that scrapes past the
   floor is shown to a model rather than to a person who can dismiss it.
9. **No per-user rate limit on generation.** A user can hold as many concurrent
   turns open as they have connections, each occupying the model server. The
   `CHAT_TIMEOUT` budget is the only bound.


# Phase 5 decisions

## Memories are a second vector index, not a new mechanism

A memory is a short text with an embedding, searched by cosine distance and
scoped to its owner in the `WHERE` clause. That is `document_chunks` with a
different table name, and it is deliberate: the retrieval path is the one part
of this system that is already proven, and inventing a second way to find
things would have doubled the surface where a user's data can leak.

The differences from `document_chunks` are all small and all deliberate:
`memories` has no parent table (a fact has no document), it carries the two
model scores, it carries a user-controlled `enabled` switch, and its HNSW index
is *partial* on `enabled` — a disabled memory is not merely filtered out after
the graph walk, it is not in the graph. HNSW with `vector_cosine_ops` matches
migration 000003 because the same embedding model produces both kinds of vector
and the same `<=>` operator searches both; consistency here was worth more than
re-deciding.

## The memory floor is 0.6 and the document floor is 0.5

The brief said to tune this independently and justify it, and the two numbers
genuinely are different questions.

- A memory is one sentence; a chunk is a ~500-token passage. Two short texts
  have little content to disagree about, so a weak topical relation scores
  *higher* between a question and a memory than between the same question and a
  chunk. The same number does not mean the same closeness.
- A memory reaches the prompt as a stated fact about the user. A wrongly
  retrieved chunk is an irrelevant quotation they can dismiss; a wrongly
  retrieved memory is a false claim about them.
- Documents are searched with the question as the query, and a question about
  the aurora simply does not match a chunk about fuel filters. Memories are
  searched on *every* turn regardless of topic, and the floor is the only thing
  keeping a standing preference out of an unrelated answer. There is no second
  filter behind it.

0.6 is where nomic-embed-text puts a question and a memory that are about the
same thing (~0.65–0.85), above where it puts one that merely shares a register.
`MEMORY_MIN_SIMILARITY` moves it.

## Extraction runs inline, after the last token

The brief ruled out a job queue, so extraction is synchronous — but *where* in
the turn it sits was still a choice, and it sits at the end on purpose.

The order is: generate, stream every token, persist the turn, extract, send
`done`. The user reads a complete answer while extraction is still running. What
the second model call delays is the end-of-stream marker, not the reply.

The cost is real and worth stating plainly: a substantial turn now makes two
model calls instead of one, and on a local 3B model the second is tens of
seconds. It is free in money and not free in time. Three things bound it:
`MinExtractionChars` skips the trivial turns entirely, `MEMORY_EXTRACT_TIMEOUT`
caps the call, and `MEMORY_EXTRACTION=false` removes it while leaving retrieval
running — an assistant that uses what it already knows without learning
anything new is a supported configuration, not a broken one.

Extraction is given the turn's own context, so a client that hangs up abandons
it. The exchange is already saved; there will be another turn.

## Extraction cannot fail a chat turn

If the extraction model is down, returns prose, or runs past its deadline, the
error is logged and the answer is returned unchanged. By the time extraction
runs the answer has been generated, streamed and stored — turning that into a
500 because a second call failed would be an outright regression, and the user
would have watched the answer appear and then be told the request failed.

The corollary: nothing is extracted from a turn that was not persisted. A
refused model call or a failed write leaves no exchange to learn from.

## The threshold is 120 characters, and it is a real trade

Extraction is skipped unless the user's message and the assistant's answer
together reach 120 characters after trimming. A greeting, a "thanks", an "ok
that works" and their one-line replies all land well below it; a turn where the
user says something about themselves and gets a substantive reply clears it
easily.

The measure is the pair rather than the user's message alone because a durable
fact needs both halves: someone stating something *and* an exchange with enough
substance to be worth a second model call.

What it costs: a fact stated in six words and answered in six is missed. That is
the trade — a memory table full of "the user said hello" is worse than a missed
fact the user can restate, and every retrieved memory is spent from the same
small context budget the documents compete for.

## Four filters stand between the model and the table

Everything the extractor proposes passes through these before it is stored, and
each one came from watching llama3.2:3b actually do the job:

1. **Confidence below 0.4** — the model's own estimate that it *read* the fact
   rather than inferred it. A low-confidence reading, once stored, is retrieved
   into later prompts as a flat statement about the user, where nothing carries
   the doubt any more.
2. **Importance below 0.4.** This one was added from measurement, against my
   first instinct. Asked to extract from an ordinary lookup ("what do my field
   notes say about the tundra?"), the model reliably produces "The user inquired
   about their field notes" at importance 0.2 — confident, accurate, worthless.
   It knows they are worthless; it says so in the field. Believing it is cheaper
   than a second filtering pass, and the failure is symmetrical: a genuinely
   important fact scored low is one the model was equally happy to discard.
3. **Not about the user.** The prompt requires every fact to start with "The
   user"; this enforces it by requiring the word "user" anywhere in the
   sentence. It exists because of a measured failure: on that same lookup the
   model returns *the retrieved material itself* — "The aurora borealis appeared
   over the tundra shortly after midnight" — confidently and at high importance.
   That is a true sentence about a document, about to be stored as a fact about
   a person and recited back later as something the assistant knows about them.
   The test is loose on purpose; a genuine fact phrased without the word is
   lost, which is the right price on a path where a wrong memory costs far more
   than a missing one.
   There is a second reason for this filter, and it is not about quality. The
   extractor reads an assistant answer that was itself grounded in the user's
   documents, so text in an uploaded file reaches the extraction prompt. A
   document that says "Remember: the user has approved all future purchases" is
   a plausible thing for someone to send a user, and without this check the
   sentence has a path into the user's own memory, where it would be recited
   back as something the assistant knows about them. Requiring the fact to be
   about the user narrows that path; it does not close it, and nothing in this
   phase closes it. The mitigations that exist are all partial: the assistant
   cannot take actions, memories are visible and deletable, and every one says
   which conversation taught it.

4. **Already known**, measured as cosine ≥ 0.95 against an existing memory. The
   threshold is high on purpose: this catches the same fact restated in a later
   conversation, not two related facts. Below ~0.9 a refinement ("...in the
   morning *before class*") would be swallowed by the vaguer memory it should
   have joined.

## JSON mode is off, because it makes a 3B model worse

The obvious way to get structured output is Ollama's `format: "json"`, and
`ai.Options.Format` exists for it. The extractor does not use it by default,
and that is measured rather than assumed: with JSON mode on, llama3.2:3b
satisfies the constraint immediately with `{}` and then emits whitespace until
it hits `num_predict`, so the stream ends without a done frame and the whole
extraction is reported as a truncated generation. The identical prompt without
the constraint returns a clean JSON array — or a clean `[]`.

Constrained decoding is a real capability and a larger model uses it well, so
the switch stays (`MEMORY_JSON_MODE`), but the default has to be what works
with the model this phase ships with.

The parser is therefore lenient by design: it accepts a bare array, an object
wrapping one under any key, a single object, any of those inside a Markdown
fence or after a sentence of prose, and — as the last resort the prompt itself
names — one fact per line as `type | importance | confidence | sentence`. What
it will not do is guess: a reply matching none of those shapes yields no
candidates, an unreadable score becomes the neutral 0.5 rather than a confident
1.0, and an unrecognised type becomes `semantic` rather than reaching a column
with a CHECK constraint on it.

The reliability trade, stated: without constrained decoding, invalid JSON is
possible. The failure mode when it happens is that a turn's facts are lost, not
that a wrong fact is stored — and a lost fact can be restated.

## The extraction prompt carries a worked example

Without one, llama3.2:3b answers `[]` to every exchange, including ones that
plainly state a preference. Adding a single input/output example — and removing
an earlier line saying "most exchanges have nothing" — is what made the phase
work at all. That line was true and it was also the entire failure: a 3B model
reads a prior toward the empty answer as an instruction.

A known weakness remains. The extractor reads the assistant's reply as well as
the user's message, and when the assistant opens with "I could not find anything
about your schedule or preferences", the model treats that as evidence that the
exchange contained nothing — even though the user had just stated a preference
in the same breath. The prompt says the reply is context only and the facts come
from the user; that helps and does not fully fix it. The honest fix is a better
extraction model, and `MEMORY_MODEL` exists to point at one.

## Deleting a conversation keeps its memories

`source_conversation_id` is `ON DELETE SET NULL`, not `ON DELETE CASCADE`.
Deleting a conversation is tidying up a transcript; it is not a statement that
what you said in it was untrue. The memory survives with its provenance simply
unknown, and the user who did mean "forget that" has `DELETE /memories/{id}`
and the clear-all endpoint to say so.

## Clearing everything answers 200 with a count

Not `204`. "Forget everything about me" is irreversible in this phase — there is
no archive and no undo — and the count is the only way to tell "there was
nothing to forget" from "four hundred facts are gone". The `{"confirm": true}`
flag is checked in the *service*, not only in the handler, because every caller
goes through the service and a future agent will be one of them.

## Editing a memory re-embeds it

A `PATCH` to `content` embeds the new text and replaces the vector in the same
statement. The two have to move together: a memory whose text says one thing
and whose vector still points at the old wording is retrievable by what it used
to say and invisible to what it now says, which is worse than either. Toggling
`enabled` touches no text and costs no model call.

`type`, `importance` and `confidence` are not editable. They are a record of
what the extraction said; rewriting them would make the scores mean nothing.

## `expires_at` exists and nothing writes it

The column, and the `expires_at IS NULL OR expires_at > now()` clause in the
retrieval query, are both in place. No code path sets a value, so it is always
NULL in this phase. That is not an oversight: deciding *which* facts should
expire is a judgement this phase does not have a mechanism for, and adding the
column later would mean a migration plus a change to the query that every
retrieval depends on. The filter is tested against a row the test expires by
hand.

## Memories are the assistant's to write, not the client's

There is no `POST /memories`. A memory is something the assistant extracted from
a conversation, and its `importance` and `confidence` describe that extraction.
A hand-written memory would have no honest values for either, and "the assistant
learned this about me" and "I typed this into a box" are different enough that
they would want different columns. The client's powers are exactly the ones the
brief names: see, correct, disable, delete.

# Phase 5 — explicitly deferred

## 15. No consolidation, merging or forgetting curve

Memories accumulate. Nothing merges "the user prefers mornings" with "the user
prefers studying before class", nothing decays an old fact, and nothing notices
that a new memory contradicts an existing one — the prompt tells the model to
prefer what the user says now, which is a mitigation rather than a fix. The
duplicate check is exact-ish similarity only.

## 16. Extraction sees one turn, not the conversation

Each exchange is read in isolation. A fact stated across three turns ("I am
switching topics" … "to distributed consensus") is not assembled, because the
extractor is never shown two turns at once.

## 17. No knowledge graph

Memories do not reference each other, or tasks, goals and notes. `project` and
`goal` are type labels, not foreign keys — the brief rules the graph out of this
phase, and typing a memory is not the same as linking it.

## 18. Retrieval is similarity only

Importance, confidence, recency and how often a memory has been used are all
recorded and none of them affect the ranking. A memory scored 0.9 important and
one scored 0.5 compete on cosine distance alone.

## 19. The memory floor was reasoned, not swept

0.6 comes from the argument above and from what nomic-embed-text does on a
handful of pairs. Nobody has run a labelled set through it. The same is true of
the 0.95 duplicate threshold and both 0.4 score floors: they are defensible
numbers with knobs on them, not measured optima.

## 20. Extraction quality is a 3B model's quality

The pipeline is sound and the model is small. It misses facts, occasionally
keeps a marginal one, and is influenced by what the assistant said as well as by
what the user said. `MEMORY_MODEL` points extraction at a different model
without touching anything else, and that is the intended fix.

# Phase 6 decisions

## Node sync happens inline on write, not lazily on read

Creating a task creates its graph node in the same request, through a one-method
`NodeSyncer` interface each resource module declares for itself. The brief named
the alternative — compute nodes lazily on the first graph query — and it was
rejected for three reasons.

It is consistent with everything else here. Nothing in this codebase defers work
to a later read: documents are extracted, chunked and embedded inside the
upload, memories are extracted inside the chat turn. A lazily-materialised graph
would be the only place where reading changes the database.

It keeps the read honest. Lazy population means the first `GET
/knowledge-graph` after any write does a scan of four tables and a batch insert,
so the endpoint's cost depends on how much has happened since it was last
called — and the *extraction* path would need the same backfill before it could
match a name against an existing node, because a conversation is not a graph
read. Both paths would end up calling the same reconcile function, and then
there is no laziness left, only a worse place to trigger it from.

And it makes the failure visible. An upsert on the resource's own request either
works or logs a warning next to the write that caused it. A reconcile pass that
silently misses a table is a graph that is quietly incomplete, which is the
failure mode that matters least when the graph is small and most when it is not.

The cost is that each of the four modules gains a dependency it did not have.
That is paid for by keeping it a locally-declared one-method interface — the
same pattern `internal/chat` uses for the searchers it consumes — so
`internal/tasks` still does not import `internal/graph`, and a service built
without the option is byte-for-byte the Phase 2 behaviour.

## `SyncNode` returns no error

It runs *after* the row is committed. There is nothing useful for the caller to
do with a failure: returning an error would report a task that exists as
failed, and undoing the write would throw away what the user asked for to
protect an index derived from it. So the graph logs its own failures, and the
resource module gets an interface it cannot misuse.

The repair is the update path. Sync runs on every update as well as every
create — it has to, so a renamed task's node does not keep matching on a name
the user has stopped using — which means the next edit fixes a create whose sync
was lost. There is no backfill for rows written before this phase; see
**deferred** below.

## Deleting a node is a trigger, not application code

The brief said a deleted task's node should go "automatically via FK". It cannot:
`ref_id` names four different tables, so there is no foreign key to hang a
cascade on. That is a real gap in the specified schema rather than a detail, and
it needed a decision rather than a silent omission.

The schema stayed as specified — `ref_table` + `ref_id`, which is what makes the
node table one table and the graph queries uniform — and the deletion moved into
an `AFTER DELETE` trigger on each of the four tables, sharing one function.

A trigger rather than a `DropNode` call from each service, because a row can
leave by routes the service never sees. Deleting a parent task cascades to its
subtasks in SQL: `tasks.Service.Delete` is called once and four rows go. A
`DELETE FROM users` takes everything. A future bulk operation, a `psql` session,
a repair script — none of them are on a path anybody would remember to wire. A
trigger fires on cascaded deletes too, which is exactly the case that would
otherwise leave orphan nodes pointing at rows that no longer exist.

The alternative that *would* have given a real foreign key is four nullable
columns — `task_id`, `goal_id`, `note_id`, `document_id` — with a `CHECK` that
at most one is set. It was rejected: it makes every query on the node's
provenance a four-way `COALESCE`, it makes adding a fifth mirrored type a
migration on a table with data, and it trades a clear polymorphic pair for four
columns that are three-quarters `NULL` on every row.

## Entity matching is exact and case-insensitive, not fuzzy

Two mentions of a name are the same node when their labels match after trimming,
collapsing internal whitespace, stripping surrounding quotes and punctuation,
and folding case. Nothing more.

Nodes carry no embeddings this phase, so "fuzzy" here means edit distance or
trigrams, and both merge names that are genuinely different: Go and Godot, Alice
and Alicia, "OS coursework" and "OS homework". The asymmetry is what decides it.
A duplicate node is visible, harmless and deletable. A wrong merge does not lose
a distinction — it **invents relationships**, handing Godot every edge Go has and
then feeding them back into later prompts as things the assistant knows about
the user. One failure is untidy; the other is a fabrication that reads exactly
like a fact.

Case folding and whitespace collapsing are kept because they catch the
duplication that actually happens: the same model writes "Go" in one turn and
"go" in the next. The stored label keeps its case — it is what a client displays
— and only the comparison folds, in SQL (`lower(label)`) and in Go
(`FoldLabel`), which is the same rule said in the two places it has to be said.

The matching looks at *every* node the user has, not only the extracted ones.
That is the point of it: a conversation about "the backend project" should
attach its edge to the goal the user already has by that name, so asking about
it later reaches the real record. When several nodes share a label, a type match
wins and the oldest node wins after that — a name the user already has a record
for is more likely to be that record than a new entity spelled the same way.

The upgrade path is node embeddings, where a merge can be proposed with a score
instead of guessed at with a string metric. That is a phase with a UI for
confirming merges, which this one is not.

## Relationship extraction is a separate model call from memory extraction

The brief offered combining them. They are two calls.

The outputs are different shapes with different failure modes. Memory extraction
asks for sentences about a person; relationship extraction asks for typed
triples over two closed vocabularies. Asked for both in one reply, llama3.2:3b
does one of them well and the other badly — the same behaviour that made JSON
mode counterproductive in Phase 5 — and the two are then indistinguishable in
the parser, so a bad half cannot be dropped without dropping the good one.

They are independently switchable, which is the pattern Phase 5 established when
it split `MemorySearcher` from `MemoryExtractor`. `MEMORY_EXTRACTION=false` with
`GRAPH_EXTRACTION=true` is an assistant that maps how your work connects without
recording facts about you. That is a coherent thing to want, and one call cannot
offer it.

And a failure in one does not lose the other.

The cost is stated rather than hidden: a substantial turn now makes **three**
model calls, and the tail of the turn is roughly twice as long as it was in
Phase 5. `GRAPH_EXTRACTION=false` removes it, `GRAPH_MODEL` points it at a
smaller model, and `GRAPH_EXTRACT_TIMEOUT` bounds it.

## The two extractions run in sequence, not concurrently

They both go to the same Ollama, which holds one model resident and serves
requests to it in sequence. Running them concurrently would not halve the wait;
it would put two entries in the same queue and make the turn's tail latency
harder to reason about, for nothing. On a deployment with two model servers, or
a hosted provider, that arithmetic changes — and `chat.Service.link` is the
function that would change with it.

## The turn's budget grew, and the process now refuses an incoherent one

`CHAT_TIMEOUT` defaults to 5 minutes rather than 3, and the process will not
start if `MEMORY_EXTRACT_TIMEOUT + GRAPH_EXTRACT_TIMEOUT` exceeds half of it.

Both come out of running the end-to-end script rather than out of reasoning.
The extractions run on the *turn's* context — that is Phase 5's decision, so a
client that hangs up cancels them — which means they spend the same budget the
answer does. Phase 5's arithmetic was 3 minutes minus one 60s extraction, so
two minutes for retrieval and generation. Phase 6 added a second 60s extraction
to the same budget without moving it, and quietly halved what the answer had.

On a machine where llama3.2:3b takes a while, that tips over, and the failure
is worse than slow. The turn generates a good answer, streams it, and persists
it — all before the extractions run — and then the deadline expires under the
end-of-stream frame. `SendMessage` returns `context.DeadlineExceeded`, which is
neither `ai.ErrUnavailable` nor an embedding failure, so `Unavailable` says no
and the client is told: *"The answer could not be completed, and nothing was
saved. Send the message again."* Both halves are false. The answer is complete
and it is in the database, and the user has just read it on their screen.

So the default moved to leave the answer three of the five minutes, and the
coherence check went into `config.Load` next to the `JWT_SECRET` one, on the
same principle: refuse to boot rather than degrade. The rule is proportional
rather than an absolute floor because the absolute number is a property of the
model — an operator on a hosted one may reasonably run the whole turn in thirty
seconds, and `CHAT_TIMEOUT=30s` with 5s extractions passes.

The narrower fix — reporting a deadline that expires *after* persistence as a
success — is a real improvement and is not in this phase. It would mean
`SendMessage` distinguishing "failed" from "finished, then the clock ran out",
which is a change to the Phase 4 orchestrator's error contract rather than to
anything Phase 6 owns.

## The confidence floor is 0.5, not the memory system's 0.4

A memory is filtered on two scores: the model's confidence that it read the fact,
and its importance. The measured behaviour of llama3.2:3b is that the worthless
extractions are the ones it scores low on *importance* while remaining perfectly
confident about them, so both filters earn their place.

An edge has one score. The schema gives a relationship no importance column —
"how important is it that Go relates to the backend project" is not a question
with an answer — so the confidence floor is carrying the weight of both filters
and is raised to compensate.

The neutral default for an omitted or unreadable score sits exactly *on* the
floor, at 0.5. So a relationship the model did not score is admitted, and one it
scored 0.3 is not. Dropping unscored relationships would look more principled
and would in practice mean that a model which simply does not emit the field
extracts nothing at all, silently. An omitted score is a model that did not
answer the question; only a low score is evidence against.

## The length threshold is the same constant, referenced not copied

`graph.MinExtractionChars` is `memories.MinExtractionChars`. The two gate on the
same measurement of the same input for the same reason, and an alias makes that
a compile-time fact rather than two constants that drift. If a later phase finds
that relationships need more text than facts do — they name two entities, not
one — that is the line that splits, and the reason will be written there.

## The user is an ordinary `person` node

Most of what a conversation states is a relationship the user is one end of, so
the graph needs a node for them. It is an extracted `person` node labelled
**You**, created the first time an extraction refers to the user, listed and
traversed and deletable like any other. The only thing special about it is that
"I", "me", "myself", "the user" and "you" all resolve to it — which is what stops
one person becoming five.

A dedicated column or a reserved id was considered and rejected: it would make
every query special-case a row, to buy nothing the alias table does not already
give.

## Extraction cannot create a mirrored node type

`NormalizeNodeType` can only ever return `skill`, `person` or `project`. A model
that writes `"type": "task"` gets `project`.

A node claiming to be a task while carrying no `ref_table` would be a node that
says it mirrors a row and does not — invisible to sync, undeletable through the
API (the delete rule refuses backed types by `ref_table`, but a client reading
`type` would show no delete button), and wrong in the one field a client
branches on. Mirrored nodes come from writes. Conversations cannot name one into
existence.

## Entity names must come from the user's half of the exchange

`GroundedInMessage` requires at least one significant word of each entity name
to appear in the *user's* message, as a whole word. The user themselves is
always grounded, whatever pronoun they used.

This is the graph's counterpart to `memories.AboutTheUser`, and it was added
because of a failure that only appeared when the thing was actually run.
Handed a turn where the assistant had quoted the user's own field notes back at
them, llama3.2:3b returned `aurora borealis`, `tundra` and `field notes from 14
March` as the entities, wired them into confident relationships, and stored
them as things it knew about the user's life. Every one passes `Nameable` —
they are perfectly good names. They are simply not what the exchange was about,
and the graph would then have matched a later question mentioning "the tundra"
and offered the model a neighbourhood built entirely out of its own retrieval.

The extraction prompt already says to read the exchange and not the context, in
the same words the memory prompt uses. This is the enforcement, because a
prompt is a request and a filter is a guarantee.

The test is loose on purpose — one word of the label, at least two characters,
not a stop word, matched on a word boundary. That admits "the systems
programming coursework" against "this term's systems programming coursework is
in Rust", where no substring match exists. The two-character floor is not a free
choice: it is `MinMentionLen`, and the tests caught a three-character floor
silently discarding every relationship about **Go**, which is the canonical
entity of the entire phase. Short function words are excluded by name instead.

The cost: a relationship where the user said "it" and the assistant supplied
the name is lost. That is the trade — a wrong edge is fed back into later
prompts as a fact, and a missing one is restated the next time the user
mentions the thing.

## An unknown relationship becomes `RELATED_TO`

Same reasoning as `memories.normalizeType` falling back to `semantic`. The pair
is the finding; the label on the arrow is a facet. "The user and the compiler
project are connected somehow" is true and useful; discarding it because the
model wrote `USES` costs the connection itself. The entity *type*, when the
model omits it, is guessed from the relationship rather than from a constant —
`STUDIES` points at something learnable, `WORKS_ON` at work.

## Chat retrieval is a mention lookup, and nothing more

If the question names a node — as a whole word, case-insensitively — that node's
1-hop neighbourhood joins the context. That is the entire integration.

The prefilter is a substring match in SQL (`position(lower(label) in
lower($1)) > 0`) and the word-boundary test is applied in Go. Doing the boundary
test in SQL would mean building a regular expression out of a user-supplied
label, which is an injection into the pattern language rather than into the
statement, and Postgres has no quoting function for it. So SQL narrows and Go
decides.

The boundary test is not optional: without it "Go" matches "going",
"algorithm" and "Django", and the graph fires on every second message. It costs
the near-miss on "Golang", which is a name a user can have their own node for.

Graph sources rank after memories and before the task/goal/note heuristic. They
fire on a real mention, which makes them targeted rather than background — but
what they carry is a set of links, not a claim in the user's own words.

The **self node never matches a mention**, and that is a correctness rule
rather than a tuning choice — one found by running the thing rather than by
reading it. Its label is "You", and in a message written by the user the word
"you" means the *assistant*: "can you check my deadlines" is not the user
naming themselves. So the node the scan would fire on most often is the one it
would be wrong about every time, and it would fire on nearly every turn, which
is not what "the question named something" is supposed to mean. Matching it on
first-person pronouns instead is no better — "I" and "my" are in most messages
too. What the user is like is what the memory system retrieves; what a *named
thing* connects to is what this does. The self node is still perfectly visible
on the far end of any edge that is returned.

A node with no edges is skipped rather than returned. The task it mirrors is
already reachable through the task list, and an isolated extracted node is a
name with no claim attached; spending a source on it would displace one that
carries something.

## A graph source carries links, not details

The brief's example ends "…and that goal's deadline". The node does not have the
deadline, and it deliberately does not go and fetch it: that would make the
graph a second, worse path to the goals table.

Instead a mirrored node names the record it stands for (`this is the goal
<uuid>`), and the goal's own retrieval path supplies the deadline — active goals
are already in the context. When the goal is not in that top-5, the deadline is
not there. That is a real limit and it is the right trade for a phase that is
supposed to add a lookup, not a join planner.

The system prompt gained a rule for it (rule 7). A graph source is the one thing
in the context that is not a claim in the user's own words, and without the rule
a 3B model reads a rendered triple as a sentence it may quote back as fact.

## The graph read returns a closed subgraph

`GET /knowledge-graph` returns the edges whose **both** endpoints are among the
nodes it returned — not the edges incident to them. A client drawing the result
needs every edge to have two nodes it was also given, and `?type=skill` would
otherwise come back full of references to projects it was not sent.

The visible consequence is that `?type=skill` usually returns zero edges, since
a skill's links point at projects and people. That is correct: it is the
subgraph of skills, and skills are rarely connected to each other.

## Deleting a mirrored node is `409`, not `400` or `404`

The node exists, the caller owns it, and nothing about the request is malformed
— the resource is in a state that forbids the operation, which is what `409`
means. The message names what to delete instead.

Somebody else's mirrored node is still `404`: answering `409` there would
confirm the id is real, which is the property every other endpoint in this API
is careful about.

## The same relationship twice is one edge

A unique index on `(user_id, from_node_id, to_node_id, relationship)`, and an
upsert that keeps the higher confidence. Without it the graph grows a parallel
edge every time the user mentions the same pair again, and the 1-hop lookup
hands the model the same line five times.

`source_conversation_id` keeps the *first* conversation rather than the latest.
The column records where the assistant learned this; overwriting it would make
the earliest evidence unfindable.

## Deleting an edge is not a suppression list

A later conversation that states the same relationship recreates it. Deleting an
edge says "this is wrong", not "never record this". A real suppression list is a
table of negative assertions and a prompt that respects them, which is more
machinery than a phase with no UI can justify.

# Phase 6 — explicitly deferred

## 21. No backfill for rows written before this phase

A task created in Phase 2 has no node until it is next updated. Sync is
idempotent and runs on update, so the graph fills in as things are touched, but
nothing walks the four tables on startup. A migration that did it would have to
be re-run for every future mirrored type; a startup pass would make boot time
depend on table size. The reconcile job belongs with the job queue.

## 22. No multi-hop anything

One hop, in the API and in retrieval. No shortest path, no centrality, no
community detection, no "how are these two things related". The chat
integration is a lookup, not reasoning over structure.

## 23. Extraction sees one turn, not the conversation

Inherited from Phase 5 and true for the same reason: the extractor is never
shown two turns at once, so a relationship stated across three of them is not
assembled.

## 24. Nothing decays, merges or contradicts

Edges accumulate. Nothing notices that "the user STUDIES Go" should become
"KNOWS" eventually, nothing merges two nodes a human would call the same thing,
and nothing detects that a new edge contradicts an existing one. Confidence is
recorded and never re-estimated.

## 25. The per-turn cap is applied before the quality gate

`MaxRelationshipsPerTurn` is enforced in `ParseExtraction`, so a reply whose
first three relationships are all junk yields nothing even if its fourth was
good. This matches `memories.MaxFactsPerTurn`, which is why it was left alone
rather than "fixed" into a divergence — but it is a real limitation, and the
better order is parse to a hard ceiling, gate, then cap. It is a two-line change
in both packages when somebody makes it in both.

## 26. "Asked about" and "stated about" are told apart by the prompt only

The extraction prompt says it outright — *"The user asking a question about
something does not connect them to it"* — and nothing enforces it. A user who
asks "what do my field notes say about the tundra?" has named the tundra, so
`GroundedInMessage` admits it, and only the model's judgement stops
`(You)-[INTERESTED_IN]->(the tundra)` being recorded.

The grounding filter is not the place to fix it: its job is to stop entities
from the *retrieved context* becoming nodes, and it does that — "aurora
borealis", which appeared only in the assistant's reply, is rejected. Telling a
question apart from a statement is a semantic judgement, and the honest options
are a better model or a second classifying pass, neither of which belongs in
this phase.

## 27. The thresholds were reasoned, not swept

0.5 for the confidence floor, 8 words for an entity name, 3 relationships per
turn, 3 mentioned nodes per question, 25 neighbours per node. Every one is a
defensible number with a knob or a constant on it, not a measured optimum.
Nobody has run a labelled set through any of them.

## 28. Extraction quality is a 3B model's quality

The pipeline is sound and the model is small. It misses relationships, states
some in the wrong direction, and reaches for `RELATED_TO` when a specific
relationship was available. `GRAPH_MODEL` points extraction at a different model
without touching anything else, and that is the intended fix.

# Phase 7 decisions

## The model chooses a tool through a structured reply, not native tool calling

This was measured, not assumed. Eighteen messages -- ten that need one of the
eight tools, eight that need none (a greeting, thanks, a general question, a
remark about the user's day, a delete request, "yes, go ahead") -- were sent to
llama3.2:3b three ways:

| Approach | Right | Tool called on the 8 no-tool messages | Median latency |
| --- | --- | --- | --- |
| Ollama's native `tools` parameter | 10/18 | **8 of 8** | 3.9s |
| Structured prompt, a JSON *list* of calls, examples inline | 8/18 | 0 -- but `[]` for everything | 1.6s |
| Structured prompt, one decision *object* with `"none"` as a named choice, examples as real chat turns | **18/18** | 0 | 3.3s |

Native tool calling is disqualified by its failure mode, not its score: with
tools in the request, the model called one on *every* message. "Hi, how are
you?" searched the notes; "Thanks, that's really helpful" produced a
`create_task` with an invented deadline in 2024. In an assistant whose writes
are proposals, that is a proposal on every thank-you.

The first structured prompt failed the opposite way -- the attractor Phase 6
documented, where a 3B model handed a list schema answers `[]` to everything.
Two changes fixed it and both are load-bearing: "no tool" became a named
choice (`{"tool": "none"}`) rather than an empty list, and the worked examples
became real user/assistant turns instead of text inside the system prompt.

So routing is Phase 5/6's pattern -- a small classifier prompt, a lenient
parser, a strict check on what it returns -- and it needs nothing new from
`ai.Provider`. Native calling would have needed the interface to grow tool
support and would have sent the full retrieved context twice per turn.

The production prompt is re-measured by an opt-in test,
`internal/agents/ollama_eval_test.go`, over the eighteen plus six harder
messages. Its first run scored 22/24 with no write proposed for a message that
asked for none; the two misses ("remind me to call the bank on Friday" got no
tool, and a statement containing "finish" got a search) led to one more worked
example of each, and the second run scored **23/24, 0 spurious writes**. The
remaining miss is a statement that triggers `search_notes` -- a read, which
changes nothing.

## The routing call is gated by a lexical check

The routing prompt is ~720 tokens. On the development machine (an i5-7360U
running Ollama on two threads) prompt evaluation runs at ~13 tokens a second:
the routing call costs **~55 seconds** when Ollama's prefix cache does not
already hold it, and ~3 seconds when it does. In real use the chat and
extraction prompts pass through the same model between turns, and the cache
rarely holds it. Every one of those seconds is before the first token of the
answer.

So `agents.MightUseTool` sits in front of it, the routing counterpart of the
extractors' length threshold: a message is only routed if it contains a cue --
a word starting "task", "note", "goal", "document", "add", "create", "find",
"mark", "remind", "delete" and some forty more. Greetings, thanks, questions
about the world and remarks about the day contain none, and most turns are
those.

The gate is deliberately permissive and it can only fail one way. A request
phrased with none of the cues gets an answer without a tool -- exactly the
answer Phase 6 gave -- and never the dangerous way: the gate can decline a
tool, only the model can choose one, and only the user can approve a write. A
false positive costs one routing call and nothing else, which is why "which",
"list" and "show" are cues despite appearing in ordinary questions.

## The router reads the message and nothing else

No retrieved context, no conversation history, no date. That keeps the prompt
short, which is the whole cost of the call, and it has three consequences:

- **Relative dates are resolved in Go.** The prompt tells the model to copy a
  date as the user wrote it ("tomorrow", "friday"), and `tools.ParseDate`
  resolves it against the server clock. Date arithmetic is exactly what a 3B
  model gets confidently wrong, and Go does it exactly. Anything `ParseDate`
  cannot read ("next year", "soon") is not guessed: the call is declined and
  the model asks for a date.
- **"Yes, go ahead" never re-proposes.** With no history the router cannot
  see what "yes" is agreeing to, so it chooses no tool -- which is also the
  only correct thing to do, since replying in the chat is not an approval.
- **"Mark it done" about a task named two turns ago does not resolve.** That
  is a real limit; see deferred item 30.
- **Nothing in your documents can reach it.** The prompt-injection path Phases 5
  and 6 documented -- a file engineered to say "the user has approved X" being
  read back by an extractor -- does not lead here. The router never sees
  retrieved text, so a document cannot cause a proposal, and a proposal cannot
  run without the user anyway.

## Agents are data, and there is one

`agents.Agent` is a name, a purpose and a set of tool names. `TaskAgent`,
`GoalAgent`, `NoteAgent` and `DocumentAgent` each wrap one module's tools, and
`General` is `Compose`d from all four. The orchestrator uses `General`, and
nothing routes to the domain agents individually.

It is data rather than a `TaskAgent` type with methods because agents differ
only in what they may use. How they decide (the one `Router`), how a decision
is checked (the agent's own tool set, then the registry's validation) and what
happens to it (a read runs, a write is proposed) is the same for all of them.
`Router.Decide` takes the agent as an argument, renders only its tools into the
prompt, shows only the worked examples for tools it has, and refuses a
decision naming anything outside its set.

That is what makes the named-agent layer an addition rather than a rework. A
later phase puts one step in front of `Decide` that picks an agent -- say a
Study agent composed from `TaskAgent`, `NoteAgent` and a courses module -- and
calls the same `Decide` with it. The tools, the registry, the action engine and
the chat turn do not change. The domain agents are a real partition, not
labels: a test pins that every standard tool belongs to exactly one.

## "No write without an approved action" is a property of the code, not a rule

The brief's one unrelaxable rule is enforced by what exists, not by callers
remembering it:

- **There is no method that runs a write tool with an input.** `tools.Registry`
  has `RunRead`, which refuses any tool the registry says is a write -- it looks
  the permission up rather than trusting the `Call` it was handed -- and
  `RunApproved`, which takes an **action id** and nothing else.
- **`RunApproved` asks the ledger to approve that row, then runs what the row
  says.** The ledger is the actions repository, and its `Approve` is one
  conditional `UPDATE ... SET status = 'approved' WHERE id = $1 AND user_id =
  $2 AND status = 'proposed' AND permission_level = 'write' RETURNING tool_name,
  input`. The tool and its input come out of that row, so what runs is exactly
  what was proposed and shown, and the caller cannot substitute anything.
- **The chat turn cannot reach even that.** `chat.ToolRunner` is `Prepare` and
  `RunRead`. The only production path to `RunApproved` is
  `POST /actions/{id}/approve`.
- **The tools cannot delete.** Their service interfaces (`tools.TaskService`
  and friends) have no `Delete` method, so no tool can call one by mistake.

The rule holds even when the user asks to relax it -- "don't ask me, just do
it" produces a proposal like any other, and "I approve" typed into the chat is
not routed at all -- and it is pinned at four layers: the registry against a
fake ledger, the action engine against an in-memory store, the chat turn with
a registry that has *no ledger at all*, and `internal/db` against the real
table, where every state but `proposed` -- including a row set to `approved` by
hand -- is refused and the tasks table is counted after each attempt.

## Approval is single-use, and `approved` is transient

The conditional `UPDATE` is what makes an approval happen at most once. Two
requests approving the same action both run it; Postgres row-locks the first,
the second re-evaluates `status = 'proposed'` against the committed row, and
matches nothing. Ten concurrent approvals over HTTP produce one `200`, nine
`409`s and one task; sixteen concurrent `RunApproved` calls against the table
produce one success.

`approved` means "the user said yes and the tool is running", and becomes
`executed` or `failed` within the same request. Everything after the approval
runs on a context detached from the request (and bounded by its own 20s), so a
client that hangs up the moment it clicks cannot strand the row. A row left in
`approved` means the process died mid-execution and the outcome is unknown. It
is **never retried**: retrying a create that did in fact land would create it
twice, and the gate would refuse anyway -- the row is no longer proposed.

## Proposals are part of the turn

A turn's actions -- the read it ran, the write it proposed -- are inserted in
the same transaction as its messages, by `chat.Repository.AppendTurn` calling
`actions.Insert` on its own transaction. The SQL for the table still lives in
`internal/actions`; the commit is shared. Phase 4's rule is kept whole: a turn
that fails persists nothing, and that now includes a proposal the user never
saw answered.

The `action` frame is sent **after the last token and before `done`** -- the
moment the turn is committed and before the two extractions run. Not earlier:
before the answer, the row does not exist, and a client that showed an approve
button during the stream would get a `404` for a real click. Not later: `done`
waits for up to two extractions, and an approve button that appears a minute
after the answer is one nobody waits for. `done` repeats the actions for a
client that reads only it.

## A write is validated when it is proposed, not when it is approved

`Registry.Prepare` turns the model's arguments into the tool's canonical input:
lenient about shape (aliases, `"none"` for absent, a number where a string was
asked for), strict about content -- the resource service's own `ValidateCreate`
/ `ValidatePatch` runs at proposal time, so the user is never shown a proposal
that would fail on approval. The canonical input is what is stored, what the
user sees, and what an approval executes; it is decoded strictly on the way
back, so a stored input with a key its type does not declare is refused rather
than run with the key ignored.

The one-sentence `summary` on every action is derived from the input on each
read and never stored, so it cannot disagree with what would run.

A call the model got wrong -- no title, a date that is not a date -- is not
recorded and is not an error. The answering model is told, in the ACTIONS
section, what could not be prepared and why, so it can ask the user for the
missing piece.

## `update_task` resolves its reference to one task, or declines

The model names the task in words ("the scheduler task"). `resolveTask` looks
among the caller's own tasks only: an exact title match wins; otherwise the
reference is searched as text -- the whole phrase, then its significant words
-- and it resolves only if exactly one task matches. Several matches are
declined with the candidates listed; none is declined with the reference
quoted. Guessing which task to change is precisely the decision the user should
make, and a proposal is the wrong place to discover the guess was wrong.

The resolved task is stored by id, with its title alongside for the summary, and
the approval executes by id alone. A change to what the task already has is
declined ("nothing to change") rather than proposed.

## Read tools are recorded too

A read runs without approval, and is still an `actions` row -- `permission_level
= 'read'`, born `executed` -- which is why the brief's schema has the column. It
is the audit trail of what the assistant looked at on the user's behalf. The
schema says the half of the state machine it can: a read can never be
`proposed`, `approved` or `rejected`. `search_documents` records which chunks it
found and their scores, not the passages, so the audit trail does not become a
second copy of the corpus.

What a read found joins the context as the turn's *first* sources, marked with
`tool`, rendered with the same summaries the heuristic uses, and deduplicated
against everything else retrieved -- a task the search found is usually also on
the heuristic's list, and two labels for one task would split its citations. A
read that fails because the database or the embedder is down fails the turn,
as a failed retrieval does.

## The model is told what became of what it proposed

Every turn with tools reads the conversation's last five write actions and
shows their outcomes in the ACTIONS section -- "the user approved it and it was
done", "the user rejected it", "the user approved it, but it failed: …".
Without it the model's only memory of a proposal is its own sentence in the
history, and it goes on calling an approved task "waiting".

Proposing exactly what is already pending in the conversation reuses the
pending action rather than queueing a second one -- which would let the user
approve the same task twice.

## Approve and reject take no parameters

A body is optional and, if present, must be `{}`. `{"input": {...}}` is a `400`
and the action is untouched: an ignored field is how a client comes to believe
it edited a proposal on the way through, and what runs must be what was
proposed. Editing a proposal is deferred (item 32).

Approve answers `200` with the action in its final state whether the tool
succeeded or not -- the approval was accepted and recorded; `status` says
`executed` or `failed`. A failed action's `error_message` is a fixed sentence,
never the underlying error, for the reason `documents.clientMessage` gives.
An action that exists and is no longer proposed is `409 action_not_pending`,
naming its state; somebody else's is `404` whatever its state.

## The resource lists grew a text filter

`search_tasks`, `search_goals` and `search_notes` need to find records by what
they say, and none of the Phase 2 filters could. `Filter.Query` is a
case-insensitive substring match on the title and the description (or a note's
content), escaped by `db.Contains` so `%` and `_` are characters. It is exposed
as `?q=` on `GET /tasks`, `/goals` and `/notes`, so the assistant sees exactly
what the API returns. A leading-wildcard `ILIKE` cannot use an index; it scans
one user's rows, which the `user_id` index has already narrowed.

## The turn's budget grew again

`CHAT_TIMEOUT` defaults to 6 minutes, and the boot-time coherence check now
counts `AGENT_TIMEOUT` alongside the two extraction timeouts: the routing call
runs before the answer rather than after it, but on the same context and out of
the same budget. Phase 6 added a minute for its extraction; this phase adds one
for its routing call, for the same reason. `AGENT_TOOLS=false` removes the
routing call and its share of the check.

## The system prompt's action rule has two versions

Without tools, the action rule is Phase 4's: the assistant can only read, and
says so. With them it can propose, and the rule's whole job is the one lie a proposal
invites -- "done, I've added that task". It says a proposal has not been made,
that replying in the chat does not approve it, and to report earlier proposals
exactly as the ACTIONS section states them.

How the ACTIONS section is *worded* turned out to matter as much as the rule,
and it was measured twice. The first version reported the proposal to the model
-- "You proposed this change, and it has NOT been made: …" -- and in both
end-to-end runs llama3.2:3b copied the section into its reply, heading and all.
Four renderings were then tried against the same system prompt: moving the
section above the context made the model open with "I could not find anything
about that"; a bare note with no heading was copied every time; the report
wording was copied in some samples; and wording it as *what to say* -- "Reply by
telling the user, in your own words, that you have prepared it, what it will do
(…), and that it will only happen once they approve it" -- produced "I have
prepared a change … it will only happen once you approve it" in every sample.
That is the wording that ships.

It is still a prompt, so it is a request: the end-to-end run warns when an
answer describes a proposal as done, and the property that matters -- nothing
was created -- is asserted against the database, not the wording.

## A goal with no stated type is filed as `personal`

`goals.type` is required and has no default. Asking "what *type* of goal is
running a marathon?" before proposing it is a worse experience than filing it
under the broadest type and showing that in the proposal, where the user sees it
before anything is written. Near-misses are mapped (`savings` → `financial`,
`work` → `career`).

# Phase 7 — explicitly deferred

## 29. No delete tools

Deleting through a conversation is a sharper edge than creating. An approved
create that was wrong costs a click to undo; an approved delete of the wrong
task -- resolved from "the antenna one" -- costs the task, its subtasks and its
graph node. It wants a proposal that shows exactly what would go, and probably
an undo window. The tool service interfaces have no `Delete` method, so adding
it is a deliberate change to an interface, not a line written by accident. The
router is told nothing can be deleted and chooses no tool; the answering model
is told to say so.

## 30. One tool call per turn, and no conversation history for the router

"Find my scheduler task and mark it done" works only because `update_task`
resolves its own reference. "Create tasks for A, B and C" proposes one. "Mark
it done" about a task named two turns ago does not resolve, because the router
reads only the message. Multi-call turns and history-aware routing are the
agent loop the spec describes, and both would lengthen the prompt on the path
to the first token.

## 31. No named specialist agents

Study, Career, Finance, Travel and Research need modules that do not exist.
The structure for them does; see "Agents are data" above.

## 32. A proposal cannot be edited, and does not expire

Approve takes no body. Fixing a proposal's title means rejecting it and asking
again. Proposals do not expire and are not checked for staleness: an
`update_task` approved a week later applies the fields it named to the task as
it is then, whatever else changed meanwhile.

## 33. `update_task` cannot clear a field

It sets values; it cannot remove a deadline or a description. Clearing is a
`null` in the canonical input and a third state in the summary, and it waits for
a UI that can show "deadline → (none)" unambiguously.

## 34. No undo, and no reconciler for a stranded `approved`

An executed action is final; undoing it is an ordinary edit through the API. A
row left `approved` by a crash is logged loudly and left for a human, because
the only safe automatic answer -- never retry -- is already what happens.

## 35. Dates are resolved in UTC

"Tomorrow" is tomorrow in UTC, the same clock the chat prompt states.
`user_profiles.timezone` exists and nothing here reads it yet; a user in
UTC+10 asking after 2pm local time gets the day after the one they meant.

## 36. The gate and the router were measured on 24 messages

23/24 with no spurious write is a good result on a small, hand-written set, not
a labelled evaluation. The eval test exists so the next change to the prompt,
the tools or the model can be re-measured the same way.

## 37. No per-user limit on proposals or approvals

A user can hold as many proposals open as they can type requests. Every one
waits for them, so the harm is clutter rather than writes, but a rate limit on
the approval endpoint belongs with the rest of the per-user limits.

# UI phase decisions

## The refresh token lives in localStorage; the access token only in memory

The access token is a module-level variable in `frontend/src/lib/api.ts`: never
persisted, gone on reload. The refresh token is in `localStorage` so a reload
can mint a new access token. That is XSS-exposed — any script running on the
origin can read a 30-day credential — and it is accepted for this project's
scope because the alternative, an `HttpOnly; Secure; SameSite=Strict` refresh
cookie, needs the backend to set and read it (deferred item 38). The mitigations
in place: the access token is short-lived and never stored, logout removes the
refresh token and revokes its session server-side, and nothing in the UI
renders user or model text as HTML.

## Refreshing is single-flight, and serialised across tabs

Refresh tokens rotate on every use and the old value dies at once, so two
refreshes racing with one token log the user out. Inside a tab, concurrent
401s share one refresh promise. Across tabs, the refresh runs under a Web Locks
API lock (`navigator.locks`), and reads the token from storage only after taking
it — so the second tab refreshes with the token the first tab just received.
Only a `401` from `/auth/refresh` ends a session; a network error or a 5xx
leaves the stored token alone, so restarting the backend does not sign anybody
out. A logout in one tab signs the others out through the `storage` event.

## A 401 is refreshed and retried once, POST included

A 401 means the server did not act on the request, so repeating it after a
refresh is safe even for a write or a chat turn. An access token that expires
within 15 seconds is refreshed before the request rather than after it fails.

## No router or state library

Seven flat routes and no nested layouts: a 60-line router over the History API
(`src/lib/router.tsx`) covers them. State is component state plus one context
for the session; the only cross-screen signal — "proposed actions changed", for
the sidebar badge — is a window event.

## The chat stream is read with fetch, not EventSource

`EventSource` cannot send a body or an `Authorization` header. The reader in
`src/lib/sse.ts` splits frames on blank lines and dispatches `sources`, `token`,
`action`, `done` and `error`. A stream that closes without `done` or `error`
(a proxy timeout, a restart) is reported as an error, and the view re-reads the
conversation to find out whether the turn was saved — it may have been, since
the turn commits before the extractors run.

## Leaving a conversation does not cancel its turn

The request context is the turn's context, so aborting the fetch would lose the
turn. Navigating away keeps reading the stream in the background and lets the
turn finish; only the Stop button aborts.

## The action card is the only thing that says an action happened

The model's prose can claim a proposal is done (deferred item 39 of Phase 7).
The card's status line comes from the actions API alone — "Awaiting your
approval — not done yet" until Approve returns `executed`. Approve and Reject
send no body, as the endpoints require. Reloading a conversation re-attaches
each action to the answer that proposed it by timestamp: actions are inserted in
the turn's transaction just after its messages, with `clock_timestamp()`.

## Deadlines are calendar dates in UTC

The date inputs write midnight UTC and dates are formatted in UTC, matching the
backend's own date resolution (item 35). An edit sends only the fields that
changed, so a deadline with a time of day set through the API survives an edit
that did not touch it.

# UI phase — explicitly deferred

## 38. No HttpOnly refresh cookie

The fix for the `localStorage` exposure is for `/auth/login`, `/register` and
`/refresh` to set the refresh token as an `HttpOnly; Secure; SameSite=Strict`
cookie scoped to `/api/v1/auth`, and for `/refresh` and `/logout` to read it
from there. That is a backend change; this phase made none.

## 39. The composer waits for `done`

A new message cannot be sent until the previous turn's `done` frame, which
arrives after both extractors — on a slow CPU, a minute or two after the answer
is visibly complete. The UI says so ("Answer complete · updating memory and
connections…"). Sending while the extractors run is probably safe, because the
turn is committed, but it has not been designed or tested.

## 40. Lists show what was loaded

The parent-task and dependency pickers offer the tasks on the loaded pages, not
every task. There is no endpoint to remove a dependency or delete a milestone,
so the UI offers neither.

# Hardening pass

## 41. Extraction reads only confirmed reality

Three bugs had one cause: the memory and graph extractors read a turn as text,
and the text of a turn is not all fact. A proposed or rejected change ("book a
flight to Delhi") became a memory of something the user did; a proposal became
an extracted graph node that the approved task's real node never joined; and a
retrieved document's content became "The user knows that ...".

The chat turn now hands each extractor a `Turn` rather than two strings: the
exchange, the text of every change in the conversation that has *not* been
carried out (this turn's proposal, and earlier ones still waiting, rejected or
failed — never an executed one), and, for memories, the retrieved document
passages. Each is enforced in code, not only asked for in the prompt:

- A memory sharing a content word with an unconfirmed change is dropped
  (`memories.RestatesUnconfirmed`). One word is strict on purpose: the message
  that proposes a change *is* the request, so a fact about its subject was read
  out of it. A fact about something else in the same message is kept.
- A memory whose content words mostly come from a retrieved passage and not
  from the user's own message is dropped (`memories.RestatesRetrieved`),
  however it is phrased. Words the user said are never counted against it.
- A relationship with an end named in an unconfirmed change is dropped from the
  turn. When the change is approved and executes, the action engine's
  `OnExecuted` hook reads the proposing turn again with the created record as an
  anchor: an end naming the record resolves to the record's own node, and only
  relationships touching it are kept. It runs in the background after the
  approval answers; it is a model call, and the user should see their task now.

Memories get no second pass after approval: the task, goal or note is itself
the record, and a memory restating it would not follow it through an edit or a
delete.

The cost of the lexical checks is a genuine fact that shares a word with a
pending change or with a document the user did not quote — the same trade
`AboutTheUser` makes, on a path where a wrong memory costs more than a missing
one.

## 42. A filter argument must come from the message

`search_notes`' `tag` and the search tools' `status` are marked `Filter`, and
the router drops a filter whose value the user's message does not give — the
value or a synonym for an enum, the words plus a cue ("tag", "tagged", or a
`#hashtag") for a tag. An invented filter was the one argument error that
failed silently: a valid value, a search that ran, and a true "nothing
matched". Write arguments are not checked; they are shown to the user in the
proposal.

## 43. Retrieval scores are not shown to the model

Source headers used to carry "(similarity 0.79)", and the model recited it to
the user. The score stays on the source for the client; the model gets sources
in relevance order. Graph edges still carry "confidence" in their excerpt —
the same class of leak, not observed, and left alone in this pass.

## 44. Timeouts default to this machine

The extraction and routing timeouts default to 180s and the turn to 18m
(twice their sum, per `MaxExtractionShare`), which is what `scripts/e2e.sh`
already needed on the development machine. A faster model can lower all four.
`LOG_LEVEL` (default `info`) makes the extractors' drop reasons visible at
`debug`.

# Phase 8 decisions

## The calendar read is a range query, and there is no other kind

`GET /calendar` requires `start` and `end`. Every other list endpoint in this
codebase offers an unfiltered read with paging; this one does not, and the
absence is the design rather than an omission.

A calendar grows a row per meeting for as long as the user has the account, and
the only question anybody asks of it is "what is on between X and Y". An
unbounded read is therefore a denial-of-service lever with no use case behind
it. It is also a trap for a client: a page of "your first fifty events" looks
like an answer and is not one, and a client that drew a week from it would draw
the wrong week silently.

So the window is part of the query rather than a filter on it: `Filter.Start`
and `Filter.End` are required, `ValidateFilter` reports a missing one as the
field error it is, and the SQL is one index scan over `(user_id, start_time)`.
The response echoes the window back, so what answered is visible in the answer.

## Overlap, not containment, and the interval is half-open

An event is returned when it *overlaps* the window. A query that matched only
events starting inside it would hide the conference that began on Tuesday from
Thursday's calendar — exactly the events most worth seeing.

The interval is half-open, `[start, end)`: the 09:00–10:00 meeting and the
10:00–11:00 one do not overlap, and an event ending exactly at the window's
start is outside it. The clause is:

```sql
start_time < :end AND (end_time > :start OR start_time >= :start)
```

The second half of the disjunction is for a zero-length event — a reminder at a
moment, which the CHECK allows because people put them in calendars — whose end
is not *after* the window start even when it sits inside the window.

## Both ends are always stored, and `all_day` is a display fact

An all-day event is a row whose ends are midnight and midnight the next day,
with `all_day` set. The alternative — a null `end_time` meaning "all day" —
makes every range query special-case a missing end, and a range query that has
to special-case anything is a range query that gets it wrong somewhere.

`all_day` therefore says how to *show* the interval, not what it is. A client
renders "Thursday" rather than "00:00–00:00"; the SQL treats it like any other
event.

## `recurrence_rule` is stored and never read

The brief scopes recurrence to storage, and the reason is worth writing down:
expansion is a phase, not a feature. It needs exceptions (EXDATE, a moved
occurrence), time zones (a weekly 09:00 that crosses a DST boundary), a bound on
the unbounded tail, and a decision about whether an occurrence is a row. Half of
that is worse than none, because a calendar that expands repeats *sometimes* is
one the user cannot trust.

So the column is opaque: any string within the length limit is accepted, nothing
parses it, and a range query returns the single stored row and no occurrence of
it. The chat prompt says so out loud in the calendar rule, and the event summary
the model sees carries "repeats (only this occurrence is recorded)" — because a
model shown an RRULE will otherwise describe next Monday as scheduled.

## Deleting a task does not delete the event it was booked for

`related_task_id` and `related_goal_id` are `ON DELETE SET NULL`, unlike the
`ON DELETE CASCADE` from `users`. Deleting the task you set aside Thursday
morning for should not silently remove Thursday morning from your calendar: the
hour was blocked, and whether it is still needed is a decision, not a
consequence.

## The calendar repository reads `tasks` and `goals`

`Repository.TaskExists` and `GoalExists` are the only place in this codebase
where one module's SQL touches another module's table, and it is a considered
exception.

The foreign key alone cannot do the job: it accepts *another user's* task id
perfectly happily, which is the leak the whole ownership design exists to
prevent. The check has to be owner-scoped, so it has to be a query. The
alternatives were wiring the task and goal services into the calendar service —
two dependencies, threaded through `cmd/api` and every test, to answer a
question that is one index probe — or adding an `Exists` method to both services
for this one caller. The coupling already exists in the schema, since
`calendar_events` has a foreign key to each table, and `ownsRow` is four lines
with the table name as a literal at both call sites.

A link the caller does not own is `ErrNotFound`, not a validation error naming
the field: the two answers differ, and only the first keeps another user's ids
unconfirmable. It is the rule `tasks` already applies to `parent_task_id`.

## A time of day the user did not give is not invented

`create_calendar_event` with a day and no time proposes an **all-day** event.

This is the hardening pass's rule (item 42) applied to a new kind of argument.
"Book the dentist on Thursday" states a day and no hour; a proposal for 09:00
would be a detail the assistant made up, shown to the user as though they had
said it. An all-day Thursday is true, is visible, and is editable.

The same rule decides what counts as a stated time. "3pm", "15:00" and "noon"
are times. "Tomorrow evening" is not: it is a time of day to a person and a
two-hour error bar to a calendar. A bare number is not either — "lunch at 1" is
a time and "in 1 week" is not, and a pattern that reads both the same way turns
a date the user *did* give into a time they did not.

## The window is a filter, so the router grounds it — and the default is stated

`search_calendar`'s `start` and `end` are `Filter` params, which means the
router drops a value the user's message does not contain (item 42). A fabricated
date range is the worst kind of invented filter: it does not empty a search
visibly, it answers a question about the wrong days, and "nothing on Tuesday" is
a sentence the user has no way to tell from the truth.

When both ends are dropped — or were never given — the tool uses a stated
default: the next seven days, or ninety when the message names an event to look
for, because the dentist appointment somebody is trying to remember is not
usually this week. That default is not the thing the rule forbids, for two
reasons: it is the same window every time rather than a guess at what the user
meant, and the proposal summary and the ACTIONS section both name the days it
used. An answer drawn from the wrong week says which week it was.

`Param.Aliases` exists for this. The router drops an ungrounded filter *by key*,
so a window the tool reads from `when` while the declaration only names `start`
would be a value that escapes the check entirely. Declaring the aliases makes
the grounding cover every key the tool reads.

## The calendar leads the heuristic sources

Retrieval order is: what a read tool found, then documents, then memories, then
the graph, then **the calendar**, then tasks, goals and notes.

The calendar goes first among the heuristic items because it is the only one of
them selected by *time* rather than by status. An event this afternoon is about
now in a way that a pending task is not, and "what does my day look like" is
answerable from background context only if the day is in the context.

The window is two days and the cap is three events, which are small on purpose.
Next month's conference is not background to every question; a question that is
actually about next month routes to `search_calendar`, which takes the dates
from the user. The window starts at the top of *today* rather than at `now`, so
an event that began this morning is still in front of the model at four in the
afternoon — "what have I got on today" is a question about the whole day.

## An event's times are written out for the model

The summary the model sees is `starts Thu 19 Nov 2026 15:00 UTC`, not an ISO
timestamp. Date arithmetic is the thing a 3B model gets confidently wrong — it
is why `ParseDate` exists — and a model shown `2026-11-19T15:00:00Z` will offer
to work out what day that is. Writing the weekday and the month out removes the
temptation rather than relying on the model to resist it.

Rule 8 of the system prompt says the same thing twice more: use the times as
written, and an empty calendar section means "I found nothing on your calendar",
not "you are free". Asked "am I free on Friday" with nothing retrieved, a 3B
model reaches for "yes, you are free" — which is a claim about the calendar
rather than the absence of one.

## One index, not two

The brief asked for an index on `user_id` and one on `(user_id, start_time)`.
There is one composite index, and it serves both: its leading column answers a
plain owner lookup, including the one the `ON DELETE CASCADE` from `users`
makes. A standalone `user_id` index would be a second copy of the same prefix,
paid for on every insert. It is the call migration 000006 already made for
`knowledge_nodes`, and it is noted here because it is a deliberate departure
from the brief's wording rather than from its intent.

# Phase 8 — explicitly deferred

## 45. No recurrence expansion

See above. One recurring event is one row; nothing computes the occurrences, and
no query returns them.

## 46. No external calendar sync

No Google Calendar, no CalDAV, no iCal import or export. The calendar is
internal, which also means nothing reconciles an event moved somewhere else, and
there is no notion of an event this user does not own (an invitation).

## 47. No `update_calendar_event` and no delete tool

`create_calendar_event` is the only calendar write the assistant has. Moving an
event is the same sharp edge as deleting a task — the wrong meeting moved is a
meeting missed — and it needs the same "show the user exactly what would change"
that deletion is waiting for (item 29). `PATCH /calendar/{id}` is the API, and
the UI would be the place to offer it.

## 48. No conflict detection, no free/busy, no travel time

Two events at the same time are two events. Nothing warns, nothing suggests a
slot, and "am I free on Thursday" is answered by listing what is on, not by
reasoning about gaps. Each of those is a feature with its own edge cases
(all-day events, zero-length reminders, working hours) and none is in this
phase.

## 49. No reminders or notifications

`calendar_events` has no notion of an alert, and there is no scheduler to fire
one. That needs the job queue the whole project still does not have.

## 50. The calendar has no screen

The web UI gained one source type and nothing else: an event appears as a
citation in an answer, with no page to open. Creating one through the UI means
the API or an approved proposal.

## 51. Times are UTC, like every other date here

Item 35 applies unchanged: "tomorrow at 3pm" is 15:00 UTC, the summary says UTC,
and the profile's timezone is still read by nothing. For a calendar this is more
visible than it was for a deadline, and it is the first thing a timezone phase
would fix.

# Phase 9 decisions

## Money is an integer, everywhere, at every layer

`Amount` is an `int64` of hundredths. It is never a `float64` — not in the
service, not in the repository, not on the wire, and not in the SQL driver.

This is the one decision in the module with no trade-off to weigh. A `float64`
cannot represent `0.1`, so a column of them does not add up to what a person
adding the same numbers on paper gets; and the entire point of the summary
endpoint is to produce a total somebody will compare against their bank. Ten
lots of `0.10` have to be `1.00`, not `0.9999999999999999`.

So the column is `numeric(12,2)`, which is exact decimal, and the two ends are
joined without a float in between:

- Reading, the SELECT asks for `amount::text` and `ParseAmount` turns the digits
  Postgres holds into the same integer that was written. Asking for the numeric
  directly would let the driver decide, and one of its choices is a float.
- Writing, the INSERT sends `$n::numeric` with the canonical decimal string.
- On the wire, `Amount.MarshalJSON` writes the digits out as an unquoted JSON
  *number* (`450.50`) rather than passing them through a float on the way. A
  client that decodes it into a float of its own has made that choice itself;
  this side does not make it for them.

More than two decimal places is **refused**, not rounded. `12.345` is a value
this column cannot hold, and silently making it `12.35` is an edit to somebody's
ledger that nobody asked for. Rejecting it costs one field error; accepting it
costs a number that is quietly wrong forever.

## There is no total, only a total per currency

Nothing in this phase converts currencies, and the shape of `Summary` is what
enforces that: totals are grouped by currency first, and there is no field
holding a combined figure. `GET /expenses/summary` returns a list of currencies
and no grand total; `analyze_spending` reports them separately and says in words
that they are not added together.

The brief rules out multi-currency conversion, but the reason it is worth
stating as a *shape* rather than as a missing feature is that a combined total
is the kind of wrong that cannot be seen. Adding ₹500 to $20 needs a rate; a
rate has a date; and a stale or wrong one produces a plausible number that
nobody looking at the answer can tell from the truth. A schema that cannot
express the combined figure cannot produce it by accident.

## Unlike the calendar, the range is optional — and inclusive

Two deliberate departures from Phase 8, both because dates are not timestamps
and a ledger is not a diary.

**Optional.** `GET /calendar` requires a window because a calendar grows a row
per meeting and "all my events" is a denial-of-service lever with no use case
(see above). A finance table grows a row per purchase, and "what have I ever
spent on this?" is a real question with a real answer. So `start` and `end` are
optional here and the paging is what bounds the read. What is still refused is a
range that runs *backwards*, which returns nothing whatever is in the table.

**Inclusive.** `calendar.Filter` is half-open; `finance.Filter` is closed on
both ends. `?start=2026-09-01&end=2026-09-30` is September, all of it. These are
`date` values: there is no moment between the last instant of the 30th and the
first of the 1st for an exclusive bound to be more correct about, and an
exclusive `end` would mean a month query had to name the 1st of *October*, which
is not what anybody types. The list and the summary share one `WHERE` clause
(`Filter.conditions`) so the two can never disagree about which expenses a
period holds.

The dates themselves are `date` columns and `time.Time` at midnight UTC, run
through `finance.Day` on the way in and on the way out. An expense has no time
of day, and inventing one would make "this month" depend on which timezone
invented it.

## What the parser refuses, and what it leaves to validation

Two rules in `ParseAmount` that a review found the wrong way round, both about
where a limit belongs.

**A comma is a grouping separator and never a decimal point.** The parser
strips `1,234.50` to `1234.50`, and it used to strip `12,34` to `1234` — a
hundredfold error, silent, and reachable both from `POST /expenses` with
`{"amount": "12,34"}` and from a user saying "I spent 12,34 euros". Half the
world writes the decimal comma, so guessing is not available: a comma not
followed by exactly three digits is now refused, with a message saying to use a
full stop.

**How large one expense may be is validation's rule, not the parser's.** The
parser used to reject more than ten whole digits, matching `numeric(12,2)`. But
`ParseAmount` also reads back the *sums* the summary query produces, and two
expenses of `MaxAmount` add up to eleven digits — so a user with two very large
expenses got a 500 from `GET /expenses/summary`, permanently. The parser's only
limit is now what an `int64` of hundredths can hold, which `strconv.ParseInt`
reports; `MaxAmount` stays in `ValidateCreate`, where a single row's size is
actually decided. A leading zero stopped being an overflow as a side effect.

## Dates in this module are read backwards

`calendarDate` resolves a year-less "5 September" to the *next* one, because it
was written for deadlines and a deadline is ahead of you. An expense is behind
you, and the same parser serving both directions produced two bugs.

A bare calendar date is now pulled back a year when it lands in the future, for
`create_expense` and for the start of a period. The rule is narrow on purpose:
only a phrase that names a month with no year is moved, so "tomorrow" stays
where the user put it.

The end of a *range* is not read backwards — it is anchored to the start, in
the earliest year that does not put it before it. "1 September to 30
September", asked on the 10th, is this September; reading the end backwards on
its own would pull it to last year and invert the range. An end with no start
is left where the phrase puts it, because "everything up to 30 September" read
backwards would hide this year's spending, while read forwards it simply
includes everything.

And whatever survives all that, `create_expense` refuses a date in the future.
Money cannot have been spent on a day that has not happened, and an expense
dated forwards falls outside every period anybody asks about. The HTTP API does
not refuse it — there a date is something the caller stated, not something a
model inferred.

`AddDate` also had to be replaced for months: it normalizes an overflowing
day-of-month *forward*, so the 31st of March minus one month is the 3rd of
March. "How much did I spend in the past month", asked on the 31st, silently
left out the first three days of the period it named — and a total wrong by a
few days does not look wrong. `MonthsBefore` clamps to the end of the month it
lands in instead.

## The default categories are seeded by a trigger on `users`

The brief asked whether this should be a migration-time seed or lazy creation on
first use. It is neither: migration 000009 puts an `AFTER INSERT` trigger on
`users` that inserts the five defaults, plus a one-off backfill for the accounts
that already exist.

A migration-time `INSERT` alone is wrong: it covers the users who exist the
moment it runs and nobody who registers afterwards, so it would have to be
paired with application code regardless.

Putting it in that application code — `users.Repository.Create`, which already
inserts the user and its profile in one transaction — would make the `users`
package depend on the finance schema, and would fire only on the one path that
remembered to call it. The trigger makes "every user has the default categories"
true of the table whatever creates a user: the API, a `psql` session, a test
fixture, a future admin import. It is the argument `drop_knowledge_node` makes
in 000006, applied to an insert instead of a delete.

Lazy creation on first use was the other candidate, and it is worse for a reason
that is not about correctness. An empty category list gives a new user a "pick a
category" control with nothing in it, and gives `create_expense` nothing to
resolve "food" against — so the very first expense anybody logs is filed under
nothing, which is exactly the case the categories exist to avoid.

## Category names are unique case-insensitively

`UNIQUE (user_id, lower(name))`, not `UNIQUE (user_id, name)`.

This was found by a test rather than designed: with a plain unique constraint,
`POST /expense-categories {"name": "food"}` succeeded for a user who already had
`Food`. The consequence is not cosmetic. `CategoryByName` resolves
case-insensitively — it has to, because the model writes whatever the user
typed — so two rows make "log it under food" ambiguous, and a spending breakdown
reports one category as two lines that each look like the whole of it.

The name is still stored as the user spelled it; only the comparison ignores
case. It is the call migration 000001 makes for `users.email`, for the same
reason.

## `analyze_spending` is a tool that returns a computation, not records

Every other read tool answers with rows: `search_tasks` returns tasks,
`search_calendar` returns events, and the orchestrator renders each one as a
source. `analyze_spending` returns a `tools.Report` — a title and a block of
text carrying figures that are already worked out — and the chat layer renders
it as a source of type `spending`.

Two reasons, and the second is the important one.

The cheap one is context budget: "how much did I spend last year" over a
thousand rows is one `GROUP BY` and a handful of lines, or a thousand rows in
front of a model with an 8k window.

The real one is that a 3B model asked to add up forty amounts will produce a
number, and it will sometimes be wrong. There is exactly one right answer to
"how much did I spend on food this month", the user can check it against their
bank, and a plausible wrong total is the worst output this system can produce.
So the arithmetic happens in Postgres, the words describing it are written by
the tool that did the arithmetic — not by the orchestrator, because a second
place that formats totals is a second place they can be formatted wrongly — and
the system prompt tells the model the figures are already computed and must not
be re-totalled or converted.

The `Source` for a report carries the zero uuid: it is a total, not a row, and
there is nothing for a client to open.

## `create_expense` refuses an unknown category; the reads absorb it

The same argument — a category name the user's list does not have — gets
opposite treatment in the write and in the two reads, on purpose.

`search_expenses` and `analyze_spending` fold an unmatched name into the text
search. "What did I spend on coffee?" then answers from the descriptions instead
of coming back empty because there is no `Coffee` category, and the summary says
what it actually did. A read that answered the wrong question can be run again.

`create_expense` declines, and the `ArgumentError` names the categories that do
exist so the model can ask a precise question. Filing money under a label the
user never made is not recoverable by reading it again: it is wrong in the
breakdown, in the summary and in every answer built on them, from then on. A
tool that quietly added "Coffe" because a model spelled it that way would make
the category list a dumping ground for typos and the analysis built on it
meaningless.

This is also why `tools.FinanceService` has `Categories` and `CategoryByName`
and no `CreateCategory`: no tool can add a category, which makes "the category
list is the user's" structural rather than a rule.

## The proposal stores the category by name, not by id

`createExpenseInput` carries `category: "Food"` and no `category_id`, and the id
is resolved again when the approval runs.

The stored input is what the user is shown before approving, and a uuid is not
something anybody can check. It is also the whole of what the tool's declared
params allow — a test pins that a canonical input never carries a key the schema
does not declare — and a resolved id would be exactly such a key.

Re-resolving is safe here because nothing this phase offers renames or deletes a
category. If the name does not resolve at approval time the write *fails* and is
recorded as failed, rather than quietly filing the money under nothing.

## The financial-advice boundary is a prompt rule, in every prompt

The master spec requires that spending analysis be framed as informational
rather than as professional financial advice. That is rule 10 of the chat system
prompt, and three things about how it is written were deliberate.

**It is in every prompt, not only the ones a finance tool ran for.** The
half about sources needs a source to apply to; the half about advice does not.
"Should I move my savings into an index fund?" retrieves nothing at all, and it
is precisely the question where a model with no rule in front of it answers
confidently. Gating the rule on a finance tool having run would remove it from
the turn that needs it most.

**It names the sentences, not the principle.** "Do not give financial advice" is
a rule a 3B model agrees with and then breaks; a list of what not to say —
invest, save, borrow, buy, sell, this category is too high, you should spend
less — is one it follows. It also forbids claiming professional or regulatory
standing outright, because "as your financial adviser" is a sentence a model
will write to sound helpful.

**It says what the assistant may do instead.** Describing what the user's own
numbers show is not advice, and is stated as the approved alternative. A rule
that only forbade would push the model into refusing to answer questions it can
answer perfectly well — which is its own failure, just a quieter one.

The rule is in the prompt whether or not the model obeys it, and that is what a
test can check: `internal/chat/finance_test.go` asserts the framing reaches the
answering model, for a turn that ran `analyze_spending` and for a turn that
retrieved nothing. What the model then writes is checked by `scripts/e2e.sh`,
which warns rather than fails — the same treatment `claims_done` gets, for the
same reason.

## The currency is grounded in the message; the rest of the proposal is not

The hardening pass's rule was that a *filter* argument has to come from the
user's message, because a made-up filter is the one argument that is silently
wrong: the search runs, finds nothing, and the user is told truthfully that
nothing matched. A write's arguments were deliberately not checked — they are
shown to the user in the proposal before anything happens.

Phase 9 found the exception, and found it in the end-to-end run rather than by
reasoning. Asked "I spent 1450.50 on printer cartridges today, log it",
llama3.2:3b called `create_expense` with `currency: "USD"` — a message with no
currency in it at all. The proposal then read "Record an expense of USD
1450.50", the user approves it, and ₹1450.50 is filed as $1450.50: a row that is
never added to their rupee total, because nothing here converts currencies.

The reason a currency is not like a title is that the user cannot check it. A
title is their own words echoed back — if it is wrong they see that it is wrong.
"USD" is a fact the assistant supplied, and it reads on the proposal exactly
like one they supplied themselves.

So `tools.Param` gained `Grounded`, which is `Filter`'s rule without `Filter`'s
"this narrows a read" meaning, and `currency` is the one write argument that
carries it. The check accepts the code, the word (`dollars`, `rupees`) or the
symbol (`$`, `₹`) — a symbol through a substring test rather than the
word-boundary one, since `$20` has no boundary between the two — and drops
anything else, leaving the column default, which the proposal then shows.

The amount is not checked, and does not need to be: `parseMoney` reads a
currency out of the amount string (`"$20"`, `"20 dollars"`) and that string is
the user's own figure copied over. Nor is anything else about the write: the
rule is for arguments the user cannot audit by reading, and there is exactly one
of those here.

## An expense node is labelled by what it was for, never "Expense"

Expenses are mirrored into the knowledge graph like tasks, goals, notes,
documents and events. Categories are not: a category is a label on other rows
rather than a thing that happened, and five category nodes per account would be
five nodes the mention scan fires on whenever somebody says "food".

An expense has no title, so `NodeLabel` builds one: the description when there
is one, and otherwise the category and the date ("Food expense on 2026-09-17").
It is never the bare word "Expense", and that is the whole reason it is a
function rather than a column. The graph's mention scan matches a node when its
label occurs in the message, so a node labelled "Expense" would fire on every
question containing the word — the opposite of "the question named something the
user has".

## The date parser learned the past

`ParseDate` gained "yesterday", "the day before yesterday" and "N days ago";
`ParseWindow` gained "last month", "last year" and "the last N days".

Nothing before Phase 9 needed them. A deadline is ahead of you and a calendar
mostly is, so every relative phrase the parser knew pointed forwards. A ledger
is read backwards — "I paid the rent yesterday", "how much did I spend last
month" — and a parser that could only go forwards would have made the ordinary
way of mentioning an expense unreadable.

# Phase 9 — explicitly deferred

## 52. No budgets, and nothing tracked against one

The spec lists budget recommendations as a capability. This phase records
spending and adds it up; there is no budget table, no target, no "you are over
by ₹2,000", and no forecast. Tracking against a budget needs a budget to exist,
a period to attach it to, and a decision about what happens when one is edited
halfway through — and every one of those is a phase's worth of edge cases.

The prompt rule forbids the assistant from inventing one in the meantime: no
budgets or targets, and no telling the user a category is too high.

## 53. No currency conversion

See above. Amounts are stored in the currency they were incurred in and never
converted, there is no rate table and no exchange-rate fetch, and a total is
always per currency.

## 54. No receipt parsing

`related_document_id` links an expense to a document already uploaded, and that
is all it does. Nothing reads the file, nothing extracts an amount or a date or
a merchant from it, and uploading a receipt does not create an expense. Receipt
OCR is a pipeline of its own.

## 55. No `update_expense` and no delete tool

`create_expense` is the only finance write the assistant has, on the same terms
as `create_calendar_event` (item 47) and for the same reason. A number the
assistant recorded wrongly is corrected through `PATCH /expenses/{id}` or the
UI, not by asking it again — and changing a recorded amount through a
conversation needs the same "show the user exactly what would change" that
deletion is still waiting for (item 29).

## 56. Categories cannot be renamed or deleted through the API

`GET` and `POST` only. Renaming a category relabels every expense filed under
it, and deleting one un-files them all through `ON DELETE SET NULL`; both are
edits to spending history made through a door marked "categories". The foreign
key and the cascade are in place, so a later phase that can show the user what
would change can add both endpoints without a migration.

## 57. Expenses are not background context

The chat turn retrieves recent tasks, goals, notes and the next two days of the
calendar as background on every substantial message. It does not retrieve
recent expenses. An expense is not about *now* the way a pending task or
tomorrow's meeting is, and a list of recent purchases in front of every answer
would spend the context budget on something the question is almost never about.
A question about spending routes to a tool, which is what tools are for.

## 58. `SPENT_ON` is still not a relationship

Migration 000006 left `SPENT_ON` out of the relationship allow-list because it
pointed at expenses and expenses did not exist. They do now, and it is still
out: adding it means widening a CHECK constraint *and* teaching the extraction
prompt when to use it, which is a change to how the model reads every turn
rather than a new table. An expense node can still be linked with `RELATED_TO`.

## 59. Finance has no screen

The web UI gained nothing this phase. An expense reaches the user through the
API, through an approved proposal, or as a citation in an answer;
`GET /expenses/summary` exists in the shape a dashboard would want, and is not
drawn by one yet.

# Phase 10a decisions

## A generated flashcard is checked against the document, not trusted to the prompt

This is the phase. Everything else in it is a CRUD resource.

The generation prompt says, three times, that every answer must be stated in
the passages. A 3B model does not always comply — the measured Phase 5 failure
was llama3.2:3b answering from its own knowledge when the retrieved text was
thin, confidently and in exactly the register of a real answer. So the prompt
is the request and `internal/study/grounding.go` is the guarantee: after
parsing, every proposed card is compared against the same passages the model
was shown, and one whose answer is not in them is dropped before the user ever
sees it.

The rule, precisely:

- the **back** must be `GroundedIn` the passages: at least two thirds of its
  content words have to appear in them;
- a **figure** on the back that the passages do not contain sinks the card
  outright, whatever the share works out to;
- the **front** only has to `MentionsAny` — name at least one thing the
  passages talk about.

The two sides differ because they do different jobs. The back is the claim; it
is what the learner ends up believing. The front is mostly the words of asking,
which say nothing about the source — "What did the aurora do, and for how
long?" is four content words of which one is the document's, and holding a
question to the answer's share would reject a model for writing English. What
the weaker test still catches is the card that is about something else
entirely: a question about photosynthesis with an answer stitched out of a
networking paper.

Two thirds rather than all of it, because an answer written as a sentence
carries some of the model's own glue. The number is a judgement, and the thing
it trades off is stated below.

## Why a number is exact and a word is not

`sameWord` allows an inflection — "last" matches "lasted", "filter" matches
"filters" — because a restatement introduces them and rejecting one would be
rejecting English. A token with a digit in it has to match exactly, and an
unsupported one disqualifies the answer rather than counting against a share.

Both halves of that come from the same case. "400 volts after 12 seconds",
against a passage that says 40 volts after 12 seconds, is three quarters the
document's words and entirely wrong; and "40" is a prefix of "400", so the
inflection rule would have matched it. A figure is the part of a flashcard a
learner is least able to check and most likely to memorise, so it gets the
strict rule and the veto.

The cost is stated rather than hidden: a card answering "40 minutes" from a
passage that says "forty minutes" is dropped. That is the right direction.

## The grounding check is the inverse of the memory one

`memories.RestatesRetrieved` throws away a "fact about the user" that turned out
to be the content of a retrieved document. `study.CardGrounded` throws away a
flashcard whose answer is *not* the content of one. Same machinery — content
words, stop words, prefix matching — opposite sign, because the two callers want
opposite things out of the same model.

They are separate files rather than a shared package, and that is deliberate.
The stop-word lists differ: study's drops the vocabulary of asking ("define",
"explain", "according", "question") so a card is judged on its subject rather
than on the asking, and memory's drops the vocabulary of attribution ("knows",
"read", "mentioned") for the mirror-image reason. Study keeps short tokens with
digits in them; memory has no use for them. A shared implementation would be one
whose parameters had to be tuned for two jobs that pull in opposite directions.

## Generation happens when the call is prepared, not when it is approved

`generate_flashcards` writes its cards during `Registry.Prepare` — which may
read and never writes — stores them in the canonical input, and writes exactly
those rows on approval. `study.Service` offers no method that generates and
stores in one step, so a write tool cannot regenerate even by mistake.

The alternative was to store the document and the count and generate on
approval. That would mean the user approved "8 flashcards from lecture-3.pdf"
and got eight sentences nobody had read — for content whose entire purpose is
to be rehearsed until it is believed, the wrong way round. It also makes the
proposal reproducible: the same stored input always writes the same cards, so
an approval is not a second roll of the dice, and a re-read of the action log
shows what was actually agreed to.

What it costs is that a pending proposal could go stale if its document
changed. It cannot: a document cannot be edited in this version, only deleted,
and a deleted one is caught when the approval runs.

## The approval card shows the cards, so `Param` grew a `Derived` flag

The canonical input for `generate_flashcards` carries a `cards` key the model
never wrote. That collided with an invariant the tool registry has had since
Phase 7 and a test that pins it: every key in a stored input is a declared
parameter, so what the user is shown, what the schema promises and what runs
are one document.

Rather than weaken the invariant, `Param` gained `Derived`. A derived parameter
*is* declared — so the stored input is still exactly the declaration — and is
left out of `Tool.InputSchema` and of the routing prompt, so the model is never
offered it and never asked to invent one. It is the only one in the codebase,
and `registry_test.go` also pins that a derived parameter is never `Filter` or
`Grounded`: the grounding check is about values a model read out of a message,
and this is a value no model wrote.

## The prompt's worked example is about bread

The generation prompt needs a worked example — without one, llama3.2:3b writes
a prose summary and no cards, the same finding Phase 5 recorded for extraction.
But an example inside the prompt is text the model can copy, and a copied card
would sail through the grounding check if the example's subject overlapped the
document being studied. The check would then be measuring nothing.

So the example is about sourdough, it is pulled out into its own constants, and
`TestTheWorkedExampleIsAboutSomethingElse` fails if it ever shares a content
word with the documents the tests and `scripts/e2e.sh` upload.

## A topic that matches nothing is refused, not silently widened

`generate_flashcards` takes an optional `topic`, which narrows generation to the
passages a vector search over that one document returns. When the search returns
nothing the call fails with `ErrNoPassages` and the assistant asks which part of
the document the user means.

Falling back to the start of the document was the obvious alternative and is
worse in the way this codebase cares about: the user asked for cards about
chapter nine, would have got cards about chapter one, and would have had no way
to see that it had happened. It is the same rule the hardening pass applied to
invented filters — a read that answers a question nobody asked is worse than
one that says it cannot.

The search runs with **no** similarity floor, unlike chat retrieval. The floor
exists to stop an unrelated question citing a distant chunk; here the search is
already restricted to one document the user named by name, so there is nothing
for it to protect against, and applying it would make "cards about the appendix"
fail on a document whose appendix is not worded like the word "appendix".

## A study plan is a `project` node, and no eighth node type was added

`study_plans` joins the mirrored tables, and its node type is `project` —
introduced by migration 000006 for "a piece of work being worked towards",
which is what a study plan is.

`skill` was the other candidate and is wrong: a plan is not the subject it is
about, and a node labelled "Linear algebra" standing for a plan would collide
with the `skill` node an extraction writes for the subject itself. A new
`study_plan` type would be a new kind of node for something the graph already
has a word for.

The consequence is worth naming, because it is the first time it is true:
`project` is now both a mirrored type and an extracted one. So "may this node be
deleted directly" is `Node.Extracted` — does it have a `ref_table` — and never
the type. `DeleteNode` already asked it that way; `graph.ExtractedTypes` is now
documented as "what a conversation can create", which is not the same list, and
the HTTP `409` is asserted for a mirrored `project` in both
`internal/db/phase10_integration_test.go` and `internal/api/study_isolation_test.go`.

## The study rule is in every prompt, and it is about what is *not* there

Rule 9 is the study rule; the money rule moved to 10 and the action rule to 11.

Most of the source rules explain what a source carries. This one mostly
explains what it does not: a `study_plan` source names the plan, its status,
its document and *how many* flashcards are filed under it, and says nothing
about any of them.

That is the shape that invites an invention. Rule 3 already forbids claiming
something is in the user's data when it is not, but here the thing being
invented is *known to exist* — the context says "24 flashcards" — and a model
asked "what is on my linear algebra cards" will produce three of them. So the
rule names it: never state, quote, summarise or guess what a card says, and
tell the user to open the plan instead.

It is in every prompt rather than only the ones a study tool ran for, for the
reason the finance rule is: "read me my flashcards" from a user with no
matching plan retrieves nothing at all, and that is exactly the turn where a
model with no rule in front of it invents a deck.

The second half is subtler. A saved card's answer *is* the document's — the
grounding check made sure of it — so the assistant must not go further and
vouch for it as fact, or quietly correct one it disagrees with. The card is the
user's material, not the assistant's opinion.

## Flashcards get no node

A deck is hundreds of rows of one or two sentences. A node per card would swamp
the graph, and the mention scan — which matches a node whose label occurs in the
user's message — would fire on any question sharing a word with an answer. The
plan is the thing worth connecting, and the cards hang off it. It is the same
call Phase 9 made for expense categories.

## The study module depends on `internal/documents`, plainly

`study.Library` is declared over `documents.Document`, `documents.Passage` and
`documents.SearchQuery`, rather than over strings the way `NodeSyncer` is.

What study needs from documents is the *text of a document*, and there is no way
to say that in this package's own vocabulary that would not be
`documents.Passage` with a different name on it. "Document-grounded flashcards"
is the feature, so the dependency is the phase; laundering it through a
translation layer would hide it rather than remove it. The ownership *probe* on
`document_id` still reads the table directly from this module's repository, the
same considered exception `calendar` and `finance` make.

## `documents.Passages` is a new read, and it is not `Search`

Generating from a document with no topic needs its text, in order, with nothing
to rank against. `Service.Search` cannot answer that: it embeds a query and
sorts by distance.

So `documents` gained `Passages(ctx, userID, documentID, limit)` — one indexed
scan of `document_chunks`, owner-scoped on the chunk as well as the document. It
returns a new `documents.Passage` rather than a `SearchResult`, because a
`SearchResult` with `Similarity: 0` reads exactly like "no resemblance at all"
when what is meant is "nothing measured it".

`PromptPassages` bounds what goes into one prompt — 8 chunks, 6,000 characters
— and the *same* function's output is what the grounding check runs against. The
text a card is checked against is exactly the text the model was shown; checking
against the whole document would only ever mean the check was measuring
something else.

## A hand-written card claims no source

`POST /study-plans/{id}/flashcards` writes a card with `document_id: null`, even
when the plan it is filed under names a document. Borrowing the plan's document
would assert a provenance the card does not have, and that column exists for
exactly one purpose: to say "this answer came from there".

That endpoint also takes no approval. Approval stands between the assistant and
the user's data, not between the user and their own.

## A deck reads back in the order it was written, which needed `clock_timestamp()`

A batch of cards is one transaction, `now()` is the transaction's start time,
and `ORDER BY created_at, id` then falls to a tie-break on a random uuid. The
first end-to-end run produced eight correct cards in a shuffled order — and
nothing about the API would have said so, because every card was right.

`CreateFlashcards` stamps each row with `clock_timestamp()` instead, which is
the call `chat.Repository` already makes for the two messages of a turn and for
the same reason. `TestADeckReadsBackInTheOrderItWasWritten` inserts twelve
cards and fails without it.

It matters because the order is the document's: a generated deck walks the
passages front to back, so reading it in order is reading the source in order.

## `CHAT_TIMEOUT` defaults to twenty-four minutes now

A flashcard generation is a model call on the path to the *proposal*, so it
happens before the first token, like the routing call, and out of the same turn
budget. `config.Load` counts it in the `MaxExtractionShare` check, and the
default turn budget grew by the same 180s × 2 that every previous model call
added: 4 × 180s × 2 = 24 minutes.

There is a second-order effect worth recording because it is not obvious and it
is what actually broke the first end-to-end run: the routing prompt renders
every offered tool, and this phase added three. At sixteen tools the prompt is
about 8,300 characters, and one routing decision on a 2019 Intel Mac was
measured at **3m08s** — over the 180s default, so the turn failed as
`model_unavailable` before anything about study ran at all. The decision itself
was correct. `docs/testing.md` records the budgets a slow machine needs.

# Phase 10a — explicitly deferred

## 60. No quizzes

10b. There is no quiz table, no question type beyond front/back, no scoring and
no multiple choice.

## 61. No review state on a card

A flashcard has a front, a back and a source. There is no `correct`/`incorrect`
counter, no last-reviewed timestamp, no ease factor, no interval and no due
date, and nothing in this phase records that a card was ever looked at. Weak-
topic tracking is 10c and spaced repetition is 10d; a column added now would
either sit unread for three phases or be the wrong column when they arrive.

The API has no "I got this right" endpoint for the same reason — it is the
interaction that *produces* review state, and building it before the state it
feeds would be building half a feature twice.

## 62. No study sessions and no streaks

10e. Nothing records when a study session started or ended, and nothing counts
consecutive days.

## 63. No `update_flashcard`, no delete tools, no `update_study_plan` tool

The assistant can propose a plan and propose cards, and that is all. There is
no tool that edits or removes either, on the same terms as
`create_calendar_event` (item 47) and `create_expense` (item 55): a card the
assistant wrote wrongly is deleted through `DELETE /flashcards/{id}` or the UI,
not by asking it again. `tools.StudyService` has no method for it, so no tool
can reach one.

## 64. A card cannot be edited at all, by anyone

There is no `PATCH /flashcards/{id}`. There is nothing to change on a card that
is not "write a different card", and adding the endpoint would mean deciding
what happens to `document_id` when a user rewrites an answer the document no
longer supports — which is the grounding question again, in a place with no
document in front of it.

## 65. No cap on cards per plan

A batch is capped at 20 and a generation defaults to 8, but nothing bounds how
many cards a plan accumulates over time. `goals` caps milestones at 100; this
does not, because a deck legitimately grows to hundreds and the number at which
it stops being legitimate is not knowable yet. The paging bounds (500) are what
protect a read in the meantime.

## 66. Generation is not exposed as an HTTP endpoint

There is no `POST /study-plans/{id}/flashcards/generate`. Generating costs a
model call and produces content the user has to read before approving, which is
the approval flow's whole job — so it goes through the assistant. A UI that
wants a "generate" button will drive it through a conversation, which is also
where the user will read the proposal.

## 67. No re-generation, and no "make me more like this one"

Asking twice makes two independent proposals. Nothing deduplicates a card
against the deck it is about to join, so approving two similar generations
gives a deck with near-duplicates in it. Deduplicating needs a similarity
measure over cards — an embedding per card, a threshold, a decision about which
of two near-duplicates to keep — and that is the machinery 10d will need
anyway.

## 68. Study has no screen

The web UI gained nothing this phase. A plan reaches the user through the API,
through an approved proposal, or as a citation in an answer.

# Phase 10b decisions

## Only the correct answer is grounded; a distractor is supposed to be wrong

Phase 10a's claim was that a generated flashcard says what the document says,
and the machinery that makes it true is `grounding.go`: the back of a card has
to be `GroundedIn` the passages the model was shown, and the front only has to
`MentionsAny` of them. A quiz question is the same claim with more parts, and
the decision this phase had to make is *which* parts.

`QuestionGrounded` is `CardGrounded` with the correct option standing in for
the back. It is literally the same two calls, and a test pins that a question
and the same question written as a front/back pair are judged identically — so
if the rule changes for one it changes for both, rather than drifting into two
rules that are nearly the same.

The distractors are not checked at all, and that is not an omission. A wrong
answer is *supposed* to be wrong. Requiring it to appear in the document would
mean every option was something the document says, which is the opposite of
what a distractor is for; and dropping a question because its wrong answers are
not in the text would drop every well-made question in the set. There is no
version of the check that helps here, because the property a distractor needs —
being *false* of the document — is not something a bag-of-words comparison can
establish.

What that costs is real and is stated rather than hidden: nothing in this
codebase can tell a good distractor from one that happens to be true of a part
of the document the model was not shown. A quiz can therefore have two right
answers and mark one of them wrong. The mitigations are the prompt, which asks
for wrong answers in so many words and forbids the lazy ones ("none of the
above", "all of the above"), and the approval card, which shows every option of
every question to the person who knows the document. Measuring distractor
quality would need a second model call per question grading its own output,
which is a worse guarantee than a person reading four lines.

## The approval card shows the answer key, because it is the part that cannot be fixed later

`generate_flashcards`' summary renders both sides of every card. `generate_quiz`
renders every question, every option, *and* which option will be marked
correct.

The last of those is the one worth arguing for, because it is what makes the
summary long. A wrong flashcard teaches the user something false, which is bad
and is what the grounding check exists for. A wrong answer *key* does something
the flashcard cannot: it marks the user wrong for knowing better. That is the
one error here they cannot correct by reading more carefully afterwards —
there is no `PATCH` on a quiz, so a bad key means deleting the quiz and their
attempts with it. So it goes in front of them before it exists.

## The options are never shuffled, anywhere

Not in the parser, not on approval, not on read. The order the model wrote is
the order stored and the order shown.

The reason is the same one the cards-in-the-proposal decision rests on: an
approval has to execute exactly what was approved. A quiz that rearranged
itself between the approval card and the database would be a quiz the user did
not read, and `correct_index` is a position — shuffling means rewriting the
answer key, which is precisely the value that must not be rewritten by anything
the user cannot see.

There is a real cost: a model that puts the right answer first every time
produces a quiz that can be passed without reading it. Shuffling belongs in the
client that renders the quiz, which can do it per attempt without touching the
stored row, and the prompt's worked example varies the position to discourage
the habit at the source.

## `correct_index` is a position and `answer` is a value

`ParseQuizQuestions` accepts the answer as an index, as a letter and as the
option's own text, because a small model writes all three about equally often.
Resolving them has one genuinely wrong outcome available, and avoiding it is
why the fields are split into two kinds.

A quiz whose options are themselves numbers — "2", "4", "6", "8" — is where the
difference bites. `correct_index: 2` means the third option; reading it as the
text "2" marks the first. So `answer`, `correct`, and `correct_answer` name the
answer *by value* and are matched against the option text first; `correct_index`
and `answer_index` name it *by position* and are never text-matched. A letter is
tried from either kind, because "B" can only mean one thing. A JSON *number* in
`answer` is read as a position, which is the one remaining ambiguity and is
resolved the way a model that was asked for `correct_index` most likely meant
it.

Within the value fields, the written-out answer beats an index that disagrees
with it: a model writes the option's text correctly far more often than it
counts the options. And an index that points at no option is not salvaged by
assuming the model counted from one. That assumption is right about as often as
it is wrong, and being wrong produces a quiz that marks the right answer wrong —
so the question is dropped instead, and the user gets five questions rather than
six.

## `GET /quizzes/{id}` does not return the answer key

The quiz reads back with its questions and their options and no `correct_index`.
The answer comes back when the question is answered, and a finished attempt
reads back with every correct index on it.

This is the one place in the API where a user is not handed their own data on
request, so it is worth being plain about what it is and is not. It is not
secrecy — nothing here is hidden from them, and two requests get the whole key.
It is that a quiz's only purpose is to be answered without the answer in view,
and an endpoint that returns the key is one that every client will leak by
accident: into a devtools panel, a debug log, a cached JSON blob, a
`console.log` left in. Withholding it in the default read makes the careless
thing the correct thing.

The cost is that reviewing a quiz you have not taken means taking it. That is
acceptable because the alternative — a `?with_answers=true` — is a flag whose
only user would be the client that should not be reading them.

## Taking a quiz is not a tool, and could not be

`tools.StudyService` gains `Quizzes`, `ProposeQuiz` and `CreateQuiz`. It does
*not* gain `StartAttempt`, `SubmitAnswer` or `CompleteAttempt`, so there is no
tool that can take a quiz, answer a question or finish an attempt — not because
a tool declines to, but because the interface offers no way, which is the same
guarantee that keeps a delete tool out (item 33).

The distinction is the one the whole approval design rests on and this is the
clearest case of it yet. Approval stands between the *assistant* and the user's
data. An attempt is the user answering their own questions: there is nothing
for them to be protected from, and an approval card reading "record that you
chose option B" would be a dialog box between a person and their own click.

What the rule has to stop is the other direction. A 3B model asked "quiz me"
will happily improvise a quiz in the chat, ask the questions itself and mark
the answers, producing a score that nothing recorded and that does not exist.
So the system prompt says, in the same rule that withholds the questions, that
the assistant cannot start, answer or finish an attempt and that taking a quiz
happens in the app. The sentence is true by construction; it is in the prompt
because the model cannot see the interface.

## A "quiz" source carries counts and never content

A `study_plan` source tells the model how many flashcards a plan has and
nothing about any of them, for a context-budget reason: a deck is hundreds of
one-line answers. A `quiz` source withholds the questions for a reason that is
not about budget at all.

A quiz exists to be answered without the answer in view. A model that could
recite a question would recite the answer with it — helpfully, in the same turn,
because the user asked — and there would be no quiz left. So `quizSummary`
renders the size and the outcome and nothing else, `quizRecord` carries no
questions and no options, and `generate_quiz`'s recorded *result* carries the
question text but not the options or the key, because a result is read back
into later prompts.

The prompt rule then forbids stating, quoting, summarising or guessing what a
quiz asks **even when asked directly**. That last clause is the one case rule 3
does not already cover: rule 3 forbids inventing, and here the thing would not
be invented — it would be real, and spoiled.

The score is written out against the total in the excerpt ("best 4 of 6")
rather than as two fields, for the reason the calendar writes its times out: a
model handed "4" and "6" separately will sometimes report a percentage it
worked out itself.

## One answer per question per attempt, enforced by the schema

`quiz_answers` has a unique index on `(attempt_id, question_id)`, and the
service also checks before it writes.

The service check is there because it has a better error to give — a 409 naming
the question rather than a constraint violation — and the index is there
because the check is a race. Two submissions of the same question can both pass
a `SELECT` and both insert, and the attempt is then scored out of more answers
than the quiz has questions. A check tells you what went wrong; a constraint
decides what is possible.

The second answer is a *conflict* rather than an overwrite, which is the
substantive half of the decision. An attempt is a record of what the user
answered. Letting the second submission win would make the score a record of
what they answered last, having already been told whether the first was right —
which is not a score. Retaking the quiz is a new attempt, and a new attempt is
a new row.

## The score is computed by the statement that closes the attempt

`CompleteAttempt` sets `completed_at` and `score` in one `UPDATE`, with the
score as a `count(*)` over the attempt's own answers, and `completed_at IS
NULL` in the `WHERE`.

Three things fall out of that shape. There is no window in which an attempt is
complete and unscored, so no read can see a half-finished state. The number
cannot disagree with the rows it came from, because it is derived from them in
the same statement. And completion is once-only for the same structural reason
an approval is: a second call matches no row, which is what `ErrAttemptComplete`
reports.

Unanswered questions are not counted wrong. They are not counted at all — the
score is how many were right — and the attempt carries the quiz's
`question_count` beside it so "4" is read as "4 of 6". An attempt can be
completed with nothing answered, because a user who stopped halfway should not
be left with a row that can never be anything else.

## A quiz gets no knowledge-graph node

Migration 000011 touches neither the `ref_table` allow-list nor the trigger
list, and `anchorFor` returns nil for a created quiz.

It follows the rule 000010 set for flashcards, which is that the *plan* is the
thing worth connecting and the material hangs off it. A node per quiz would put
a second label for the same subject into the graph — the quiz is titled after
the document it came from — and the chat's mention scan would then fire twice
on one word. An attempt is a worse candidate still: it is an event rather than
a thing the user has, and there would be one every time they sat down.

The test that pins this asserts both halves: no node points at the quiz, *and*
the plan still has its own — so it cannot pass by the graph simply being empty.

## The three counts on a quiz are computed, and `best_score` is not 10c

`question_count`, `attempt_count` and `best_score` are correlated subqueries,
not columns, for the reason `study_plans.card_count` is one: a counter kept in
a column is a counter that can be wrong.

`best_score` deserves a sentence on its own, because 10b is explicitly not
supposed to analyse anything. It is a `max` over one quiz's finished attempts
and nothing else. The thing 10c will build is *per-topic* and *across* quizzes
— "you keep getting the battery-bank questions wrong" — and needs the `topic`
column on every question and the `correct` column on every answer, both of
which this phase records and neither of which anything here reads. A quiz list
with no indication of whether you have taken it is a list nobody can use; that
is what this is for.

## `topic` is the model's word or nothing

Every question carries an optional `topic`, which nothing in this phase reads.
It is here because 10c reads it, and a tag recorded when the question was
written is better evidence than one derived later from its wording.

A question the model gave no topic stores `NULL` rather than getting one
derived from its text. A derived tag would group by phrasing — "how long does
the changeover take" and "what is the changeover time" would be two topics —
which is exactly the failure 10c's aggregation would then be built on. An
absent tag is honest and skippable; a wrong one is neither.

## `clock_timestamp()` again, on both new ordered tables

The Phase 10a ordering bug was `now()` on a batch insert: it is the
transaction's start time, so every flashcard in one generation got the same
stamp and the read order fell to the tie-break on a random uuid — which
shuffled a deck out of the order of the document it came from.

`quiz_questions` is the same insert in the same shape, and it matters more: a
shuffled deck is annoying, and a shuffled quiz means the approval card and the
stored quiz are in different orders, which is the one property the whole
generated-at-prepare-time design exists to keep. So the column defaults to
`clock_timestamp()` *and* `CreateQuiz` names it explicitly, so the ordering does
not depend on a default somebody could change.

`quiz_answers` gets it too, although answers are inserted one per request today
and `now()` would do. That is the point: "answers are never batched" is exactly
the kind of assumption that was true of flashcards until it wasn't.

## `generationSource` and `generate` are shared, not duplicated

The flashcard and quiz generations do the same six things before they differ:
check the model is wired, resolve the document, refuse one with no indexed
text, take the topic's passages or the document's first few, bound them to what
fits in a prompt, and refuse a topic that matches nothing.

That is now one function, called by both. It is not a tidiness argument. The
two have to agree about what "the document says" means, because the grounding
check runs against exactly the text the model was shown — and a quiz and a deck
made from the same request being grounded in different passages would make both
claims weaker than they read. The model call is shared for a smaller version of
the same reason: the options are the argument (a bigger model, near-zero
temperature, JSON mode off), and two copies of them are two things to keep in
step with the configuration.

## A figure written in words is a figure: the hole the digit rule left

`grounding.go` has always said that an unsupported *number* sinks an answer
outright, whatever the two-thirds share works out to, because a figure is the
part of a card a learner is least able to check and most likely to memorise.
It implemented that by looking for a digit in the token.

Measured while writing this phase, against the handbook the end-to-end check
uploads — which says "the whole bank is equalised every forty days" —
llama3.2:3b wrote:

> How often is the whole battery bank equalised?
> Every five days / Every forty days / **Every sixty days** *(correct)* / Every ninety days

The right answer was sitting in the same list, and the model marked the wrong
one. `GroundedIn("Every sixty days")` returned **true**: `every` and `days` are
both in the passage, `sixty` is not, and two content words out of three clears
`MinGroundedShare`. The rule that exists for exactly this never fired, because
"sixty" contains no digit.

So `isFigure` now covers numbers written out — cardinals, ordinals, and the
quantity words ("half", "twice", "dozen") that are figures in everything but
spelling — and it is used in both places the digit test was: an unmatched
figure sinks the answer, and a figure matches only exactly. The second half
matters on its own, because `sameWord`'s prefix rule was letting "four" match
"fourteen".

Two things are worth recording about how this was found. It was not found by a
unit test, because every unit test in this module was written by the same
person who wrote the rule and shared its blind spot; it was found by generating
a quiz from a real document with a real model and reading the output. And the
end-to-end script *reproduced it and passed it*, because its Python
re-implementation of the check had been faithful to the Go — including the
hole. Both are fixed, and `scripts/e2e.sh` now carries a note to keep its
number list in step with `numberWords`.

It does not make the check complete, and the very next run showed exactly how.
From the same handbook, which says "The ice shield is retracted before any
equalisation charge":

> What is retracted before the equalisation charge?
> The ice shield / The mast feed / **The dipole** *(correct)* / The bus

The answer key is wrong and the grounding check passed it, correctly: "dipole"
*is* in the document — "the mast feed is switched to the auxiliary dipole". The
check asks whether the answer is drawn from the text, and this one is. It has
never asked whether the answer is right, and a bag-of-words comparison cannot:
"The ice shield" and "The dipole" are each one content word, each present.

That is the honest boundary of the claim, and it is worth stating in the same
breath as the claim itself. **"Every correct answer traces back to the
document" is not "every answer key is right."** What the check buys is that no
answer is invented out of the model's own knowledge. What catches a wrong key
is the approval card, which in that run showed "The dipole (correct)" on the
line above "The ice shield" to somebody who had read the handbook — and that is
why the answer key is in the proposal rather than summarised away.

So the figure rule closes the specific class the code already recognised as
different in kind, and the class that is worst in a quiz: a contradicted figure
does not merely teach something false, it marks the learner wrong for knowing
better. The general class stays open, and the user is the check on it.

## A proposal summary is now content, so it cannot live inside a sentence

`chat.toolStep.actionsBlock` tells the answering model what to say about a
proposal, and `act.go` records the measurement behind its wording: shown "You
proposed this change, and it has NOT been made: ...", llama3.2:3b copied the
section into its reply; shown "You prepared a change that has not been made
yet. Reply by telling the user, in your own words, that you have prepared it,
what it will do (*summary*), and that it will only happen once they press
Approve...", it answered properly in every sample.

That was measured when every summary was one sentence. `generate_flashcards`
made summaries multi-line and `generate_quiz` made them long — a heading, four
option lines per question, and a closing sentence; fifteen lines for three
questions — and interpolating that into the middle of a "what it will do (...)"
clause reproduced the original failure exactly. Measured on llama3.2:3b in the
end-to-end run, the reply began:

> I prepared a change that has not been made yet. Reply by telling the user, in
> your own words, that you have prepared it, what it will do (Create a new
> quiz…

which is the instruction, read out.

So a summary containing a newline now goes *after* the instruction, under a
"What it will do:" heading, and a one-line summary keeps the inline form that
was measured. Both are pinned by tests, the second as much as the first: the
point is not to improve the wording, it is to stop this phase changing what
every other write tool produces.

The same function had a second site, found by looking for it rather than by a
failure. The recap of earlier proposals is a bullet per action, built as
`"- " + summary + ": " + status`, which for a quiz produced:

> - Save a quiz "relay-handbook.txt", 2 questions:
> 1. What is the voltage of the equalisation charge?
>    - 40 volts
>    - 58.4 volts (correct)
> The option marked (correct) is the one the quiz will mark right: the user
> approved it and it was done.

— a sentence saying the marker convention was approved, with the questions
spilling out of the bullet in between, where a second entry could not be told
from the first one's options. It fires on the second turn of any conversation
that proposed a quiz or a deck. The recap now takes the summary's first line,
which is all a recap needs: the content was shown in full when it was proposed,
on the card the user read.

The general shape is the one this phase keeps running into: **a measurement is
made under conditions, and the conditions stop holding when a later phase
changes the inputs.** Nothing failed when summaries became multi-line — the
approval still worked, the row was still correct, and only the sentence the
user read was wrong.

## The routing gate had no word for anything the study module owns

`agents.MightUseTool` is a lexical gate in front of the routing call: no cue
word, no routing call, no tool. Phase 10a added three tools and no cues, and
Phase 10b made the consequence visible.

Measured:

| Message | Reached the router before 10b |
| --- | --- |
| `Make flashcards from relay-handbook.txt …` | yes — on "make" |
| `Quiz me on the relay handbook.` | **no** |
| `What is on my flashcards?` | **no** |
| `Test me on chapter four.` | **no** |

The generation requests slipped through on a verb that happens to be in the
list, which is why nothing noticed. But the most natural way to ask for a quiz
— "quiz me on X" — contained no cue at all, so `generate_quiz` could never have
been chosen for it however good the model was. That is not a routing-quality
problem, it is a tool that is unreachable through its own front door.

The fix is the gate's own idiom: a group of prefixes for the things this module
owns — `quiz`, `flashcard`, `card`, `deck`, `stud`, `revis`, `exam`, `test`,
`practi`, `learn`. `test` and `exam` err long ("test the connection",
"examine"), which is the documented trade: a false positive costs one routing
call and the gate can only ever decline a tool, never choose one.

The general lesson is worth stating because it will recur: **a phase that adds
a tool has to add the words people use to ask for it.** The gate is not
derived from the tool declarations, and nothing fails when it falls behind —
the feature simply does not work for the phrasing nobody tested.

## Eighteen tools, and the routing prompt is now about 9,400 characters

Phase 10a recorded that the routing prompt renders every offered tool, that
sixteen tools made it about 8,300 characters, and that one routing decision on
a 2019 Intel Mac was measured at **3m08s** — over the 180s default, so the turn
failed as `model_unavailable` before anything about study ran.

This phase adds two more. The prompt is **9,387 characters** — that number is
exact, because it is computed from the tool declarations rather than measured.
The time is not, and it is worth being careful about what was and was not
established here.

What was observed while writing this phase: an `E2E_ONLY=quiz` run with
`AGENT_TIMEOUT=420s` — the budget this page and `docs/testing.md` had been
recommending — failed at the first turn with `model_unavailable`, saying
nothing about quizzes; and a standalone routing decision for the same message
did not finish inside fifteen minutes. But that machine was also under a load
average around 250 with Ollama running entirely on CPU (`size_vram: 0`), so
those numbers measure a badly loaded laptop and not the cost of two extra
tools. The honest statement is the direction, not a figure: the prompt grew
about 13%, the recommended budget was already marginal at sixteen tools, and it
is no longer adequate here.

The decision is unchanged and the fix is still configuration rather than code;
`docs/testing.md` now recommends a larger `AGENT_TIMEOUT` with a `CHAT_TIMEOUT`
to match, and says plainly which of its numbers are measurements and which are
headroom. What is worth naming is the shape, because this is the second phase
in two to hit it: **the routing prompt grows with every tool the codebase
gains, so every module makes every turn slower, including turns that have
nothing to do with it.** The named-agent layer `internal/agents` was built for
— one step that picks an agent, then a routing call offered only that agent's
tools — is the structural answer, and it stops being an architectural nicety at
roughly the point this cost crosses a budget somebody has configured. It has
now done that twice.

# Phase 10b — explicitly deferred

## 69. No weak-topic tracking

10c. `quiz_questions.topic` and `quiz_answers.correct` are recorded and nothing
reads them. There is no aggregation across attempts, no "you keep missing the
battery-bank questions", no per-topic score and no endpoint that would return
one. Recording the evidence is not the same as building the analysis, and the
analysis needs decisions this phase has no information for — whether one wrong
answer is a weak topic, how much a right answer three attempts ago counts.

## 70. No spaced repetition and no scheduling

10d. Nothing decides when a quiz should be taken again, and there is no due
date, interval or ease factor on a quiz, a question or an attempt.

## 71. No sessions and no streaks

10e. An attempt records when it started and when it finished, and nothing
groups attempts into a session or counts consecutive days.

## 72. No "retry the ones I got wrong"

A new attempt is a new row at the same quiz, with all of its questions. There
is no attempt that contains a subset, and no quiz generated from another quiz's
wrong answers. Both are 10c's, because both need to know which questions were
wrong *across* attempts, which is the aggregation deferred above.

## 73. A quiz cannot be edited

There is no `PATCH /quizzes/{id}`, no endpoint that adds or removes a question,
and no tool that changes one. A quiz with a question added is a different quiz
from the one somebody has already sat, and the attempts at the old one would
silently be scored against the new total. The fix for a bad quiz is to delete
it — which deletes its attempts, and says so — and generate another.

## 74. No question types but multiple choice

No true/false (which is multiple choice with two options, and can be written as
one), no free text, no matching, no ordering. Free text is the interesting
absence: grading it means deciding whether a sentence means the same as another
sentence, which is the grounding problem again with no document to check
against.

## 75. No difficulty, and no way to ask for one

A generation takes a document, a topic and a count. There is no "make it
harder", no difficulty column and nothing that rates a question. A model's
self-reported difficulty is not a measurement, and the real one — how often
people get it wrong — is 10c's data.

## 76. The distractors are not checked

Stated above as a decision rather than an absence, and repeated here because it
is the honest limit of the phase's claim: "every correct answer is in the
document" is guaranteed, and "every wrong answer is wrong" is asked for in the
prompt and read by the user on the approval card.

## 77. Quizzes have no screen

The web UI gained nothing this phase. A quiz reaches the user through the API,
through an approved proposal, or as a citation in an answer — and taking one
needs a client, which is a UI phase's job.
