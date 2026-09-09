# LifeOS — Phase 1 Brief (for Claude Code)

## Goal
Scaffold the repo and build Phase 1 only: repo structure, DB schema, auth. Do not build tasks, goals, notes, documents, AI, or mobile yet — those are later phases.

## Stack
- Backend: Go (net/http or chi router, your choice — explain which and why)
- DB: PostgreSQL, migrations via a proper migration tool (e.g. golang-migrate or goose)
- Frontend: React + TypeScript + Tailwind (scaffold only, no auth UI yet unless time permits)
- Password hashing: Argon2id
- No Docker dependency required for correctness — local Postgres is fine

## Repo structure
```
lifeos/
├── backend/
│   ├── cmd/api/main.go
│   ├── internal/
│   │   ├── auth/        (handlers, service, argon2id, JWT/refresh logic)
│   │   ├── users/        (user model, repository)
│   │   └── db/           (connection, migration runner)
│   ├── migrations/
│   └── go.mod
├── frontend/              (React + TS + Tailwind, vite scaffold)
├── docs/
└── README.md
```

## Database schema (Phase 1 only)
- `users`: id (uuid pk), email (unique, not null), password_hash (not null), created_at, updated_at
- `sessions`: id (uuid pk), user_id (fk → users), refresh_token_hash (not null), expires_at, device_info (text, nullable), created_at
- `user_profiles`: user_id (fk → users, pk), name, timezone, created_at

Use proper foreign keys, indexes on users.email and sessions.user_id, and timestamps on every table.

## Auth requirements
- Argon2id for password hashing (never plaintext, never bcrypt/md5)
- Access token: short-lived JWT (15 min)
- Refresh token: long-lived, stored HASHED in `sessions` table, rotated on use
- Endpoints:
  - `POST /api/v1/auth/register` (email, password) → create user + profile
  - `POST /api/v1/auth/login` (email, password) → access + refresh token
  - `POST /api/v1/auth/logout` → invalidate session
  - `POST /api/v1/auth/refresh` → rotate refresh token, issue new access token
  - `GET /api/v1/me` → current user (requires valid access token)
- Input validation on all fields (email format, password min length)
- Rate limiting on login/register can be stubbed (TODO comment) if Redis isn't wired yet — do not skip silently, flag it explicitly in the response

## What NOT to do
- Do not implement tasks, goals, notes, documents, RAG, memory, agents, or any AI features
- Do not add mobile (Kotlin) — this project has no mobile app
- Do not introduce Redis, S3/MinIO, or Python service yet — Phase 1 is backend + DB + auth only
- Do not silently skip a requirement above — if something is deferred, say so explicitly

## Response format for this phase
1. Architecture explanation (brief)
2. Files created/modified
3. Migration SQL
4. API contract
5. Implementation
6. Tests (unit tests for auth service, at minimum)
7. Exact verification commands to run (build, migrate, run server, curl the endpoints)
