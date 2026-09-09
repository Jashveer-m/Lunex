# Lunex — Phase 2 Brief (for Claude Code)

Note: this project's name is Lunex, not LifeOS. Rename any lingering "lifeos" references you find in docs/comments/module names as you touch them — don't do a blanket rename pass unless it's cheap.

## Goal
Build Phase 2 only: tasks, goals, notes — plain CRUD, authenticated, scoped per-user. No AI, no documents, no RAG, no memory yet.

## What exists already (Phase 1)
- Go backend (chi router), Postgres, golang-migrate, Argon2id auth, JWT access + rotated refresh tokens, `RequireAuth` middleware
- `users`, `user_profiles`, `sessions` tables
- Layering: transport → service → repository, per `internal/auth` as the reference pattern

Follow the same layering and testing pattern established in `internal/auth` for these new modules.

## Database schema (Phase 2 only)

```sql
CREATE TABLE tasks (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title             text NOT NULL,
    description       text,
    priority          text NOT NULL DEFAULT 'medium',  -- low/medium/high
    status            text NOT NULL DEFAULT 'pending',  -- pending/in_progress/completed
    category          text,
    tags              text[] NOT NULL DEFAULT '{}',
    deadline          timestamptz,
    parent_task_id    uuid REFERENCES tasks(id) ON DELETE CASCADE,
    estimated_effort_minutes int,
    actual_effort_minutes    int,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_dependencies (
    task_id           uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    depends_on_task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, depends_on_task_id),
    CHECK (task_id != depends_on_task_id)
);

CREATE TABLE goals (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       text NOT NULL,
    description text,
    type        text NOT NULL,  -- short_term/long_term/career/education/financial/personal/project
    status      text NOT NULL DEFAULT 'active',  -- active/completed/abandoned
    deadline    timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE goal_milestones (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    goal_id     uuid NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
    title       text NOT NULL,
    target_date timestamptz,
    completed   boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title      text NOT NULL,
    content    text NOT NULL DEFAULT '',
    tags       text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

Add appropriate indexes (user_id on all three, deadline on tasks/goals for querying upcoming items). Use the `set_updated_at` trigger already defined in migration 000001 — don't redefine it, reuse it.

## API contract

All routes below sit under `RequireAuth` and are scoped to the authenticated user's own rows — a user must never be able to read, modify, or delete another user's task/goal/note. Write a test that specifically confirms this (user A cannot GET/PATCH/DELETE user B's resource — expect 404, not 403, to avoid leaking existence).

- `GET /api/v1/tasks` (support query params: status, category, tag, sort)
- `POST /api/v1/tasks`
- `GET /api/v1/tasks/:id`
- `PATCH /api/v1/tasks/:id`
- `DELETE /api/v1/tasks/:id`
- `POST /api/v1/tasks/:id/dependencies` (add a dependency)
- `GET /api/v1/goals`, `POST /api/v1/goals`, `GET/PATCH/DELETE /api/v1/goals/:id`
- `POST /api/v1/goals/:id/milestones`, `PATCH /api/v1/goals/:id/milestones/:milestone_id`
- `GET /api/v1/notes`, `POST /api/v1/notes`, `GET/PATCH/DELETE /api/v1/notes/:id`

Same error shape as Phase 1 (`{"error": "...", "message": "...", "fields": [...]}`). Same strict JSON decoding (reject unknown fields, size cap).

## What NOT to do
- No AI, embeddings, documents, RAG, or memory
- No recurring-task logic yet (schema allows a future `recurrence_rule` column, but don't build the logic now)
- No Redis, S3, Python service
- Don't touch `internal/auth` except to import what's needed

## Response format
Same as Phase 1: architecture, files, migration SQL, API contract, implementation, tests (including the cross-user isolation test), verification commands.
