# LifeOS

Personal life-operating-system. **Phase 1** is the foundation only: repository
structure, database schema and authentication. Tasks, goals, notes, documents
and anything AI-shaped belong to later phases and are deliberately absent.

## Stack

| Piece | Choice |
| --- | --- |
| API | Go 1.26, [chi](https://github.com/go-chi/chi) router |
| Database | PostgreSQL 16, migrations via golang-migrate (embedded) |
| Passwords | Argon2id (`golang.org/x/crypto/argon2`) |
| Tokens | HS256 access JWT (15 min) + rotating opaque refresh token (30 days) |
| Frontend | Vite + React 19 + TypeScript + Tailwind v4 (scaffold only) |

## Layout

```
lifeos/
├── backend/
│   ├── cmd/api/           # HTTP server
│   ├── cmd/migrate/       # up / down / version
│   ├── internal/
│   │   ├── api/           # router and middleware wiring
│   │   ├── auth/          # argon2id, JWT, sessions, service, handlers
│   │   ├── config/        # environment configuration
│   │   ├── db/            # connection pool + migration runner
│   │   └── users/         # user + profile model and repository
│   ├── migrations/        # embedded .sql migrations
│   └── go.mod
├── frontend/              # Vite React TS scaffold
├── docs/                  # api.md, decisions.md, testing.md
└── Makefile
```

## Quick start

Requires Go 1.26+, PostgreSQL 16+ and Node 20+.

```sh
# 1. Database
createdb lifeos

# 2. Backend config
cd backend
cp .env.example .env          # then edit it
export DATABASE_URL='postgres://postgres@localhost:5432/lifeos?sslmode=disable'
export JWT_SECRET="$(openssl rand -base64 48)"

# 3. Migrate and run
go run ./cmd/migrate up
go run ./cmd/api              # listens on :8080

# 4. Frontend (optional; proxies /api to :8080)
cd ../frontend && npm install && npm run dev
```

Check it is alive:

```sh
curl -s localhost:8080/healthz
# {"database":"ok","status":"ok"}
```

Register and call an authenticated endpoint:

```sh
TOKENS=$(curl -s -X POST localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct horse battery staple","name":"Ada"}')

ACCESS=$(echo "$TOKENS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["tokens"]["access_token"])')
curl -s localhost:8080/api/v1/me -H "Authorization: Bearer $ACCESS"
```

## Configuration

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `DATABASE_URL` | yes | — | Postgres connection string |
| `JWT_SECRET` | yes | — | ≥ 32 bytes; the process refuses to start otherwise |
| `PORT` | no | `8080` | |
| `JWT_ISSUER` | no | `lifeos` | Validated on every access token |
| `ACCESS_TOKEN_TTL` | no | `15m` | Go duration |
| `REFRESH_TOKEN_TTL` | no | `720h` | 30 days |
| `LOGIN_RATE_LIMIT_BURST` | no | `10` | Per-IP burst on login/register |
| `AUTO_MIGRATE` | no | `false` | `true` migrates on boot instead of via `cmd/migrate` |

## Common commands

```sh
make build             # go build ./...
make test              # unit + handler tests, no database needed
make test-integration  # adds the Postgres-backed tests
make migrate-up        # apply migrations
make migrate-version   # print schema version
make run               # start the API
make frontend-dev      # start Vite
```

## Documentation

- [docs/api.md](docs/api.md) — endpoint-by-endpoint API contract
- [docs/decisions.md](docs/decisions.md) — why chi, why SHA-256 for refresh
  tokens, and **what Phase 1 explicitly defers**
- [docs/testing.md](docs/testing.md) — test layers and how to run them

## Known Phase 1 gaps

Summarised here, detailed in [docs/decisions.md](docs/decisions.md):

1. **Rate limiting is in-process.** A real per-IP token bucket protects
   login/register, but it resets on restart and is per-replica. Redis is out of
   scope for this phase.
2. **No auth UI.** The frontend is a scaffold with a health panel; no login
   form, no token storage.
3. **No email verification or password reset.**
4. **Refresh tokens are returned in the JSON body**, not as an `HttpOnly` cookie.
5. **No CORS middleware** — development relies on the Vite proxy.
6. **Logout does not revoke outstanding access tokens**; they expire within 15
   minutes.
