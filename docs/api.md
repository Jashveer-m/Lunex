# LifeOS API — v1 (Phase 1)

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
| `email_taken` | 409 | An account with that email exists |
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
