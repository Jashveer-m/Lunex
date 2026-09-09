# Testing

## Layers

| Layer | Location | Needs a database |
| --- | --- | --- |
| Argon2id, JWT, refresh tokens, validation, rate limiter | `internal/auth/*_test.go` | no |
| Auth service use cases (in-memory fakes) | `internal/auth/service_test.go` | no |
| HTTP handlers and middleware | `internal/auth/handlers_test.go` | no |
| Full route tree over the real service | `internal/api/router_test.go` | no |
| Config loading | `internal/config/config_test.go` | no |
| Migrations, constraints, cascades, atomic rotation | `internal/db/integration_test.go` | **yes** |

## Running

```sh
# Everything that does not need Postgres:
cd backend && go test ./...

# Including the database-backed tests:
createdb lifeos_test
cd backend && TEST_DATABASE_URL='postgres://postgres@localhost:5432/lifeos_test?sslmode=disable' \
  go test ./... -count=1
```

Without `TEST_DATABASE_URL` the `internal/db` tests skip rather than fail, so
`go test ./...` is always green on a machine with no Postgres.

The integration tests migrate up and `TRUNCATE users CASCADE` before each test,
so point `TEST_DATABASE_URL` at a throwaway database — **never** at one holding
data you care about.

## What the tests assert

Beyond the happy paths, the suite pins the security-relevant behaviour:

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
