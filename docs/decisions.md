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

## 2. No auth UI

`frontend/` is a Vite + React + TypeScript + Tailwind scaffold that renders a
health panel and nothing else. There is no login form, no token storage, no
routing, and `src/lib/api.ts` only wires `/healthz`. The brief made the UI
optional for this phase; the auth screens belong with the token-storage decision
(memory + refresh cookie vs. localStorage), which is a Phase 2 call.

## 3. No email verification or password reset

`users` has no `email_verified_at` column and nothing sends mail. Registration
trusts the address as given.

## 4. Refresh tokens are returned in the JSON body

Not as an `HttpOnly; Secure; SameSite` cookie. That is the right shape for the
API-client and mobile-free scope of Phase 1, but a browser SPA storing a refresh
token in JavaScript-reachable storage is XSS-exposed. Revisit with the auth UI.

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
