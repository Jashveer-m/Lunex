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
| Config loading | `internal/config/config_test.go` | no |
| Migrations, constraints, cascades, atomic rotation | `internal/db/integration_test.go` | **yes** |
| Phase 2 SQL: filters, sorting, partial updates, dependency cycles | `internal/db/phase2_integration_test.go` | **yes** |
| Cross-user isolation over the whole stack | `internal/api/isolation_test.go` | **yes** |

## Running

```sh
# Everything that does not need Postgres:
cd backend && go test ./...

# Including the database-backed tests:
createdb lunex_test
cd backend && TEST_DATABASE_URL='postgres://postgres@localhost:5432/lunex_test?sslmode=disable' \
  go test ./... -count=1
```

Or `make test` and `make test-integration` from the repository root.

Without `TEST_DATABASE_URL` the database-backed tests skip rather than fail, so
`go test ./...` is always green on a machine with no Postgres.

The integration tests migrate up and `TRUNCATE users CASCADE` before each test,
so point `TEST_DATABASE_URL` at a throwaway database — **never** at one holding
data you care about. One truncate is enough: tasks, goals, milestones and notes
all cascade from `users`.

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
