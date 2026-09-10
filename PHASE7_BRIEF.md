# Lunex — Phase 7 Brief (for Claude Code)

## Goal
Build a tool registry, a small set of real tools backed by existing modules, an agent router that picks tools for a user's request, and an action engine that distinguishes read (auto-execute) from write (requires user approval) actions. This is the first phase where the assistant can actually change data, not just answer questions.

## What exists already (Phases 1–6)
- `internal/tasks`, `internal/goals`, `internal/notes`, `internal/documents` — CRUD services, all per-user isolated
- `internal/chat` — orchestrator, `Deps` pattern for pluggable searchers (documents/memories/graph)
- `internal/ai` — `Provider` interface (`Ollama`/`Mock`)
- `internal/memories`, `internal/graph` — extraction pipeline pattern to reference for structured LLM output (lenient parsing, not strict JSON mode — Phase 6 found strict JSON mode makes llama3.2:3b worse)

## Tool registry

`internal/tools` — each tool declares name, description, input schema, output schema, permission level (`read`/`write`), and a Go function that calls into the relevant existing service (tasks/goals/notes/documents). No arbitrary code execution, no raw SQL from the LLM — every tool is a fixed, reviewed function.

Tools this phase (read):
- `search_tasks`, `search_goals`, `search_notes`, `search_documents`

Tools this phase (write):
- `create_task`, `update_task`, `create_goal`, `create_note`

Do not implement delete tools this phase (deletion via chat is a sharper edge — defer it, note it explicitly as deferred).

## Action engine

```sql
CREATE TABLE actions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    tool_name    text NOT NULL,
    input        jsonb NOT NULL,
    permission_level text NOT NULL,  -- read/write
    status       text NOT NULL DEFAULT 'proposed',  -- proposed/approved/rejected/executed/failed
    result       jsonb,
    error_message text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
```

Flow:
- **Read tools execute immediately** — no approval needed, result flows straight into the assistant's context (same as documents/memories/graph retrieval).
- **Write tools are proposed, not executed.** The orchestrator determines a write tool call is needed, stores it as an `actions` row with `status='proposed'`, and tells the user what it wants to do — it does not call the tool yet. The chat response should clearly present the proposed action for the user to approve or reject.
- User approves or rejects via a separate endpoint; only then does the write tool actually run.

Index on `user_id`, `status`. Reuse `set_updated_at`.

## Agent routing

Keep this simple this phase: one general orchestrator (extending Phase 4's) that decides, per user message, whether a tool is needed and which one — not a full multi-agent router with named personas (Study Agent, Career Agent, etc.) yet, since those need modules that don't exist. Structure the code so a named-agent layer can be added later without rework (e.g. a `TaskAgent` type wrapping the task tools, even if there's only one agent for now) — explain your structure choice.

Decide and justify: how does the model choose a tool — native Ollama tool-calling support, or a structured-output pattern like Phase 5/6's extraction (given the model-quality issues already found there)? State which you picked and why.

## API contract

All under `RequireAuth`, scoped to caller.

- Existing `POST /api/v1/conversations/:id/messages` — now may return a proposed action alongside the text response (add an `event: action` SSE frame, or include it in `done` — your call)
- `GET /api/v1/actions` (filter by status)
- `GET /api/v1/actions/:id`
- `POST /api/v1/actions/:id/approve` — executes the tool, updates status to `executed` or `failed`
- `POST /api/v1/actions/:id/reject` — updates status to `rejected`, does not execute

Same error shape, strict decoding, cross-user isolation pattern as prior phases.

## What NOT to do
- No delete tools via chat this phase
- No named specialist agents (Study/Career/Finance/Travel/Research) — those need modules that don't exist yet; build the general orchestrator with tools only
- No sensitive-action tier beyond read/write — nothing here (financial transactions, account changes) needs it yet
- No autonomous execution of write actions without explicit user approval, ever — this is the one rule that cannot be relaxed even by request

## Response format
Same as prior phases: architecture, files, migration SQL, API contract, implementation, tests (unit with MockProvider, cross-user isolation, a test proving a write tool never executes without an approved action row), verification commands. In verification, include a real end-to-end check: ask the assistant to create a task, confirm it's proposed not created, approve it, confirm the task now exists; then ask it to create another task and reject it, confirm no task was created.
