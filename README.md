# Lunex

Personal life-operating-system.

- **Phase 1** — repository structure, database schema, authentication.
- **Phase 2** — tasks, goals and notes: plain CRUD, authenticated, scoped per
  user.
- **Phase 3** — documents and retrieval: upload a PDF or Markdown/text file,
  extract its text, chunk it, embed it with a local Ollama, store the vectors in
  pgvector, and search them with citations.

AI chat, agents, memory and the knowledge graph belong to later phases and are
deliberately absent. Phase 3 builds the retrieval function those phases will
call, not a chat UI.

## Stack

| Piece | Choice |
| --- | --- |
| API | Go 1.26, [chi](https://github.com/go-chi/chi) router |
| Database | PostgreSQL 16 + [pgvector](https://github.com/pgvector/pgvector), migrations via golang-migrate (embedded) |
| Embeddings | [Ollama](https://ollama.com) running `nomic-embed-text` (768 dimensions) locally |
| PDF text | [ledongthuc/pdf](https://github.com/ledongthuc/pdf) — text layer only |
| Passwords | Argon2id (`golang.org/x/crypto/argon2`) |
| Tokens | HS256 access JWT (15 min) + rotating opaque refresh token (30 days) |
| Frontend | Vite + React 19 + TypeScript + Tailwind v4 (scaffold only) |

## Layout

```
lunex/
├── backend/
│   ├── cmd/api/           # HTTP server
│   ├── cmd/migrate/       # up / down / version
│   ├── internal/
│   │   ├── api/           # router and middleware wiring
│   │   ├── auth/          # argon2id, JWT, sessions, service, handlers
│   │   ├── config/        # environment configuration
│   │   ├── db/            # connection pool, migration runner, pgvector param
│   │   ├── documents/     # upload, extract, chunk, embed, search
│   │   ├── embeddings/    # Ollama client behind an Embedder interface
│   │   ├── goals/         # goals + milestones: model, service, handlers
│   │   ├── httpx/         # JSON transport helpers shared by the modules
│   │   ├── notes/         # notes: model, service, handlers
│   │   ├── optional/      # the three-state field a PATCH body needs
│   │   ├── tasks/         # tasks + dependencies: model, service, handlers
│   │   ├── users/         # user + profile model and repository
│   │   └── validate/      # field rules shared by the modules
│   ├── migrations/        # embedded .sql migrations
│   └── go.mod
├── frontend/              # Vite React TS scaffold
├── docs/                  # api.md, decisions.md, testing.md
├── scripts/e2e.sh         # upload -> search, against real Postgres and Ollama
└── Makefile
```

## Quick start

Requires Go 1.26+, PostgreSQL 16+ with pgvector, Ollama, and Node 20+.

```sh
# 0. Embeddings and the vector extension (Phase 3)
brew install pgvector          # or: apt install postgresql-16-pgvector
brew install ollama && ollama serve &
ollama pull nomic-embed-text

# 1. Database
createdb lunex

# 2. Backend config
cd backend
cp .env.example .env          # then edit it
export DATABASE_URL='postgres://postgres@localhost:5432/lunex?sslmode=disable'
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
AUTH="Authorization: Bearer $ACCESS"

curl -s localhost:8080/api/v1/me -H "$AUTH"
```

Create and list a task:

```sh
curl -s -X POST localhost:8080/api/v1/tasks -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Ship phase 2","priority":"high","tags":["api"]}'

curl -s 'localhost:8080/api/v1/tasks?status=pending&sort=-priority' -H "$AUTH"
```

Upload a document and ask it a question:

```sh
printf 'The aurora borealis appeared over the tundra shortly after midnight.\n' > notes.txt

curl -s -X POST localhost:8080/api/v1/documents -H "$AUTH" -F 'file=@notes.txt'
# {"id":"…","filename":"notes.txt","file_type":"txt","status":"ready","chunk_count":1,…}

curl -s -X POST localhost:8080/api/v1/documents/search -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"query":"what happened over the tundra?","limit":3}'
# {"count":1,"results":[{"filename":"notes.txt","chunk_index":0,"similarity":0.73,"content":"The aurora…"}]}
```

## Configuration

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `DATABASE_URL` | yes | — | Postgres connection string |
| `JWT_SECRET` | yes | — | ≥ 32 bytes; the process refuses to start otherwise |
| `PORT` | no | `8080` | |
| `JWT_ISSUER` | no | `lifeos` | Validated on every access token; unchanged from Phase 1 so existing tokens keep verifying |
| `ACCESS_TOKEN_TTL` | no | `15m` | Go duration |
| `REFRESH_TOKEN_TTL` | no | `720h` | 30 days |
| `LOGIN_RATE_LIMIT_BURST` | no | `10` | Per-IP burst on login/register |
| `AUTO_MIGRATE` | no | `false` | `true` migrates on boot instead of via `cmd/migrate` |
| `OLLAMA_BASE_URL` | no | `http://localhost:11434` | Where `ollama serve` is listening |
| `EMBEDDING_MODEL` | no | `nomic-embed-text` | |
| `EMBEDDING_DIMENSIONS` | no | `768` | Must match the `vector(n)` column in migration 000003 |
| `MAX_UPLOAD_BYTES` | no | `10485760` | 10 MB; rejected before the file is read |
| `DOCUMENT_PROCESS_TIMEOUT` | no | `2m` | Budget for one synchronous upload; also sets the server's read/write timeout |

## Common commands

```sh
make build             # go build ./...
make test              # unit + handler tests, no database and no Ollama needed
make test-integration  # adds the Postgres-backed tests (needs pgvector)
make test-e2e          # upload -> search against a real Ollama
make migrate-up        # apply migrations
make migrate-version   # print schema version
make run               # start the API
make frontend-dev      # start Vite
```

## Documentation

- [docs/api.md](docs/api.md) — endpoint-by-endpoint API contract
- [docs/decisions.md](docs/decisions.md) — why chi, why SHA-256 for refresh
  tokens, why a foreign resource is a 404, and **what is explicitly deferred**
- [docs/testing.md](docs/testing.md) — test layers and how to run them

## Known gaps

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
7. **No recurring tasks.** The schema leaves room for a `recurrence_rule`
   column; none of the logic exists yet.
8. **List paging is `limit`/`offset`.** Fine at this size, but a client paging
   deeply while rows are being inserted can see a row twice. Keyset paging is
   the fix when it starts to matter.
9. **The Go module path is still `github.com/jashveer/lifeos/backend`**, because
   it mirrors the repository URL. Renaming it is a repository rename.
10. **No object storage — the uploaded file itself is not kept.** Only the
    extracted text is stored, so there is no way to re-download the PDF you
    uploaded, show it in a viewer, or re-extract it later with a better parser
    or with OCR. MinIO is a later phase; the files uploaded before it exists are
    not recoverable. This is the largest gap in Phase 3.
11. **PDF and TXT/Markdown only.** DOCX, CSV and images (OCR) are rejected with
    `415`. A scanned PDF is accepted, extracts nothing, and fails with a message
    saying OCR is what it needs.
12. **No background job queue.** Uploads process inside the request, which is
    why `/documents` has its own longer timeout and why a document is capped at
    800 chunks. A document that failed because Ollama was down has to be deleted
    and re-uploaded; there is no retry.
13. **No reranking and no hybrid search.** Retrieval is plain vector similarity;
    `min_similarity` is the only defence against confident-looking irrelevant
    citations, and it is off by default.
14. **Embeddings are tied to nomic-embed-text.** Vectors from two models are not
    comparable, so changing the model means a migration that changes the column
    and re-embeds every chunk.
