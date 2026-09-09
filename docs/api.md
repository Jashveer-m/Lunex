# Lunex API — v1 (Phases 1–5)

Base URL: `http://localhost:8080`
All request and response bodies are JSON. Unknown JSON fields are rejected.

## Conventions

Errors share one shape:

```json
{ "error": "invalid_credentials", "message": "Invalid email or password." }
```

Validation failures add a `fields` array:

```json
{
  "error": "validation_failed",
  "message": "One or more fields are invalid.",
  "fields": [{ "field": "password", "message": "must be at least 12 characters" }]
}
```

| `error` code | Status | Meaning |
| --- | --- | --- |
| `validation_failed` | 400 | One or more fields are invalid |
| `invalid_json` | 400 | Body is not a single, well-formed JSON object |
| `unauthorized` | 401 | Missing or malformed `Authorization` header |
| `invalid_credentials` | 401 | Wrong email or password |
| `invalid_token` | 401 | Access or refresh token is invalid, expired or already used |
| `not_found` | 404 | No such resource, **or** it belongs to another user |
| `email_taken` | 409 | An account with that email exists |
| `dependency_cycle` | 409 | The task dependency would create a loop |
| `payload_too_large` | 413 | Body exceeds 64 KiB — or, on an upload, the file exceeds the size limit |
| `unsupported_media_type` | 415 | `Content-Type` is not `application/json`; or the uploaded file is not a PDF, TXT or Markdown |
| `invalid_multipart` | 400 | Upload body is not `multipart/form-data` with a `file` part |
| `embedding_unavailable` | 503 | The embedding service could not be reached, so a search cannot run |
| `model_unavailable` | 503 | The chat model or the embedding service could not be reached, so a turn cannot run |
| `rate_limited` | 429 | Too many requests from this IP (`Retry-After` header set) |
| `internal_error` | 500 | Unexpected failure (details are logged, never returned) |

### Token object

Returned by register, login and refresh:

```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIs...",
  "token_type": "Bearer",
  "expires_in": 899,
  "access_token_expires_at": "2026-09-09T05:55:55.644Z",
  "refresh_token": "_IdTzxlF-06JxiYGfCwwV7z0q54m6U01aaxhe-KPT7Q",
  "refresh_token_expires_at": "2026-10-09T05:40:55.644Z"
}
```

- **Access token** — HS256 JWT, 15 minutes, sent as `Authorization: Bearer <token>`.
- **Refresh token** — opaque 256-bit random string, 30 days, stored server-side
  only as a SHA-256 hash, and **rotated on every use**. The previous value stops
  working the moment a refresh succeeds.

---

## `POST /api/v1/auth/register`

Creates a user, their profile and a first session.

Request:

```json
{
  "email": "ada@example.com",
  "password": "correct horse battery staple",
  "name": "Ada",
  "timezone": "Europe/London"
}
```

`name` and `timezone` are optional (`timezone` defaults to `UTC`). Email is
lowercased and trimmed; password must be at least 12 characters.

`201 Created`:

```json
{
  "user": {
    "id": "bb568579-7668-4a1b-8771-a06b65792d1e",
    "email": "ada@example.com",
    "name": "Ada",
    "timezone": "Europe/London",
    "created_at": "2026-09-09T05:40:55.639Z"
  },
  "tokens": { "...": "token object" }
}
```

Errors: `400 validation_failed`, `409 email_taken`, `429 rate_limited`.

## `POST /api/v1/auth/login`

Request: `{ "email": "ada@example.com", "password": "correct horse battery staple" }`

`200 OK` — same body shape as register.

Errors: `400 validation_failed`, `401 invalid_credentials`, `429 rate_limited`.
An unknown email and a wrong password are indistinguishable, in both the
response and the time taken.

## `POST /api/v1/auth/refresh`

Request: `{ "refresh_token": "<token>" }`

`200 OK` — a bare token object (no `user`). The presented refresh token is
consumed and replaced.

Errors: `400 validation_failed`, `401 invalid_token` (unknown, expired, already
rotated, or belonging to a logged-out session).

## `POST /api/v1/auth/logout`

Request: `{ "refresh_token": "<token>" }`

`204 No Content`. Idempotent: logging out an already-dead session also returns
204. Only the session behind that token is invalidated; other devices stay
signed in. Access tokens already issued remain valid until they expire (up to
15 minutes) — see [decisions.md](decisions.md).

## `GET /api/v1/me`

Requires `Authorization: Bearer <access_token>`.

`200 OK`:

```json
{
  "id": "bb568579-7668-4a1b-8771-a06b65792d1e",
  "email": "ada@example.com",
  "name": "Ada",
  "timezone": "Europe/London",
  "created_at": "2026-09-09T05:40:55.639Z"
}
```

Errors: `401 unauthorized` (no/short header), `401 invalid_token` (bad or
expired token). Both set `WWW-Authenticate`.

## `GET /healthz`

`200 {"status":"ok","database":"ok"}` or `503 {"status":"degraded","database":"unreachable"}`.
Unauthenticated, not rate limited.

---

## Rate limiting

`POST /auth/register` and `POST /auth/login` are limited per client IP: burst of
10, refilling at 1 request per 5 seconds. Exceeding it returns `429` with
`Retry-After`. The limiter is in-process — see [decisions.md](decisions.md).

---

# Phase 2 — tasks, goals, notes

Every endpoint below requires `Authorization: Bearer <access_token>` and acts
only on the authenticated user's own rows.

**A resource belonging to another user answers `404 not_found`, never `403`.**
A 403 would confirm that the id exists; 404 says nothing at all. The same
applies to an id that is not a valid UUID, and to `parent_task_id` /
`depends_on_task_id` pointing at somebody else's task.

## Conventions for these endpoints

- **Lists** return `{ "<resource>": [...], "count": n, "limit": n, "offset": n }`.
  `count` is the size of this page, not the total. `limit` defaults to 50 and is
  clamped to 200; `offset` defaults to 0. Both are echoed back after clamping.
- **`sort`** takes a field name, optionally prefixed with `-` for descending.
  An unrecognised value is a `400`, not a silent fallback. Date sorts place
  rows with no date last in both directions.
- **PATCH is a true partial update.** A key you omit is left alone. A key set to
  `null` clears the column where it is nullable (`description`, `category`,
  `deadline`, `parent_task_id`, `target_date`), empties it where it is not
  (`tags` → `[]`, `content` → `""`), and is a `400` where the column is required
  (`title`, `priority`, `status`, `type`, `completed`).
- Free text is trimmed. `tags` are trimmed, blanks dropped and duplicates
  removed, preserving order.
- Timestamps are RFC 3339 in UTC, e.g. `2026-10-01T09:00:00.000Z`.

## Tasks

### `GET /api/v1/tasks`

Query parameters, all optional:

| Parameter | Values |
| --- | --- |
| `status` | `pending`, `in_progress`, `completed` |
| `category` | exact match |
| `tag` | matches a task carrying that tag |
| `sort` | `created_at`, `updated_at`, `deadline`, `title`, `priority`, each also with a `-` prefix (default `-created_at`) |
| `limit`, `offset` | paging (default 50, max 200) |

Filters combine with AND. `sort=priority` orders by rank (low → medium → high),
not alphabetically.

```
GET /api/v1/tasks?status=pending&tag=urgent&sort=-priority
```

```json
{
  "tasks": [ { "...": "task object" } ],
  "count": 1,
  "limit": 50,
  "offset": 0
}
```

### `POST /api/v1/tasks`

```json
{
  "title": "Ship phase 2",
  "description": "tasks, goals, notes",
  "priority": "high",
  "status": "pending",
  "category": "work",
  "tags": ["api", "go"],
  "deadline": "2026-10-01T09:00:00Z",
  "parent_task_id": null,
  "estimated_effort_minutes": 240,
  "actual_effort_minutes": null
}
```

Only `title` is required. `priority` defaults to `medium`, `status` to
`pending`, `tags` to `[]`. `parent_task_id` must be one of your own tasks.

`201 Created` — the task object:

```json
{
  "id": "d7bbc3c0-50b8-4d87-ae8a-fb33202b5cef",
  "title": "Ship phase 2",
  "description": null,
  "priority": "high",
  "status": "pending",
  "category": "work",
  "tags": ["api", "go"],
  "deadline": "2026-10-01T09:00:00.000Z",
  "parent_task_id": null,
  "estimated_effort_minutes": 240,
  "actual_effort_minutes": null,
  "depends_on": [],
  "created_at": "2026-09-09T06:52:18.538Z",
  "updated_at": "2026-09-09T06:52:18.538Z"
}
```

Errors: `400 validation_failed`, `404 not_found` (unknown or foreign
`parent_task_id`).

### `GET /api/v1/tasks/{id}`

`200 OK` — the task object, with `depends_on` populated.

### `PATCH /api/v1/tasks/{id}`

Any subset of the create fields. `{"status": "completed"}` changes the status
and nothing else; `{"deadline": null}` removes the deadline. A body of `{}` is a
no-op that returns the current task without touching `updated_at`.

`200 OK` — the updated task object.

### `DELETE /api/v1/tasks/{id}`

`204 No Content`. Subtasks and dependency edges go with it. Deleting an already
deleted task is `404`.

### `POST /api/v1/tasks/{id}/dependencies`

Records that this task waits on another.

```json
{ "depends_on_task_id": "0fb953a8-c0d3-4173-9130-2f22b92d5610" }
```

`201 Created` — the task object with the refreshed `depends_on` list. Adding an
edge that already exists is not an error; it is the state you asked for.

Errors:

- `400 validation_failed` — a task cannot depend on itself.
- `404 not_found` — either task is unknown or not yours.
- `409 dependency_cycle` — the edge would make the task transitively depend on
  itself. Both the direct case (`a → b`, then `b → a`) and the transitive one
  (`a → b → c`, then `c → a`) are caught.

## Goals

### `GET /api/v1/goals`

| Parameter | Values |
| --- | --- |
| `status` | `active`, `completed`, `abandoned` |
| `type` | `short_term`, `long_term`, `career`, `education`, `financial`, `personal`, `project` |
| `sort` | `created_at`, `updated_at`, `deadline`, `title`, each also with `-` (default `-created_at`) |
| `limit`, `offset` | paging (default 50, max 200) |

Each goal in the list carries its milestones; they are batch-loaded in one
query, so the page does not get slower as it gets longer.

### `POST /api/v1/goals`

```json
{
  "title": "Learn Postgres",
  "description": "properly",
  "type": "education",
  "status": "active",
  "deadline": "2026-12-31T00:00:00Z"
}
```

`title` and `type` are required — `type` has no sensible default, unlike
`status`, which defaults to `active`.

`201 Created`:

```json
{
  "id": "27b434df-3306-4d4d-952f-bc239cc55f2f",
  "title": "Learn Postgres",
  "description": "properly",
  "type": "education",
  "status": "active",
  "deadline": "2026-12-31T00:00:00.000Z",
  "milestones": [],
  "created_at": "2026-09-09T06:52:33.945Z",
  "updated_at": "2026-09-09T06:52:33.945Z"
}
```

### `GET`, `PATCH`, `DELETE /api/v1/goals/{id}`

As for tasks. Deleting a goal deletes its milestones.

### `POST /api/v1/goals/{id}/milestones`

```json
{ "title": "Finish the docs", "target_date": "2026-10-15T00:00:00Z" }
```

`201 Created` — the milestone object:

```json
{
  "id": "2f5d7980-b998-401e-a62d-019f80d038ae",
  "goal_id": "27b434df-3306-4d4d-952f-bc239cc55f2f",
  "title": "Finish the docs",
  "target_date": "2026-10-15T00:00:00.000Z",
  "completed": false,
  "created_at": "2026-09-09T06:52:56.623Z"
}
```

A goal holds at most 100 milestones; the 101st is a `400`.

Milestones are ordered by `target_date`, undated ones last, then by creation
time.

### `PATCH /api/v1/goals/{id}/milestones/{milestone_id}`

Accepts `title`, `target_date` and `completed`. `{"completed": true}` is the
common case.

`200 OK` — the milestone object. `404 not_found` if the milestone is unknown, is
not on that goal, or the goal is not yours.

## Notes

### `GET /api/v1/notes`

| Parameter | Values |
| --- | --- |
| `tag` | matches a note carrying that tag |
| `sort` | `created_at`, `updated_at`, `title`, each also with `-` (default `-created_at`) |
| `limit`, `offset` | paging (default 50, max 200) |

### `POST /api/v1/notes`

```json
{ "title": "Meeting", "content": "line one", "tags": ["work", "meeting"] }
```

Only `title` is required; `content` defaults to `""` and `tags` to `[]`.

`201 Created`:

```json
{
  "id": "4f7e8b49-d271-4ce2-9b67-0de2f05dc1c4",
  "title": "Meeting",
  "content": "line one",
  "tags": ["work", "meeting"],
  "created_at": "2026-09-09T06:52:56.886Z",
  "updated_at": "2026-09-09T06:52:56.886Z"
}
```

### `GET`, `PATCH`, `DELETE /api/v1/notes/{id}`

As for tasks. `{"tags": null}` empties the tag list rather than writing NULL.

---

## Documents

Upload a file, and the API extracts its text, splits it into overlapping
chunks, embeds each chunk and stores the vectors. `POST /documents/search` then
returns the chunks nearest to a query, with the filename and chunk index needed
to cite them.

Processing is **synchronous**: the upload response already carries the final
status. There is no job queue in this phase.

> **The original file is not kept.** Only the extracted text is stored, so
> re-downloading the PDF you uploaded is not possible until object storage
> arrives in a later phase. See [decisions.md](decisions.md).

### `POST /api/v1/documents`

`multipart/form-data` with one part named `file`.

```sh
curl -s -X POST localhost:8080/api/v1/documents -H "$AUTH" -F 'file=@notes.txt'
```

| Accepted | Extension | Notes |
| --- | --- | --- |
| PDF | `.pdf` | Text layer only — a scanned page has none, and OCR is a later phase |
| Plain text | `.txt`, `.text` | |
| Markdown | `.md`, `.markdown` | Stored and chunked as-is; structure is preserved |

DOCX, CSV and images are rejected with `415`. The extension is checked against
the file's actual bytes, so a `.pdf` that is not a PDF is refused up front
rather than stored as a document that failed.

`201 Created`:

```json
{
  "id": "b6d0a5c0-6e94-4d1a-9c2e-3b0b1a6f9a11",
  "filename": "notes.txt",
  "file_type": "txt",
  "status": "ready",
  "error_message": null,
  "chunk_count": 3,
  "text_length": 1042,
  "created_at": "2026-09-09T11:02:31.118Z",
  "updated_at": "2026-09-09T11:02:33.904Z"
}
```

`status` is one of `processing`, `ready` or `failed`. A document that fails to
process is still created, still has an id, and carries the reason:

```json
{
  "id": "…",
  "filename": "scan.pdf",
  "status": "failed",
  "error_message": "No text could be extracted. Scanned documents need OCR, which this version does not do.",
  "chunk_count": 0
}
```

That is deliberately a `201` and not a `4xx`: the row exists and the client
needs its id. Problems detected *before* any work happens — an empty file, an
unsupported type, an oversized upload — are ordinary errors and create nothing.

| Failure | Response |
| --- | --- |
| No `file` part, or not multipart | `400 invalid_multipart` |
| Empty file | `400 validation_failed` |
| Larger than 10 MB (`MAX_UPLOAD_BYTES`) | `413 payload_too_large` |
| Not a PDF/TXT/MD | `415 unsupported_media_type` |
| Unreadable PDF, no text, embedding outage | `201` with `status: "failed"` |

### `GET /api/v1/documents`

| Parameter | Values |
| --- | --- |
| `status` | `processing`, `ready`, `failed` |
| `sort` | `created_at`, `updated_at`, `filename`, each also with `-` (default `-created_at`) |
| `limit`, `offset` | paging (default 50, max 200) |

```json
{ "documents": [ … ], "count": 2, "limit": 50, "offset": 0 }
```

A list omits `extracted_text` — it is the largest column in the schema and a
page of fifty would be megabytes — and with it `text_length`, which is left out
rather than reported as zero. `chunk_count` is the field to read there.

### `GET /api/v1/documents/{id}`

The document object plus `extracted_text` and `text_length` (characters, not
bytes). With no object storage in this phase, this is the only way to read a
document's content back.

### `DELETE /api/v1/documents/{id}`

`204 No Content`. Chunks and their embeddings go with it.

### `POST /api/v1/documents/search`

The retrieval endpoint. Later phases call the same thing as a service function
(`documents.Service.Search`), not through HTTP.

```json
{ "query": "what happened over the tundra?", "limit": 5 }
```

| Field | Default | Notes |
| --- | --- | --- |
| `query` | — | Required, at most 4,000 characters |
| `limit` | 5 | Max 50 |
| `min_similarity` | 0 | Drops weak matches; cosine similarity runs −1 … 1 |
| `document_ids` | all | Restrict the search to these documents |

`200 OK`:

```json
{
  "query": "what happened over the tundra?",
  "count": 1,
  "results": [
    {
      "chunk_id": "3a1f…",
      "document_id": "b6d0a5c0-6e94-4d1a-9c2e-3b0b1a6f9a11",
      "filename": "field-notes.txt",
      "chunk_index": 0,
      "content": "The aurora borealis appeared over the tundra shortly after midnight…",
      "similarity": 0.71
    }
  ]
}
```

Results are ordered by cosine similarity, nearest first. Only `ready` documents
are searched, and only the caller's — a `document_ids` entry belonging to
somebody else simply matches nothing, exactly as a made-up id does.

`min_similarity` is worth setting. Vector search always returns the nearest
`limit` chunks however far away they are, so without a floor an unrelated
question still comes back with confident-looking citations.

If the embedding service is unreachable the search cannot run and answers
`503 embedding_unavailable`.

---

## Conversations

The AI assistant. Every endpoint is scoped to the caller: a conversation
belonging to somebody else answers `404`, exactly like one that does not exist.

### `POST /api/v1/conversations`

```json
{ "title": "Thesis planning" }
```

`title` is optional. Omitted, the conversation is called `New conversation`
until its first message names it — from the first line of that message, cut to
60 characters. A title the user chose is never overwritten.

`201 Created`:

```json
{
  "id": "fe69e704-57f3-4d18-b7be-b3d09cfcfd1b",
  "title": "New conversation",
  "message_count": 0,
  "created_at": "2026-09-09T18:19:07.221Z",
  "updated_at": "2026-09-09T18:19:07.221Z"
}
```

### `GET /api/v1/conversations`

| Parameter | Values |
| --- | --- |
| `sort` | `created_at`, `updated_at`, `title`, each also with `-` (default `-updated_at`) |
| `limit`, `offset` | paging (default 50, max 200) |

```json
{ "conversations": [ … ], "count": 2, "limit": 50, "offset": 0 }
```

The default order is most recently active first: `updated_at` moves every time
a turn is added, not only when the title changes.

### `GET /api/v1/conversations/{id}`

The conversation plus its messages, oldest first, each with the sources
recorded for it. A conversation longer than 500 messages returns its most
recent 500.

```json
{
  "id": "fe69e704-…",
  "title": "What do my field notes say happened over the tundra?",
  "message_count": 2,
  "messages": [
    { "id": "…", "role": "user", "content": "What do my field notes say happened over the tundra?", "sources": [], "created_at": "…" },
    {
      "id": "…",
      "role": "assistant",
      "content": "According to [S1], your field notes … the aurora borealis appeared over the tundra shortly after midnight.",
      "sources": [
        {
          "type": "document",
          "id": "4c3a26b4-b42f-4f76-8909-d610f20482ed",
          "label": "S1",
          "title": "field-notes.txt",
          "chunk_index": 0,
          "similarity": 0.7326,
          "excerpt": "Field notes, 14 March. The aurora borealis appeared…",
          "cited": true
        }
      ],
      "created_at": "…"
    }
  ],
  "created_at": "…",
  "updated_at": "…"
}
```

A **source** is one thing that was retrieved for that answer.

| Field | Meaning |
| --- | --- |
| `type` | `document`, `memory`, `task`, `goal` or `note` |
| `id` | the id of that record, so a client can link to it |
| `label` | the marker the prompt showed the model (`S1`, `S2`, …) |
| `title` | filename, the task/goal/note title, or — for a memory, which has none — its type |
| `chunk_index` | documents only |
| `similarity` | documents and memories: the cosine score retrieval used. Absent on tasks, goals and notes, which are selected by heuristic and have no score |
| `excerpt` | the text the model was actually shown; for a memory, the fact itself |
| `cited` | whether the answer referenced this label |

`cited` is measured by reading the generated answer for its labels, not
assumed. Retrieval usually offers five things and an answer uses one; `sources`
records all five and marks which. A user message carries `sources: []`.

### `DELETE /api/v1/conversations/{id}`

`204 No Content`. Its messages go with it.

### `POST /api/v1/conversations/{id}/messages`

Send a message and stream the answer.

```json
{ "content": "What do my field notes say happened over the tundra?" }
```

`content` is required, at most 8,000 characters.

**The response is Server-Sent Events** (`Content-Type: text/event-stream`), not
chunked plain text. The reason is the `sources` frame: an answer is not just
text, it also has a citation list that a client wants *before* the first token
so it can show what the reply is grounded in while it is still being written.
SSE gives every frame a name and a JSON body, so `sources`, `token`, `done` and
`error` are distinguishable without a length-prefix protocol invented for the
purpose. Raw chunked text would carry the tokens and nothing else.

Note that this is SSE framing over a normal `POST`, not an `EventSource`
resource: `EventSource` cannot send a body or an `Authorization` header, so
clients read the stream with `fetch` and a reader.

```
event: sources
data: {"sources":[{"type":"document","id":"…","label":"S1","title":"field-notes.txt","chunk_index":0,"similarity":0.7326,"excerpt":"…","cited":false}],"count":1}

event: token
data: {"text":"According"}

event: token
data: {"text":" to [S1],"}

event: done
data: {"conversation_id":"…","user_message":{…},"message":{…},"model":"llama3.2:3b","remembered":[]}
```

`done` is the last frame and it arrives late on a substantial turn: the answer
is fully streamed first, then the memory extractor reads the finished exchange,
and only then is `done` sent. The reply is complete and readable throughout —
what waits is the end-of-stream marker. `MEMORY_EXTRACT_TIMEOUT` bounds that
wait, and `MEMORY_EXTRACTION=false` removes it.

| Event | Payload | When |
| --- | --- | --- |
| `sources` | `{sources, count}` | Once, after the model accepts the request and before the first token. `cited` is always `false` here — nothing has been generated yet. |
| `token` | `{text}` | Per fragment, in order. Concatenating every `text` gives exactly the stored message content. |
| `done` | `{conversation_id, user_message, message, model, remembered}` | Once, after the turn is persisted **and** the memory extractor has run. `message.sources` is the same list with `cited` filled in; `remembered` is what the exchange added to memory, `[]` on most turns. |
| `error` | `{error, message}` | Instead of `done`, if generation failed after the stream started. |

Token text is JSON-encoded rather than written raw because a model emits
newlines mid-sentence and an SSE frame ends at a blank line.

Everything that can fail *before* the first token fails as an ordinary JSON
error with a real status code — the sink is not touched until retrieval has run
and the model has accepted the request:

| Failure | Response |
| --- | --- |
| No such conversation, or it is somebody else's | `404 not_found` |
| Empty or oversized `content` | `400 validation_failed` |
| Unknown JSON field | `400 invalid_json` |
| Model or embedding service unreachable | `503 model_unavailable` |
| Generation failed after the stream started | `200` with `event: error` |

A turn that did not finish is **not persisted at all** — not the question, not a
partial answer. Sending the message again is the retry. The `error` event says
so.

### What the assistant will not do

Two rules are in the system prompt and both are load-bearing.

**It does not claim an answer came from your data when it did not.** Only the
retrieved context is citable, by the exact `S1`-style labels it was given. If
nothing was retrieved the assistant says so rather than reaching for a
plausible filename; it may then answer from general knowledge, saying that is
what it is doing. Retrieval applies a similarity floor (`CHAT_MIN_SIMILARITY`,
default `0.5`), so an unrelated question is not handed a distant chunk to
resist in the first place.

**It cannot take actions.** There is no approval engine in this phase, so the
assistant only reads. Asked to create a task or complete a goal, it says that
taking actions is not supported yet and describes what you would do yourself.
No chat endpoint writes to `tasks`, `goals`, `notes` or `documents`.

### What grounds an answer

| Source | How it is selected |
| --- | --- |
| Document chunks | Phase 3's vector search over the caller's chunks: top 5 above `CHAT_MIN_SIMILARITY` |
| Memories | Phase 5's vector search over the caller's enabled memories: top 5 above `MEMORY_MIN_SIMILARITY` |
| Tasks | Up to 5: in progress by recency, then pending by nearest deadline |
| Goals | Up to 5 active goals by nearest deadline |
| Notes | The 3 most recently updated |

Documents and memories are the semantic half, each with its own floor. Tasks,
goals and notes are selected by that heuristic rather than semantically — they
have no embeddings yet — so they reach the model as background that the
question may or may not be about.

Sources are labelled in that order, so `S1` is the strongest document match
whenever any document matched, and the ones past the context budget are dropped
from the end.

The previous 20 messages of the conversation are replayed as history. Past that
a conversation forgets its own wording — but not what it was about: the durable
facts in those turns were extracted into memory and come back through
retrieval.

---

## Memories

What the assistant has learned about you. Every endpoint is scoped to the
caller: a memory belonging to somebody else answers `404`, exactly like one that
does not exist.

Memories are written by the assistant, not by clients — there is no `POST`. They
are created by the extraction that runs at the end of a substantial chat turn;
see **How a memory is made** below.

A memory:

```json
{
  "id": "3f7c1d2e-8a44-4b91-9c02-0a5e6d3b7f10",
  "type": "preference",
  "content": "The user prefers studying in the early morning before class.",
  "importance": 0.7,
  "confidence": 0.9,
  "source_conversation_id": "150b47f9-3572-4e85-8f23-ea77d698d2c5",
  "enabled": true,
  "expires_at": null,
  "created_at": "2026-09-10T09:12:44.108Z",
  "updated_at": "2026-09-10T09:12:44.108Z"
}
```

| Field | Meaning |
| --- | --- |
| `type` | `episodic` (something that happened), `semantic` (a standing fact), `preference` (how you like to work), `project` (tied to a piece of work), `goal` (tied to something you are trying to achieve) |
| `content` | the fact, one sentence, written in the third person |
| `importance` | 0–1, how much the model thought this was worth keeping |
| `confidence` | 0–1, how sure it was that it read the fact rather than inferred it |
| `source_conversation_id` | which conversation taught it, or `null` once that conversation has been deleted — the fact outlives its source |
| `enabled` | `false` means it is kept but never retrieved |
| `expires_at` | when the memory stops being retrievable. Always `null` in this phase: nothing writes it yet, and retrieval already honours it |

`importance` and `confidence` are the model's own judgement, recorded rather
than trusted — retrieval filters on similarity, not on them. They are rounded to
two decimal places on the wire, because the column is single-precision and a
score given to one decimal place should not be reported to fifteen.

### `GET /api/v1/memories`

| Parameter | Values |
| --- | --- |
| `type` | one of the five types |
| `enabled` | `true` or `false`; omitted lists both |
| `sort` | `created_at`, `updated_at`, `importance`, `confidence`, each also with `-` (default `-created_at`) |
| `limit`, `offset` | paging (default 50, max 200) |

```json
{ "memories": [ … ], "count": 12, "limit": 50, "offset": 0 }
```

### `PATCH /api/v1/memories/{id}`

```json
{ "content": "The user prefers revising late at night.", "enabled": false }
```

Both keys are optional; an omitted one leaves the column alone. `null` is
rejected for either — a memory with no text is not a memory, and one that is
neither on nor off does not exist.

`type`, `importance` and `confidence` are not editable. They record what the
extraction said, and rewriting them would make them mean nothing.

Changing `content` **re-embeds the memory**, so it becomes retrievable by what
it now says and stops being retrievable by what it used to. That is the one
thing on this endpoint that needs the embedding service: if it is unreachable
the edit answers `503 embedding_unavailable` and nothing is changed. Toggling
`enabled` needs no model at all.

`200 OK` with the updated memory. Errors: `400 validation_failed`,
`404 not_found`, `503 embedding_unavailable`.

### `DELETE /api/v1/memories/{id}`

`204 No Content`. Gone for good — there is no archive in this phase. To stop the
assistant using a memory without losing it, `PATCH` it to `enabled: false`.

### `DELETE /api/v1/memories`

Forget everything.

```json
{ "confirm": true }
```

The flag is required and must be `true`; anything else is
`400 validation_failed` naming `confirm`, so an accidental `DELETE` on the
collection cannot be mistaken for an instruction. A request with no body at all
is `400 invalid_json` — this endpoint takes `Content-Type: application/json`
like every other write.

`200 OK` — with a count, not `204`, because this is a request nobody makes twice
and "there was nothing to forget" and "four hundred facts are gone" should not
look identical:

```json
{ "deleted": 12 }
```

### How a memory is made

At the end of a chat turn, if the question and the answer together come to at
least 120 characters, the exchange is sent back to the model with an extraction
prompt asking for 0–3 durable facts about you. Each returned fact is embedded
and stored with the conversation it came from.

Four things are dropped before anything is written:

- facts the model scored below 0.4 on **confidence** — it is unsure it read them;
- facts it scored below 0.4 on **importance** — it says they are not worth
  keeping, and it is right: "the user asked about their field notes" is what
  that looks like;
- sentences that are not about you, checked by requiring the word "user" — the
  model sometimes returns a line it copied out of your own documents;
- facts you already have, measured as a cosine similarity of 0.95 or more
  against an existing memory, so restating a preference does not store it twice.

Extraction never affects the answer. It runs after the turn is persisted, and if
it fails — the model is down, the reply is unparseable, the deadline passes —
the failure is logged and the answer is returned unchanged. Nothing about a
memory can turn a good answer into an error.

Retrieved memories reach the model as ordinary citable sources, with one extra
rule in the system prompt: a memory is something recorded in an earlier
conversation and may be out of date, so if it disagrees with what you say now,
what you say now wins.

---

## Field limits

| Field | Limit |
| --- | --- |
| `title` | 1–500 characters, after trimming |
| `description` | 10,000 characters |
| `content` (notes) | 40,000 characters |
| `category` | 100 characters |
| `tags` | 25 tags, 50 characters each |
| `estimated_effort_minutes`, `actual_effort_minutes` | 0 – 525,600 (a year) |
| request body | 64 KiB |
| uploaded file | 10 MB (`MAX_UPLOAD_BYTES`) |
| filename | 255 characters |
| search `query` | 4,000 characters |
| chat message `content` | 8,000 characters |
| memory `content` | 1,000 characters |
| chunks per document | 800 (~1.5 MB of prose); a larger document fails to process |

Lengths count characters (runes), not bytes.
