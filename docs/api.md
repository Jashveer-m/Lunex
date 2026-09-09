# Lunex API — v1 (Phases 1–2)

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
| `payload_too_large` | 413 | Body exceeds 64 KiB |
| `unsupported_media_type` | 415 | `Content-Type` is not `application/json` |
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

Lengths count characters (runes), not bytes.
